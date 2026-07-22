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
// docs/uffd-roadmap.md Phase B and docs/uffd-b2-design.md.
//
// Correctness: chunkSz MUST be a multiple of the guest page size so a
// boundary-clamped run stays a whole number of pages — UFFDIO_COPY requires a
// page-multiple len (and a page-aligned dst; src need NOT be page-aligned, which
// is what lets the chunk buffers be plain heap allocations, and later lets B2
// decompress into a buffer). newLocalChunkedSource rounds chunkSz to a 4 KiB
// multiple; a hugepage (2 MiB) guest would need a 2 MiB-multiple chunk — not a
// concern for the current 4 KiB-page fleet, but noted for when it is.
//
// Concurrency (B2a): load() may be slow (a GCS fetch), so chunk() single-flights
// per index — a fault and any number of prefetches for the same chunk share one
// in-flight load and its result. prefetch>0 makes a fault kick off background
// loads of the next `prefetch` chunks (bounded by a semaphore) to hide the
// per-chunk RTT behind sequential access; the fault thread only ever blocks on
// its own chunk. Local sources set prefetch=0 (no RTT to hide → no goroutines,
// behaviour identical to B1).
//
// No eviction yet: a cached chunk's backing array is stable for the VM's life,
// satisfying the pageSource lifetime contract. Adding eviction later must not
// free a chunk while a UFFDIO_COPY from it is in flight. close() first stops new
// prefetches and drains in-flight ones, so no background load touches the backing
// store (or the cache) after it is released.
type chunkedSource struct {
	total    uint64                           // mem image size in bytes
	chunkSz  uint64                           // fixed chunk size (last chunk may be short)
	prefetch uint64                           // chunk-level fault-ahead window; 0 = none
	load     func(idx uint64) ([]byte, error) // materialize chunk idx (nil = past image)
	closer   func() error                     // release the backing store

	mu       sync.Mutex
	cache    map[uint64][]byte     // chunk idx → materialized bytes (never evicted yet)
	inflight map[uint64]*chunkLoad // chunk idx → in-flight load (single-flight)
	sem      chan struct{}         // bounds concurrent prefetch loads (nil if prefetch==0)
	wg       sync.WaitGroup        // tracks prefetch goroutines, drained by close()
	closed   bool
}

// chunkLoad is one in-flight chunk load, shared by every caller that races for
// the same index; done closes when buf/err are set.
type chunkLoad struct {
	done chan struct{}
	buf  []byte
	err  error
}

// newChunkedSource builds a chunked source over an injected chunk loader. The
// file-backed constructor (newLocalChunkedSource) wires load/closer to a mem
// file; tests inject a fake loader to exercise indexing + caching without I/O.
// prefetch is the chunk-level fault-ahead window (0 = none; the GCS source sets
// it >0 to pipeline sequential faults).
func newChunkedSource(total, chunkSz, prefetch uint64, load func(uint64) ([]byte, error), closer func() error) *chunkedSource {
	cs := &chunkedSource{
		total:    total,
		chunkSz:  chunkSz,
		prefetch: prefetch,
		load:     load,
		closer:   closer,
		cache:    make(map[uint64][]byte),
		inflight: make(map[uint64]*chunkLoad),
	}
	if prefetch > 0 {
		cs.sem = make(chan struct{}, prefetch)
	}
	return cs
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
	return newChunkedSource(total, chunkSz, 0, load, f.Close), nil // local: no prefetch
}

// at returns a zero-copy subslice of the chunk containing off, clamped to the
// chunk's end (a straddling run returns short; the tail refaults into the next
// chunk). Overflow-safe: off ≥ total returns nil before any arithmetic. After
// serving the faulting chunk it kicks off prefetch of the following chunks.
func (cs *chunkedSource) at(off, length uint64) ([]byte, error) {
	if off >= cs.total {
		return nil, nil // past the image; caller skips
	}
	idx := off / cs.chunkSz
	chunk, err := cs.chunk(idx)
	if err != nil {
		return nil, err
	}
	cs.prefetchAhead(idx)
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

// chunk returns chunk idx from the cache, single-flighting the load: a fault and
// any prefetches racing for the same index share one in-flight load() and its
// result, so a slow (network) load runs at most once per chunk.
func (cs *chunkedSource) chunk(idx uint64) ([]byte, error) {
	cs.mu.Lock()
	if c, ok := cs.cache[idx]; ok {
		cs.mu.Unlock()
		return c, nil
	}
	if cl, ok := cs.inflight[idx]; ok {
		cs.mu.Unlock()
		<-cl.done // someone else is loading it; wait for their result
		return cl.buf, cl.err
	}
	cl := &chunkLoad{done: make(chan struct{})}
	cs.inflight[idx] = cl
	cs.mu.Unlock()

	cl.buf, cl.err = cs.load(idx)

	cs.mu.Lock()
	if cl.err == nil && cl.buf != nil && !cs.closed {
		cs.cache[idx] = cl.buf
	}
	delete(cs.inflight, idx)
	cs.mu.Unlock()
	close(cl.done)
	return cl.buf, cl.err
}

// prefetchAhead launches background loads of the next `prefetch` chunks that are
// not already cached or in flight, bounded by the semaphore. Best-effort: if the
// pool is full it skips (the fault path will fetch on demand), and a prefetch
// error is dropped — the real fault into that chunk re-loads and escalates.
func (cs *chunkedSource) prefetchAhead(idx uint64) {
	if cs.prefetch == 0 {
		return
	}
	for i := idx + 1; i <= idx+cs.prefetch; i++ {
		if i*cs.chunkSz >= cs.total {
			break // past the image
		}
		cs.mu.Lock()
		_, cached := cs.cache[i]
		_, loading := cs.inflight[i]
		skip := cs.closed || cached || loading
		if !skip {
			cs.wg.Add(1) // registered under the lock so close() can't race the drain
		}
		cs.mu.Unlock()
		if skip {
			continue
		}
		select {
		case cs.sem <- struct{}{}: // acquired a worker slot
			go func(i uint64) {
				defer cs.wg.Done()
				defer func() { <-cs.sem }()
				_, _ = cs.chunk(i)
			}(i)
		default:
			cs.wg.Done() // pool full; don't prefetch this one
		}
	}
}

// close stops new prefetches, drains in-flight ones, drops the cache, and
// releases the backing store. Draining before releasing guarantees no background
// load touches the store or cache after this returns. Idempotent.
func (cs *chunkedSource) close() error {
	cs.mu.Lock()
	if cs.closed {
		cs.mu.Unlock()
		return nil
	}
	cs.closed = true
	cs.mu.Unlock()

	cs.wg.Wait() // let outstanding prefetch loads finish (no lock held → no deadlock)

	cs.mu.Lock()
	cs.cache = nil
	cs.mu.Unlock()
	if cs.closer != nil {
		return cs.closer()
	}
	return nil
}

// fatalOnce invokes a kill callback at most once. The UFFD fault path uses it to
// stop a guest whose fault cannot be served (the page source returned an error):
// Firecracker waits forever on an unserved fault, so the only safe response is to
// kill the VM (the wake then fails cleanly, like a failed File-backend wake)
// rather than leave a hung guest. See docs/uffd-b2-design.md.
type fatalOnce struct {
	mu    sync.Mutex
	fn    func(error)
	fired bool
}

// set installs the kill callback. Called before Firecracker connects, so the
// fault goroutine (which only runs after connect) always observes it.
func (f *fatalOnce) set(fn func(error)) {
	f.mu.Lock()
	f.fn = fn
	f.mu.Unlock()
}

// fire invokes the callback if one is set and it hasn't fired yet; reports
// whether it actually fired this call.
func (f *fatalOnce) fire(err error) bool {
	f.mu.Lock()
	fn, fired := f.fn, f.fired
	f.fired = true
	f.mu.Unlock()
	if fired || fn == nil {
		return false
	}
	fn(err)
	return true
}
