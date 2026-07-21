package vm

// Page source for the UFFD memory backend. Kept untagged (no //go:build linux)
// so localSource and its bounds arithmetic — the correctness core the fault
// loop copies straight into a live guest — are unit-testable on any host.
// x/sys/unix supports darwin, so the mmap-backed localSource builds there too.

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

// pageSource supplies the mem-image bytes that service a guest page fault: the
// fault loop asks the source for the bytes backing a copy run, then UFFDIO_COPYs
// them into Firecracker's address space. localSource (the mmap of the local mem
// file) is the only implementation today; the seam exists so a remote/chunked
// source (GCS, for cross-host restore — see docs/uffd-roadmap.md Phase B) can
// slot in without touching the fault loop, which is the whole point of UFFD over
// the eager File backend.
//
// Lifetime contract: at() returns a slice whose backing array must stay valid
// and unmutated until the UFFDIO_COPY that follows completes. localSource
// satisfies this by returning a subslice of its read-only mmap (zero-copy),
// unmapped by close() only after the fault goroutine has stopped. A remote
// source must return an owned or cached buffer that outlives the copy.
type pageSource interface {
	// at returns up to length bytes of the mem image starting at byte offset
	// off. A short return (len < length) is allowed — the caller copies what it
	// gets and the tail refaults later. A nil/empty return with a nil error means
	// "nothing at this offset" (off is at or past the image end); the caller
	// skips the copy. A non-nil error means the page could not be sourced: the
	// fault is left UNSERVED and Firecracker waits forever on it, so a source that
	// can genuinely fail (e.g. a remote fetch) must ultimately escalate to killing
	// the VM rather than return an error and hang (roadmap Phase B/D).
	at(off, length uint64) ([]byte, error)
	// close releases the source (unmap, file close, cache). Called once, from the
	// fault goroutine, after it has stopped servicing faults.
	close() error
}

// localSource serves faults from a read-only mmap of the whole local mem file,
// straight out of the page cache — so a guest page fault reads the backing file
// page only if it isn't already resident. This is the original (and default)
// UFFD page source; before B0 its logic lived inline in the handler.
type localSource struct {
	mem     []byte // read-only MAP_PRIVATE mmap of the entire mem file
	memFile *os.File
}

// newLocalSource opens and mmaps the mem file read-only.
func newLocalSource(memPath string) (*localSource, error) {
	f, err := os.Open(memPath)
	if err != nil {
		return nil, fmt.Errorf("open mem file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat mem file: %w", err)
	}
	mem, err := unix.Mmap(int(f.Fd()), 0, int(fi.Size()), unix.PROT_READ, unix.MAP_PRIVATE)
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("mmap mem file: %w", err)
	}
	return &localSource{mem: mem, memFile: f}, nil
}

// at returns the mem-image bytes [off, off+length), clamped to the file end. The
// bounds test is overflow-safe: off+length can wrap past 2^64 and slip a naive
// `off+length > len` check (this is what once let an underflowed offset panic the
// slice indexer), so it compares without adding and clamps the run — a short copy
// just refaults the tail later. off at or past the end returns nil.
func (s *localSource) at(off, length uint64) ([]byte, error) {
	memLen := uint64(len(s.mem))
	if off >= memLen {
		return nil, nil // past the image; caller skips
	}
	if length > memLen-off {
		length = memLen - off
	}
	return s.mem[off : off+length], nil
}

// close unmaps the mem file and closes it. Idempotent.
func (s *localSource) close() error {
	var err error
	if s.mem != nil {
		if e := unix.Munmap(s.mem); e != nil {
			err = e
		}
		s.mem = nil
	}
	if s.memFile != nil {
		if e := s.memFile.Close(); e != nil && err == nil {
			err = e
		}
		s.memFile = nil
	}
	return err
}
