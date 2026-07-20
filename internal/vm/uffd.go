package vm

import (
	"encoding/binary"
	"math/bits"
)

// Platform-neutral pieces of the UFFD memory backend (see uffd_linux.go for the
// syscall/mmap/socket handler). Kept untagged so the fault-offset arithmetic —
// the correctness core — is unit-testable on any host.

// uffdioCopy is the UFFDIO_COPY ioctl request: _IOWR(UFFDIO=0xAA, 0x03,
// sizeof(uffdioCopyArg)=40) = 0xc028aa03. Constant across amd64/arm64 Linux.
const uffdioCopy = 0xc028aa03

// uffdEventPagefault is UFFD_EVENT_PAGEFAULT — the only event a MISSING-mode
// registration delivers that we act on.
const uffdEventPagefault = 0x12

// uffdMsgSize is sizeof(struct uffd_msg). Reads on the uffd return whole
// multiples of this; the faulting address of a pagefault event lives at
// offset 16 (u8 event @0, 7B reserved, then the union: u64 flags @8, u64
// address @16).
const uffdMsgSize = 32

// uffdioCopyArg mirrors struct uffdio_copy: copy Len bytes from Src (our
// address space) into Dst (Firecracker's), waking the faulting thread.
type uffdioCopyArg struct {
	Dst  uint64
	Src  uint64
	Len  uint64
	Mode uint64
	Copy int64
}

// guestRegion is one entry of the JSON mapping array Firecracker sends: a guest
// memory region living at BaseHostVirtAddr in Firecracker's address space,
// backed by the mem file starting at Offset.
//
// PageSize's wire name is "page_size_kib", but Firecracker v1.15 populates it
// with the page size in BYTES (observed: 4096), not KiB — a known misnomer.
// pageSizeBytes() normalizes both interpretations so we page in at the true
// granularity instead of, say, 4096×1024 = 4 MiB.
type guestRegion struct {
	BaseHostVirtAddr uint64 `json:"base_host_virt_addr"`
	Size             uint64 `json:"size"`
	Offset           uint64 `json:"offset"`
	PageSize         uint64 `json:"page_size_kib"`
}

// pageSizeBytes returns the region's page size in bytes, tolerating both the
// documented KiB unit (small values like 4 or 2048 → ×1024) and Firecracker
// v1.15's actual bytes unit (4096 → as-is). 0 defaults to 4 KiB.
func (r guestRegion) pageSizeBytes() uint64 {
	switch {
	case r.PageSize == 0:
		return 4096
	case r.PageSize < 4096: // a KiB value (4=4KiB, 2048=2MiB)
		return r.PageSize * 1024
	default: // already bytes (4096, ...)
		return r.PageSize
	}
}

// prefetchPages is the fault-ahead window: on a fault we copy the faulting
// page plus up to this many following pages in one UFFDIO_COPY, amortizing the
// per-fault userspace round-trip that made single-page UFFD lose to the eager
// File backend (docs/uffd-roadmap.md, Phase A). 32 × 4 KiB = 128 KiB — large
// enough to cut round-trips sharply, small enough not to over-read cold pages
// the guest never touches. FC's own reference handler fills the whole region;
// this is a bounded middle ground.
const prefetchPages = 32

// locate finds the region whose PAGE contains addr and returns that region plus
// the page-aligned fault address and page size. Matching on the aligned address
// (not the raw fault address) mirrors Firecracker's reference handler and
// guarantees aligned >= BaseHostVirtAddr, so aligned-base can't underflow even
// if a region base isn't page-aligned; it also requires the whole page to sit
// inside the region. ok=false means no region's page contains the fault.
func locate(regions []guestRegion, addr uint64) (r guestRegion, aligned, pageSize uint64, ok bool) {
	for _, reg := range regions {
		pageSize = reg.pageSizeBytes()
		aligned = addr &^ (pageSize - 1)
		if aligned < reg.BaseHostVirtAddr || aligned+pageSize > reg.BaseHostVirtAddr+reg.Size {
			continue
		}
		return reg, aligned, pageSize, true
	}
	return guestRegion{}, 0, 0, false
}

// resolvePage maps a fault to the single page containing it: the page-aligned
// destination address, its source offset in the mem file, and the page size.
// Thin wrapper over locate, kept for callers/tests that want one page.
func resolvePage(regions []guestRegion, addr uint64) (aligned, srcOff, pageSize uint64, ok bool) {
	r, aligned, pageSize, ok := locate(regions, addr)
	if !ok {
		return 0, 0, 0, false
	}
	return aligned, r.Offset + (aligned - r.BaseHostVirtAddr), pageSize, true
}

// --- working-set recording + prewarm ---
//
// The set of mem-file pages a guest faults during a wake is recorded (page
// indices, which are stable across restores — unlike host addresses, whose
// mmap base changes each time), persisted beside the sandbox, and bulk-loaded
// on the next wake before the guest fault-storms them in one page at a time.
// This is what the cold first-wake and the Phase B remote source both need
// (docs/uffd-roadmap.md). The unit is a fixed 4 KiB page index.

// wsPageUnit is the page granularity of the working-set bitset. Fixed at 4 KiB
// (the observed guest page size) so a persisted set stays comparable across
// restores regardless of how a region reports its page size.
const wsPageUnit = 4096

// prewarmMaxRun caps a single prewarm UFFDIO_COPY at 4 MiB so one giant
// contiguous working set doesn't become one monster syscall that stalls the
// guest with no interleaving.
const prewarmMaxRun = 1024 * wsPageUnit

// bitset is a compact set of mem-file page indices — the recorded working set.
type bitset struct {
	words []uint64
	n     uint64 // capacity in bits (number of pages)
}

func newBitset(numPages uint64) *bitset {
	return &bitset{words: make([]uint64, (numPages+63)/64), n: numPages}
}

func (b *bitset) set(i uint64) {
	if i < b.n {
		b.words[i/64] |= 1 << (i % 64)
	}
}

func (b *bitset) get(i uint64) bool {
	return i < b.n && b.words[i/64]&(1<<(i%64)) != 0
}

func (b *bitset) count() int {
	c := 0
	for _, w := range b.words {
		c += bits.OnesCount64(w)
	}
	return c
}

// pageRun is a contiguous span of set page indices.
type pageRun struct{ start, count uint64 }

// runs returns the maximal contiguous runs of set bits — the coalesced spans
// prewarm copies, one UFFDIO_COPY each (further capped by prewarmMaxRun).
func (b *bitset) runs() []pageRun {
	var out []pageRun
	for i := uint64(0); i < b.n; {
		if !b.get(i) {
			i++
			continue
		}
		start := i
		for i < b.n && b.get(i) {
			i++
		}
		out = append(out, pageRun{start: start, count: i - start})
	}
	return out
}

// marshal serializes the set as little-endian 64-bit words.
func (b *bitset) marshal() []byte {
	out := make([]byte, len(b.words)*8)
	for i, w := range b.words {
		binary.LittleEndian.PutUint64(out[i*8:], w)
	}
	return out
}

// bitsetFromBytes rebuilds a set of capacity numPages from marshal() output,
// tolerating a length mismatch (a mem size that changed clamps, never panics).
func bitsetFromBytes(data []byte, numPages uint64) *bitset {
	b := newBitset(numPages)
	for i := 0; i < len(b.words) && (i+1)*8 <= len(data); i++ {
		b.words[i] = binary.LittleEndian.Uint64(data[i*8:])
	}
	return b
}

// hostAddrForOffset maps a mem-file offset back to its host virtual address in
// the current restore's regions — the inverse of the fault path, used by
// prewarm to pick the UFFDIO_COPY destination. ok=false if no region covers it.
func hostAddrForOffset(regions []guestRegion, memOffset uint64) (dst uint64, ok bool) {
	for _, r := range regions {
		if memOffset >= r.Offset && memOffset < r.Offset+r.Size {
			return r.BaseHostVirtAddr + (memOffset - r.Offset), true
		}
	}
	return 0, false
}

// faultWindow returns the copy run to service a fault at addr with fault-ahead:
// the destination host address, the source offset in the mem file, and the byte
// length — the faulting page plus up to prefetch-1 following pages, clamped to
// the end of the enclosing region so a copy never crosses a region boundary.
// length is always a whole number of pages (UFFDIO_COPY requires it) and >0 on
// ok. Caller still clamps against the mem-file length.
func faultWindow(regions []guestRegion, addr, prefetch uint64) (dst, srcOff, length uint64, ok bool) {
	r, aligned, pageSize, ok := locate(regions, addr)
	if !ok {
		return 0, 0, 0, false
	}
	if prefetch == 0 {
		prefetch = 1
	}
	dst = aligned
	srcOff = r.Offset + (aligned - r.BaseHostVirtAddr)
	length = prefetch * pageSize
	if regionTail := r.BaseHostVirtAddr + r.Size - aligned; length > regionTail {
		length = regionTail // clamp to region end (whole pages, since size is page-multiple)
	}
	return dst, srcOff, length, true
}
