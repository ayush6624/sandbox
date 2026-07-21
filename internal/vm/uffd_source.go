package vm

// Page source for the UFFD memory backend. Kept untagged (no //go:build linux)
// so localSource and its bounds arithmetic — the correctness core the fault
// loop copies straight into a live guest — are unit-testable on any host.
// x/sys/unix supports darwin, so the mmap-backed localSource builds there too.

import (
	"fmt"
	"io"
	"os"
	"sync"

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

// chunkedSource serves faults out of fixed-size chunks of the mem image. It maps
// a fault offset to (chunk index, offset within chunk), materializes the chunk on
// first touch via load(), caches it, and serves the fault as a zero-copy subslice
// of the cached chunk — clamping any run to the chunk's end so a run never spans
// two chunks (the tail refaults into the next chunk). This is the indexing +
// cache + fault-over-chunks machinery B2's GCS source reuses unchanged: only
// load() changes (local ReadAt → lazy per-chunk GCS fetch). See
// docs/uffd-roadmap.md Phase B.
//
// Correctness: chunkSz MUST be a multiple of the guest page size so a
// boundary-clamped run stays a whole number of pages — UFFDIO_COPY requires a
// page-multiple len (and a page-aligned dst; src need NOT be page-aligned, which
// is what lets the chunk buffers be plain heap allocations, and later lets B2
// decompress into a buffer). newLocalChunkedSource rounds chunkSz to a 4 KiB
// multiple; a hugepage (2 MiB) guest would need a 2 MiB-multiple chunk — not a
// concern for the current 4 KiB-page fleet, but noted for when it is.
//
// B1 never evicts, so a cached chunk's backing array is stable for the VM's life,
// satisfying the pageSource lifetime contract. Adding eviction later must not
// free a chunk while a UFFDIO_COPY from it is in flight.
type chunkedSource struct {
	total   uint64                           // mem image size in bytes
	chunkSz uint64                           // fixed chunk size (last chunk may be short)
	load    func(idx uint64) ([]byte, error) // materialize chunk idx (nil = past image)
	closer  func() error                     // release the backing store

	mu    sync.RWMutex
	cache map[uint64][]byte // chunk idx → materialized bytes (never evicted in B1)
}

// newChunkedSource builds a chunked source over an injected chunk loader. The
// file-backed constructor (newLocalChunkedSource) wires load/closer to a mem
// file; tests inject a fake loader to exercise indexing + caching without I/O.
func newChunkedSource(total, chunkSz uint64, load func(uint64) ([]byte, error), closer func() error) *chunkedSource {
	return &chunkedSource{
		total:   total,
		chunkSz: chunkSz,
		load:    load,
		closer:  closer,
		cache:   make(map[uint64][]byte),
	}
}

// newLocalChunkedSource opens the mem file and serves chunks by ReadAt-ing them
// on demand into cached heap buffers (no whole-file mmap). chunkBytes is rounded
// down to a 4 KiB multiple, floored at one page.
func newLocalChunkedSource(memPath string, chunkBytes uint64) (*chunkedSource, error) {
	chunkSz := chunkBytes &^ (4096 - 1)
	if chunkSz < 4096 {
		chunkSz = 4096
	}
	f, err := os.Open(memPath)
	if err != nil {
		return nil, fmt.Errorf("open mem file: %w", err)
	}
	fi, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("stat mem file: %w", err)
	}
	total := uint64(fi.Size())
	load := func(idx uint64) ([]byte, error) {
		start := idx * chunkSz
		if start >= total {
			return nil, nil
		}
		n := chunkSz
		if n > total-start { // last chunk is short; overflow-safe (start < total)
			n = total - start
		}
		buf := make([]byte, n)
		// ReadAt fills buf exactly (n == remaining bytes), so a trailing io.EOF is
		// benign; any other short read is a real error that leaves the fault
		// unserved, so surface it.
		if _, err := f.ReadAt(buf, int64(start)); err != nil && err != io.EOF {
			return nil, fmt.Errorf("read chunk %d @%d: %w", idx, start, err)
		}
		return buf, nil
	}
	return newChunkedSource(total, chunkSz, load, f.Close), nil
}

// at returns a zero-copy subslice of the chunk containing off, clamped to the
// chunk's end (a straddling run returns short; the tail refaults into the next
// chunk). Overflow-safe: off ≥ total returns nil before any arithmetic.
func (cs *chunkedSource) at(off, length uint64) ([]byte, error) {
	if off >= cs.total {
		return nil, nil // past the image; caller skips
	}
	chunk, err := cs.chunk(off / cs.chunkSz)
	if err != nil {
		return nil, err
	}
	within := off % cs.chunkSz
	clen := uint64(len(chunk))
	if within >= clen {
		return nil, nil // chunk shorter than the index implies; nothing here
	}
	if avail := clen - within; length > avail {
		length = avail // clamp to this chunk
	}
	return chunk[within : within+length], nil
}

// chunk returns chunk idx from the cache, loading and caching it on a miss.
func (cs *chunkedSource) chunk(idx uint64) ([]byte, error) {
	cs.mu.RLock()
	c, ok := cs.cache[idx]
	cs.mu.RUnlock()
	if ok {
		return c, nil
	}
	c, err := cs.load(idx)
	if err != nil {
		return nil, err
	}
	if c == nil {
		return nil, nil // past the image
	}
	// A concurrent miss may have loaded the same chunk; keep whichever landed in
	// the map so every caller after this converges on one backing array. Either
	// buffer is a correct, immutable copy, so a caller holding the loser is still
	// safe for its in-flight copy.
	cs.mu.Lock()
	if existing, ok := cs.cache[idx]; ok {
		c = existing
	} else {
		cs.cache[idx] = c
	}
	cs.mu.Unlock()
	return c, nil
}

// close drops the chunk cache and releases the backing store. Idempotent.
func (cs *chunkedSource) close() error {
	cs.mu.Lock()
	cs.cache = nil
	cs.mu.Unlock()
	if cs.closer != nil {
		return cs.closer()
	}
	return nil
}
