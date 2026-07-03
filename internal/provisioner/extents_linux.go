//go:build linux

package provisioner

import (
	"fmt"
	"os"
	"sort"
	"unsafe"

	"golang.org/x/sys/unix"
)

// FIEMAP plumbing for rootfs diff uploads. A hot-created sandbox's rootfs is a
// reflink CoW clone of the golden snapshot's rootfs: unmodified regions share
// physical extents with the base, and only written regions get new ones. So
// "which bytes changed" is answerable from extent metadata alone — compare the
// two files' logical→physical maps and keep every clone range whose physical
// backing diverged (or that the base doesn't map at all). Conservative by
// construction: a false positive costs upload bytes, never correctness; a
// shared extent with different content is impossible (sharing is what reflink
// means).

const (
	// Linux lseek whence values for sparse-file navigation (unistd.h).
	seekDataWhence = 3 // SEEK_DATA
	seekHoleWhence = 4 // SEEK_HOLE

	fsIocFiemap = 0xc020660b // _IOWR('f', 11, struct fiemap)

	fiemapFlagSync = 0x1 // sync the file before mapping

	fiemapExtentLast     = 0x0001 // last extent in the file
	fiemapExtentUnknown  = 0x0002 // location unknown — treat as changed
	fiemapExtentDelalloc = 0x0004 // not yet allocated — treat as changed
	fiemapExtentEncoded  = 0x0008 // physical offset not directly comparable

	fiemapBatch = 512 // extents fetched per ioctl
)

type fiemapExtent struct {
	Logical    uint64
	Physical   uint64
	Length     uint64
	Reserved64 [2]uint64
	Flags      uint32
	Reserved   [3]uint32
}

type fiemapArg struct {
	Start         uint64
	Length        uint64
	Flags         uint32
	MappedExtents uint32
	ExtentCount   uint32
	Reserved      uint32
	Extents       [fiemapBatch]fiemapExtent
}

// fileExtents returns the full extent map of path, sorted by logical offset.
func fileExtents(path string) ([]fiemapExtent, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var out []fiemapExtent
	var start uint64
	for {
		arg := &fiemapArg{
			Start:       start,
			Length:      ^uint64(0) - start,
			Flags:       fiemapFlagSync,
			ExtentCount: fiemapBatch,
		}
		_, _, errno := unix.Syscall(unix.SYS_IOCTL, f.Fd(), fsIocFiemap, uintptr(unsafe.Pointer(arg)))
		if errno != 0 {
			return nil, fmt.Errorf("fiemap %s: %w", path, errno)
		}
		if arg.MappedExtents == 0 {
			break
		}
		last := false
		for i := uint32(0); i < arg.MappedExtents; i++ {
			e := arg.Extents[i]
			out = append(out, e)
			if e.Flags&fiemapExtentLast != 0 {
				last = true
			}
			start = e.Logical + e.Length
		}
		if last {
			break
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Logical < out[j].Logical })
	return out, nil
}

// DiffExtents returns the logical byte ranges of clonePath whose contents may
// differ from basePath — every allocated clone range that isn't physically
// shared with the base at the same logical offset. Both files must be
// quiescent (the frozen snapshot copy and the golden rootfs are). Ranges are
// clamped to the clone's file size and coalesced.
//
// Errors fall back on the caller (upload the full allocated range instead) —
// they never make the diff silently incomplete.
func (p *Provisioner) DiffExtents(clonePath, basePath string) ([]Range, error) {
	fi, err := os.Stat(clonePath)
	if err != nil {
		return nil, err
	}
	size := fi.Size()

	cloneExts, err := fileExtents(clonePath)
	if err != nil {
		return nil, err
	}
	baseExts, err := fileExtents(basePath)
	if err != nil {
		return nil, err
	}

	var out []Range
	bi := 0
	for _, ce := range cloneExts {
		cOff := int64(ce.Logical)
		cEnd := cOff + int64(ce.Length)
		if cOff >= size {
			continue
		}
		if cEnd > size {
			cEnd = size
		}
		// Extents whose physical location is meaningless are always included.
		if ce.Flags&(fiemapExtentUnknown|fiemapExtentDelalloc|fiemapExtentEncoded) != 0 {
			out = appendRange(out, cOff, cEnd)
			continue
		}
		// Walk base extents overlapping [cOff, cEnd); anything not physically
		// identical (same physical byte for the same logical byte) is changed.
		pos := cOff
		for pos < cEnd {
			for bi < len(baseExts) && int64(baseExts[bi].Logical)+int64(baseExts[bi].Length) <= pos {
				bi++
			}
			if bi >= len(baseExts) || int64(baseExts[bi].Logical) >= cEnd {
				// Base has no mapping here → clone-only data → changed.
				out = appendRange(out, pos, cEnd)
				break
			}
			be := baseExts[bi]
			bOff := int64(be.Logical)
			bEnd := bOff + int64(be.Length)
			if bOff > pos {
				// Gap in base before its next extent → changed.
				end := min64(bOff, cEnd)
				out = appendRange(out, pos, end)
				pos = end
				continue
			}
			// Overlap [pos, min(bEnd, cEnd)). Shared iff physical addresses line up.
			end := min64(bEnd, cEnd)
			cPhys := int64(ce.Physical) + (pos - cOff)
			bPhys := int64(be.Physical) + (pos - bOff)
			shared := cPhys == bPhys && be.Flags&(fiemapExtentUnknown|fiemapExtentDelalloc|fiemapExtentEncoded) == 0
			if !shared {
				out = appendRange(out, pos, end)
			}
			pos = end
		}
	}
	return out, nil
}

// OverlaySparse writes src's data regions into dst at the same offsets,
// leaving the rest of dst untouched. Used to rebase a Firecracker diff memory
// file (sparse; data = the dirty pages) onto a reflinked copy of its base —
// the same operation as firecracker's rebase-snap tool.
func (p *Provisioner) OverlaySparse(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer out.Close()

	fi, err := in.Stat()
	if err != nil {
		return err
	}
	size := fi.Size()
	buf := make([]byte, 1<<20)
	var off int64
	for off < size {
		dataOff, err := in.Seek(off, seekDataWhence)
		if err != nil {
			break // ENXIO: no more data
		}
		if dataOff >= size {
			break
		}
		holeOff, err := in.Seek(dataOff, seekHoleWhence)
		if err != nil || holeOff > size {
			holeOff = size
		}
		pos := dataOff
		for pos < holeOff {
			n := holeOff - pos
			if n > int64(len(buf)) {
				n = int64(len(buf))
			}
			if _, err := in.ReadAt(buf[:n], pos); err != nil {
				return fmt.Errorf("read %s @%d: %w", src, pos, err)
			}
			if _, err := out.WriteAt(buf[:n], pos); err != nil {
				return fmt.Errorf("write %s @%d: %w", dst, pos, err)
			}
			pos += n
		}
		off = holeOff
	}
	return out.Sync()
}

// appendRange adds [off, end) to out, merging with the previous range when
// adjacent or overlapping.
func appendRange(out []Range, off, end int64) []Range {
	if end <= off {
		return out
	}
	if n := len(out); n > 0 && out[n-1].Off+out[n-1].Len >= off {
		if e := out[n-1].Off + out[n-1].Len; end > e {
			out[n-1].Len = end - out[n-1].Off
		}
		return out
	}
	return append(out, Range{Off: off, Len: end - off})
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}
