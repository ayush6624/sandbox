package vm

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestResolvePage(t *testing.T) {
	const kib4 = 4096
	// Two regions: the first backed from mem-file offset 0, the second from a
	// nonzero offset (as a multi-region snapshot would lay them out).
	regions := []guestRegion{
		{BaseHostVirtAddr: 0x1000_0000, Size: 0x10000, Offset: 0, PageSize: 4},
		{BaseHostVirtAddr: 0x2000_0000, Size: 0x10000, Offset: 0x10000, PageSize: 4},
	}

	tests := []struct {
		name        string
		addr        uint64
		wantAligned uint64
		wantSrcOff  uint64
		wantPage    uint64
		wantOK      bool
	}{
		{"region0 page-aligned start", 0x1000_0000, 0x1000_0000, 0, kib4, true},
		{"region0 unaligned mid-page", 0x1000_0abc, 0x1000_0000, 0, kib4, true},
		{"region0 second page", 0x1000_1000, 0x1000_1000, 0x1000, kib4, true},
		{"region0 unaligned second page", 0x1000_1fff, 0x1000_1000, 0x1000, kib4, true},
		{"region1 start maps to its offset", 0x2000_0000, 0x2000_0000, 0x10000, kib4, true},
		{"region1 third page", 0x2000_2345, 0x2000_2000, 0x12000, kib4, true},
		{"below all regions", 0x0fff_ffff, 0, 0, 0, false},
		{"gap between regions", 0x1800_0000, 0, 0, 0, false},
		{"at region0 end (exclusive)", 0x1001_0000, 0, 0, 0, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			aligned, srcOff, page, ok := resolvePage(regions, tc.addr)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if aligned != tc.wantAligned {
				t.Errorf("aligned = %#x, want %#x", aligned, tc.wantAligned)
			}
			if srcOff != tc.wantSrcOff {
				t.Errorf("srcOff = %#x, want %#x", srcOff, tc.wantSrcOff)
			}
			if page != tc.wantPage {
				t.Errorf("pageSize = %d, want %d", page, tc.wantPage)
			}
			// dst and src must move in lockstep: the page's offset from its
			// region base is identical on both sides of the copy.
			i := regionOf(t, regions, tc.addr)
			if aligned-regions[i].BaseHostVirtAddr != srcOff-regions[i].Offset {
				t.Errorf("dst/src delta mismatch: aligned=%#x srcOff=%#x", aligned, srcOff)
			}
		})
	}
}

func regionOf(t *testing.T, regions []guestRegion, addr uint64) int {
	t.Helper()
	for i, r := range regions {
		if addr >= r.BaseHostVirtAddr && addr < r.BaseHostVirtAddr+r.Size {
			return i
		}
	}
	t.Fatalf("addr %#x in no region", addr)
	return -1
}

// TestResolvePageDefaultPageSize covers a mapping that omits page_size_kib
// (defaults to 4096) — a safety net against a Firecracker JSON field rename.
func TestResolvePageDefaultPageSize(t *testing.T) {
	regions := []guestRegion{{BaseHostVirtAddr: 0x4000, Size: 0x8000, Offset: 0x100000}}
	_, srcOff, page, ok := resolvePage(regions, 0x5abc)
	if !ok {
		t.Fatal("expected hit")
	}
	if page != 4096 {
		t.Fatalf("pageSize = %d, want 4096 default", page)
	}
	if srcOff != 0x100000+(0x5000-0x4000) {
		t.Fatalf("srcOff = %#x, want %#x", srcOff, 0x100000+0x1000)
	}
}

// TestResolvePageNoUnderflow is the regression for the crash: a fault whose
// page-aligned address falls below the region base (e.g. a base not aligned to
// the page size) must NOT match — the old code matched on the raw address and
// then underflowed aligned-base into a ~2^64 offset that panicked the indexer.
func TestResolvePageNoUnderflow(t *testing.T) {
	// Region base is 0x800 into a page; a fault at base aligns down to 0x...000,
	// which is below base.
	regions := []guestRegion{{BaseHostVirtAddr: 0x1000_0800, Size: 0x10000, Offset: 0, PageSize: 4}}
	if _, srcOff, _, ok := resolvePage(regions, 0x1000_0800); ok {
		t.Fatalf("expected no match (aligned below base), got srcOff=%#x", srcOff)
	}
}

// TestResolvePageHugePage covers 2 MiB pages (page_size_kib=2048), the layout
// whose -2 MiB underflow surfaced the bug on the fleet.
func TestResolvePageHugePage(t *testing.T) {
	const twoMiB = 2 * 1024 * 1024
	regions := []guestRegion{{BaseHostVirtAddr: 0x4000_0000, Size: 0x4000_0000, Offset: 0, PageSize: 2048}}
	aligned, srcOff, page, ok := resolvePage(regions, 0x4020_1234)
	if !ok {
		t.Fatal("expected hit")
	}
	if page != twoMiB {
		t.Fatalf("pageSize = %d, want %d", page, twoMiB)
	}
	if aligned != 0x4020_0000 || srcOff != 0x20_0000 {
		t.Fatalf("aligned=%#x srcOff=%#x, want 0x40200000 / 0x200000", aligned, srcOff)
	}
}

// TestResolvePagePageStraddlesEnd rejects a page that would run past the region
// end (keeps a copy from reading beyond the mapped region).
func TestResolvePagePageStraddlesEnd(t *testing.T) {
	// size is one byte into the last page, so the aligned last page + pageSize
	// exceeds base+size.
	regions := []guestRegion{{BaseHostVirtAddr: 0x2000_0000, Size: 0x1001, Offset: 0, PageSize: 4}}
	if _, _, _, ok := resolvePage(regions, 0x2000_1000); ok {
		t.Fatal("expected no match: page straddles region end")
	}
}

// TestPageSizeBytes pins the unit normalization, including Firecracker v1.15's
// real behavior: page_size_kib carries BYTES (4096), not KiB.
func TestPageSizeBytes(t *testing.T) {
	cases := []struct {
		field uint64
		want  uint64
	}{
		{0, 4096},       // absent → 4 KiB default
		{4, 4096},       // documented KiB unit: 4 KiB
		{2048, 2 << 20}, // KiB unit: 2 MiB huge page
		{4096, 4096},    // FC v1.15 actual: bytes, 4 KiB page (the crash's origin)
	}
	for _, c := range cases {
		if got := (guestRegion{PageSize: c.field}).pageSizeBytes(); got != c.want {
			t.Errorf("pageSizeBytes(%d) = %d, want %d", c.field, got, c.want)
		}
	}
}

// TestResolvePageRealFirecracker replays the exact region the fleet logged
// (base 4 KiB-page-aligned host VA, 1 GiB, page_size_kib=4096-as-bytes): a
// fault must page in at true 4 KiB granularity, not 4 MiB.
func TestResolvePageRealFirecracker(t *testing.T) {
	base := uint64(124664269504512) // 0x716c00000000, from the fleet log
	regions := []guestRegion{{BaseHostVirtAddr: base, Size: 1 << 30, Offset: 0, PageSize: 4096}}
	aligned, srcOff, page, ok := resolvePage(regions, base+0x1234)
	if !ok {
		t.Fatal("expected hit")
	}
	if page != 4096 {
		t.Fatalf("pageSize = %d, want 4096 (4 KiB, not 4 MiB)", page)
	}
	if aligned != base+0x1000 || srcOff != 0x1000 {
		t.Fatalf("aligned=%#x srcOff=%#x, want base+0x1000 / 0x1000", aligned, srcOff)
	}
}

// TestFaultWindow covers fault-ahead: the run spans the faulting page plus
// prefetch-1 following pages, is clamped to the region end, and never crosses
// a boundary.
func TestFaultWindow(t *testing.T) {
	const p = 4096
	// One region: base 0x1000_0000, 64 KiB (16 pages), backed from offset 0x8000.
	regions := []guestRegion{{BaseHostVirtAddr: 0x1000_0000, Size: 0x10000, Offset: 0x8000, PageSize: 4096}}

	t.Run("full window mid-region", func(t *testing.T) {
		dst, srcOff, length, ok := faultWindow(regions, 0x1000_0abc, 4)
		if !ok || dst != 0x1000_0000 || srcOff != 0x8000 || length != 4*p {
			t.Fatalf("got dst=%#x srcOff=%#x len=%d ok=%v", dst, srcOff, length, ok)
		}
	})
	t.Run("clamped to region end", func(t *testing.T) {
		// Fault in the 15th page (0-indexed 14); only 2 pages remain, so a
		// prefetch of 8 clamps to 2 pages.
		addr := regions[0].BaseHostVirtAddr + 14*p + 10
		dst, srcOff, length, ok := faultWindow(regions, addr, 8)
		if !ok || dst != regions[0].BaseHostVirtAddr+14*p || length != 2*p {
			t.Fatalf("got dst=%#x len=%d ok=%v, want dst=%#x len=%d", dst, length, ok, regions[0].BaseHostVirtAddr+14*p, 2*p)
		}
		if srcOff != 0x8000+14*p {
			t.Fatalf("srcOff=%#x, want %#x", srcOff, 0x8000+14*p)
		}
	})
	t.Run("prefetch 1 is a single page", func(t *testing.T) {
		_, _, length, ok := faultWindow(regions, 0x1000_0000, 1)
		if !ok || length != p {
			t.Fatalf("len=%d ok=%v, want %d", length, ok, p)
		}
	})
	t.Run("prefetch 0 defaults to 1 page", func(t *testing.T) {
		_, _, length, ok := faultWindow(regions, 0x1000_0000, 0)
		if !ok || length != p {
			t.Fatalf("len=%d ok=%v, want %d", length, ok, p)
		}
	})
	t.Run("last page: window is exactly one page", func(t *testing.T) {
		addr := regions[0].BaseHostVirtAddr + 15*p // final page
		_, _, length, ok := faultWindow(regions, addr, 8)
		if !ok || length != p {
			t.Fatalf("len=%d ok=%v, want %d", length, ok, p)
		}
	})
	t.Run("no region", func(t *testing.T) {
		if _, _, _, ok := faultWindow(regions, 0x2000_0000, 4); ok {
			t.Fatal("expected no match")
		}
	})
}

// TestLocalSource covers the default page source: mmap of a mem file, byte-range
// fetch with overflow-safe clamping, and zero-copy aliasing of the mapping.
func TestLocalSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem")
	data := make([]byte, 3*4096) // 12 KiB, three "pages"
	for i := range data {
		data[i] = byte(i * 7) // deterministic, non-trivial pattern
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	s, err := newLocalSource(path)
	if err != nil {
		t.Fatalf("newLocalSource: %v", err)
	}
	defer s.close()

	t.Run("full read within bounds", func(t *testing.T) {
		b, err := s.at(0, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, data[:4096]) {
			t.Fatalf("bytes mismatch (len %d)", len(b))
		}
	})
	t.Run("offset read", func(t *testing.T) {
		b, err := s.at(4096, 100)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(b, data[4096:4196]) {
			t.Fatalf("bytes mismatch (len %d)", len(b))
		}
	})
	t.Run("clamped past end returns a short slice", func(t *testing.T) {
		off := uint64(len(data)) - 10
		b, err := s.at(off, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) != 10 || !bytes.Equal(b, data[off:]) {
			t.Fatalf("len = %d, want 10 (short-clamped)", len(b))
		}
	})
	t.Run("off exactly at end returns nil", func(t *testing.T) {
		b, err := s.at(uint64(len(data)), 4096)
		if err != nil || b != nil {
			t.Fatalf("b = %v err = %v, want nil, nil", b, err)
		}
	})
	t.Run("off past end (overflow-safe) returns nil", func(t *testing.T) {
		// off + length would wrap past 2^64 — the check must not add them.
		b, err := s.at(^uint64(0)-100, 4096)
		if err != nil || b != nil {
			t.Fatalf("b = %v err = %v, want nil, nil", b, err)
		}
	})
	t.Run("zero-copy: returned slice aliases the mmap", func(t *testing.T) {
		b, err := s.at(4096, 100)
		if err != nil {
			t.Fatal(err)
		}
		if &b[0] != &s.mem[4096] {
			t.Fatal("at() must return a subslice of the mmap, not a copy")
		}
	})
}

// TestChunkedSourceIndexing exercises the chunk-offset math and boundary
// clamping with an injected loader (no file), asserting a run never spans two
// chunks and the returned bytes match the image.
func TestChunkedSourceIndexing(t *testing.T) {
	const chunkSz = 4096 * 4      // 16 KiB
	const total = chunkSz*2 + 500 // 2 full chunks + a short third (500 B)
	image := make([]byte, total)
	for i := range image {
		image[i] = byte(i*13 + 1)
	}
	loads := make(map[uint64]int)
	load := func(idx uint64) ([]byte, error) {
		start := idx * chunkSz
		if start >= total {
			return nil, nil
		}
		loads[idx]++
		n := uint64(chunkSz)
		if n > total-start {
			n = total - start
		}
		return image[start : start+n], nil
	}
	cs := newChunkedSource(total, chunkSz, 0, load, nil)

	// A within-chunk run returns exactly the requested bytes.
	if b, err := cs.at(100, 200); err != nil || !bytes.Equal(b, image[100:300]) {
		t.Fatalf("within-chunk: len=%d err=%v", len(b), err)
	}
	// A run that would straddle the chunk-0/chunk-1 boundary is clamped to the
	// end of chunk 0 (the tail refaults into chunk 1).
	off := uint64(chunkSz - 8)
	b, err := cs.at(off, 4096)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(b)) != 8 || !bytes.Equal(b, image[off:chunkSz]) {
		t.Fatalf("boundary clamp: len=%d want 8", len(b))
	}
	// The refaulted tail is served from chunk 1.
	if b, err := cs.at(chunkSz, 4096); err != nil || !bytes.Equal(b, image[chunkSz:chunkSz+4096]) {
		t.Fatalf("next chunk: len=%d err=%v", len(b), err)
	}
	// The short last chunk clamps to the image end.
	if b, err := cs.at(chunkSz*2+100, 4096); err != nil || uint64(len(b)) != 400 || !bytes.Equal(b, image[chunkSz*2+100:total]) {
		t.Fatalf("short last chunk: len=%d err=%v", len(b), err)
	}
	// Past the image → nil.
	if b, err := cs.at(total, 4096); err != nil || b != nil {
		t.Fatalf("past end: b=%v err=%v", b, err)
	}
	// Cache: chunk 0 was loaded once despite multiple faults into it.
	if loads[0] != 1 {
		t.Fatalf("chunk 0 loaded %d times, want 1 (cache miss then hit)", loads[0])
	}
}

// TestChunkedSourceLoadError surfaces a loader error as an at() error (an
// unserved fault the handler must escalate, not swallow).
func TestChunkedSourceLoadError(t *testing.T) {
	boom := errors.New("fetch failed")
	cs := newChunkedSource(1<<20, 4096, 0, func(uint64) ([]byte, error) { return nil, boom }, nil)
	if _, err := cs.at(0, 4096); !errors.Is(err, boom) {
		t.Fatalf("at() err = %v, want %v", err, boom)
	}
}

// TestChunkedSourceSingleFlight proves concurrent faults+prefetches for the same
// chunk share one load (a slow network fetch runs at most once per chunk).
func TestChunkedSourceSingleFlight(t *testing.T) {
	const chunkSz = 4096
	var mu sync.Mutex
	loads := 0
	release := make(chan struct{})
	load := func(idx uint64) ([]byte, error) {
		mu.Lock()
		loads++
		mu.Unlock()
		<-release // hold the load open so all callers pile onto the one in-flight
		return make([]byte, chunkSz), nil
	}
	cs := newChunkedSource(chunkSz*4, chunkSz, 0, load, nil)

	const racers = 8
	var wg sync.WaitGroup
	wg.Add(racers)
	for i := 0; i < racers; i++ {
		go func() { defer wg.Done(); _, _ = cs.at(0, chunkSz) }()
	}
	// Give the racers time to converge on the single in-flight load, then release.
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()

	if loads != 1 {
		t.Fatalf("load called %d times for one chunk, want 1 (single-flight)", loads)
	}
}

// TestChunkedSourcePrefetch verifies a fault warms the next `prefetch` chunks in
// the background and that close() drains those goroutines.
func TestChunkedSourcePrefetch(t *testing.T) {
	const chunkSz = 4096
	const nChunks = 10
	var mu sync.Mutex
	loaded := map[uint64]bool{}
	load := func(idx uint64) ([]byte, error) {
		mu.Lock()
		loaded[idx] = true
		mu.Unlock()
		return make([]byte, chunkSz), nil
	}
	cs := newChunkedSource(chunkSz*nChunks, chunkSz, 3, load, nil) // prefetch 3

	if _, err := cs.at(0, chunkSz); err != nil { // fault chunk 0 → prefetch 1,2,3
		t.Fatal(err)
	}
	// Prefetch is async; poll briefly for chunks 1..3 to warm.
	deadline := time.Now().Add(time.Second)
	for {
		mu.Lock()
		got := loaded[1] && loaded[2] && loaded[3]
		mu.Unlock()
		if got || time.Now().After(deadline) {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	for i := uint64(1); i <= 3; i++ {
		if !loaded[i] {
			t.Errorf("chunk %d was not prefetched", i)
		}
	}
	if loaded[5] {
		t.Error("chunk 5 prefetched but window was only 3")
	}
	// close() must return promptly (drains prefetch goroutines, no hang).
	done := make(chan struct{})
	go func() { _ = cs.close(); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("close() did not drain prefetch goroutines in time")
	}
}

// TestFatalOnce pins the kill-once semantics the fault path relies on.
func TestFatalOnce(t *testing.T) {
	var f fatalOnce
	// fire before set: no callback, reports not-fired.
	if f.fire(errors.New("x")) {
		t.Fatal("fire with no callback should report false")
	}
	calls := 0
	f.set(func(error) { calls++ })
	// The pre-set fire already marked it fired, so a real fire now won't run.
	// Reset for a clean assertion of the once-semantics on a fresh instance.
	var g fatalOnce
	g.set(func(error) { calls++ })
	if !g.fire(errors.New("boom")) {
		t.Fatal("first fire should report true")
	}
	if g.fire(errors.New("again")) {
		t.Fatal("second fire should report false (already fired)")
	}
	if calls != 1 {
		t.Fatalf("callback ran %d times, want exactly 1", calls)
	}
}

// TestLatencyHist checks the histogram's count/mean/max and that the percentile
// estimate lands in the right bucket for a skewed distribution (many fast faults,
// a few slow ones — the shape a warm cache with cold-fetch tails produces).
func TestLatencyHist(t *testing.T) {
	var h latencyHist
	// 95 fast faults at ~10µs, 5 slow at ~50ms (the slow tail is >1%, so p99
	// must reach it; a single slow sample would be p100/max, not p99).
	for i := 0; i < 95; i++ {
		h.record(10 * time.Microsecond)
	}
	for i := 0; i < 5; i++ {
		h.record(50 * time.Millisecond)
	}

	if got := h.count.Load(); got != 100 {
		t.Fatalf("count = %d, want 100", got)
	}
	if got := h.maxUS.Load(); got != 50000 {
		t.Fatalf("maxUS = %d, want 50000", got)
	}
	// p50 sits among the 10µs faults: bucket for 10µs is floor(log2 10)=3
	// ([8,16)µs), upper edge 16.
	if p50 := h.percentileUS(0.50); p50 != 16 {
		t.Errorf("p50 = %dµs, want 16 (bucket ceiling for ~10µs)", p50)
	}
	// p99 must reach the slow tail: 50000µs → floor(log2 50000)=15 ([32768,65536)),
	// upper edge 65536.
	if p99 := h.percentileUS(0.99); p99 < 32768 {
		t.Errorf("p99 = %dµs, want ≥32768 (should include the 50ms tail)", p99)
	}
	if h.summary() == "no faults served" {
		t.Error("summary should report faults")
	}
}

// TestLocalChunkedSource covers the file-backed loader end to end, including a
// short last chunk and rounding a non-page-multiple chunk size down to 4 KiB.
func TestLocalChunkedSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mem")
	data := make([]byte, 4096*10+123) // 10 pages + a short tail
	for i := range data {
		data[i] = byte(i*31 + 5)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	// 5000 bytes rounds down to 4096 (one page).
	cs, err := newLocalChunkedSource(path, 5000)
	if err != nil {
		t.Fatal(err)
	}
	defer cs.close()
	if cs.chunkSz != 4096 {
		t.Fatalf("chunkSz = %d, want 4096 (rounded down)", cs.chunkSz)
	}

	// Read every byte back through faults of assorted lengths and lengths that
	// straddle chunk boundaries, reassembling the image and comparing.
	got := make([]byte, 0, len(data))
	for off := uint64(0); off < uint64(len(data)); {
		b, err := cs.at(off, 4096)
		if err != nil {
			t.Fatal(err)
		}
		if len(b) == 0 {
			t.Fatalf("empty read at off=%d", off)
		}
		got = append(got, b...)
		off += uint64(len(b))
	}
	if !bytes.Equal(got, data) {
		t.Fatal("reassembled image != original")
	}
}

// TestGuestRegionJSON pins the wire field names Firecracker sends.
func TestGuestRegionJSON(t *testing.T) {
	const body = `[{"base_host_virt_addr":123456,"size":65536,"offset":4096,"page_size_kib":4}]`
	var regions []guestRegion
	if err := json.Unmarshal([]byte(body), &regions); err != nil {
		t.Fatal(err)
	}
	if len(regions) != 1 {
		t.Fatalf("got %d regions", len(regions))
	}
	r := regions[0]
	if r.BaseHostVirtAddr != 123456 || r.Size != 65536 || r.Offset != 4096 || r.PageSize != 4 {
		t.Fatalf("bad decode: %+v", r)
	}
}
