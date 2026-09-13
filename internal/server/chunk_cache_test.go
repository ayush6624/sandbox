package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func testChunkCache(t *testing.T, dir string, limit int64) *chunkCache {
	t.Helper()
	c := newChunkCache(dir, limit)
	t.Cleanup(c.close)
	return c
}

func cacheTestManifest(raw ...[]byte) *chunkManifest {
	m := &chunkManifest{Version: chunkManifestRawVersion, Codec: chunkCodecRawSHA256, ChunkSize: 4096}
	for _, b := range raw {
		m.MemSize += uint64(len(b))
		m.Chunks = append(m.Chunks, chunkEntry{Hash: testChunkHash(b)})
	}
	return m
}

func assertCacheFiles(t *testing.T, c *chunkCache) {
	t.Helper()
	files, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	var payload, allocated int64
	for _, f := range files {
		if f.Name() == ".lock" {
			continue
		}
		if !cacheHash(f.Name()) {
			t.Fatalf("unexpected cache artifact: %s", f.Name())
		}
		info, err := f.Info()
		if err != nil {
			t.Fatal(err)
		}
		payload += info.Size()
		allocated += fileAllocatedBytes(info)
	}
	st := c.stats()
	if payload != st.Resident || allocated != st.Allocated || st.Resident+st.Reserved+st.Residue > st.Limit {
		t.Fatalf("disk payload=%d allocated=%d, stats=%+v", payload, allocated, st)
	}
}

func TestChunkCacheRepeatedEvictionPreservesCapturedLoader(t *testing.T) {
	c := testChunkCache(t, t.TempDir(), 8192)
	first := bytes.Repeat([]byte{42}, 4096)
	m := cacheTestManifest(first)
	compressed, _ := gzipBytes(first)
	fetches := 0
	load := newChunkLoad(m, c, func(string) ([]byte, error) { fetches++; return compressed, nil })
	checkLoad(t, load, 0, first)
	for i := range 100 {
		raw := bytes.Repeat([]byte{byte(i + 100)}, 4096)
		c.put(testChunkHash(raw), raw)
		assertCacheFiles(t, c)
	}
	if c.get(m.Chunks[0].Hash, 4096) != nil {
		t.Fatal("unused first chunk was never reclaimed")
	}
	checkLoad(t, load, 0, first)
	if fetches != 2 || c.stats().Reclaimed < 99*4096 {
		t.Fatalf("late fault or repeated reclamation failed: fetches=%d stats=%+v", fetches, c.stats())
	}
	assertCacheFiles(t, c)
}

func TestChunkCacheHydrationReservationsShareBytesAndPinThroughAck(t *testing.T) {
	a, b, other := bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096), bytes.Repeat([]byte{3}, 4096)
	c := testChunkCache(t, t.TempDir(), 8192)
	release, err := c.reserve(cacheTestManifest(a, a, b))
	if err != nil {
		t.Fatal(err)
	}
	secondRelease, err := c.reserve(cacheTestManifest(a))
	if err != nil || c.stats().Reserved != 8192 {
		t.Fatalf("shared reservation = %+v, %v", c.stats(), err)
	}
	if _, err := c.reserve(cacheTestManifest(other)); err == nil {
		t.Fatal("overlapping full-budget reservation succeeded")
	}
	c.put(testChunkHash(a), a)
	c.put(testChunkHash(b), b)
	c.put(testChunkHash(other), other)
	if c.get(testChunkHash(a), 4096) == nil || c.get(testChunkHash(b), 4096) == nil || c.get(testChunkHash(other), 4096) != nil {
		t.Fatal("pressure evicted a pinned hydration chunk")
	}
	release()
	release()
	c.put(testChunkHash(other), other)
	if c.get(testChunkHash(a), 4096) == nil || c.get(testChunkHash(other), 4096) == nil {
		t.Fatal("shared reservation was released by the first hydration")
	}
	secondRelease()
	assertCacheFiles(t, c)
}

func TestChunkCacheStartupTrimsAndRemovesAbandonedWrites(t *testing.T) {
	dir := t.TempDir()
	for i := range 3 {
		raw := bytes.Repeat([]byte{byte(i + 1)}, 4096)
		path := filepath.Join(dir, testChunkHash(raw))
		if err := os.WriteFile(path, raw, 0600); err != nil {
			t.Fatal(err)
		}
		stamp := time.Unix(int64(i+1), 0)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}
	tmp := filepath.Join(dir, "."+testChunkHash([]byte("abandoned"))+".tmp-old")
	if err := os.WriteFile(tmp, []byte("unfinished"), 0600); err != nil {
		t.Fatal(err)
	}
	c := testChunkCache(t, dir, 4096)
	if st := c.stats(); st.Resident != 4096 || st.Reclaimed != 8192+10 {
		t.Fatalf("startup trim = %+v", st)
	}
	if c.get(testChunkHash(bytes.Repeat([]byte{3}, 4096)), 4096) == nil {
		t.Fatal("startup did not retain newest file")
	}
	assertCacheFiles(t, c)
}

func TestChunkCacheExclusiveOwnerAndLateWriteAfterClose(t *testing.T) {
	dir := t.TempDir()
	first := testChunkCache(t, dir, 4096)
	second := testChunkCache(t, dir, 4096)
	if second.stats().Ready != 0 {
		t.Fatal("second cache acquired the directory")
	}
	first.close()
	third := testChunkCache(t, dir, 4096)
	a, b := bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096)
	third.put(testChunkHash(a), a)
	first.put(testChunkHash(b), b)
	second.put(testChunkHash(b), b)
	if third.get(testChunkHash(a), 4096) == nil || third.get(testChunkHash(b), 4096) != nil {
		t.Fatal("closed or non-owning cache changed the new owner's files")
	}
	assertCacheFiles(t, third)
}

func TestChunkCacheOversizedLoadBypassesDisk(t *testing.T) {
	raw := bytes.Repeat([]byte{12}, 4096)
	c := testChunkCache(t, t.TempDir(), 2048)
	compressed, _ := gzipBytes(raw)
	load := newChunkLoad(cacheTestManifest(raw), c, func(string) ([]byte, error) { return compressed, nil })
	checkLoad(t, load, 0, raw)
	if st := c.stats(); st.Resident != 0 || st.Bypassed != 1 {
		t.Fatalf("oversized admission = %+v", st)
	}
	assertCacheFiles(t, c)
}

func TestChunkCacheConcurrentLoadsStayWithinBudget(t *testing.T) {
	c := testChunkCache(t, t.TempDir(), 16384)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			raw := bytes.Repeat([]byte{byte(i + 1)}, 4096)
			compressed, _ := gzipBytes(raw)
			load := newChunkLoad(cacheTestManifest(raw), c, func(string) ([]byte, error) { return compressed, nil })
			for range 10 {
				checkLoad(t, load, 0, raw)
			}
		}()
	}
	wg.Wait()
	assertCacheFiles(t, c)
}

func TestPeerHydrationInsufficientCacheKeepsFallback(t *testing.T) {
	a, b := bytes.Repeat([]byte{11}, 4096), bytes.Repeat([]byte{12}, 4096)
	f := newHibPeerClientFixture(t, a, b)
	f.server.cfg.UFFDChunkCacheBytes = 4096
	p, err := f.server.openHibernationPeer(context.Background(), &f.record)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(f.server.memoryChunkCache().close)
	if err := p.hydrateAndAcknowledge(context.Background()); err == nil {
		t.Fatal("oversized hydration acknowledged")
	}
	checkLoad(t, p.load, 0, a)
	checkLoad(t, p.load, 1, b)
	checkLoad(t, p.load, 0, a)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acks != 0 || f.deleted {
		t.Fatal("cache pressure discarded peer fallback")
	}
	assertCacheFiles(t, f.server.memoryChunkCache())
}

func TestChunkCacheFailedEvictionRemainsCounted(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root bypasses directory write permissions")
	}
	dir := t.TempDir()
	c := testChunkCache(t, dir, 4096)
	a, b := bytes.Repeat([]byte{1}, 4096), bytes.Repeat([]byte{2}, 4096)
	c.put(testChunkHash(a), a)
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0700) })
	c.put(testChunkHash(b), b)
	if st := c.stats(); st.Resident != 4096 || st.Reclaimed != 0 || st.Bypassed != 1 || st.Errors == 0 {
		t.Fatalf("failed unlink lost accounting: %+v", st)
	}
	assertCacheFiles(t, c)
}

func TestChunkCacheFailedRenameRemovesTemporaryFile(t *testing.T) {
	c := testChunkCache(t, t.TempDir(), 4096)
	raw := bytes.Repeat([]byte{4}, 4096)
	hash := testChunkHash(raw)
	if err := os.Mkdir(filepath.Join(c.dir, hash), 0700); err != nil {
		t.Fatal(err)
	}
	c.put(hash, raw)
	files, err := os.ReadDir(c.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if cacheTemp(f.Name()) {
			t.Fatalf("failed rename left temporary file %s", f.Name())
		}
	}
	if st := c.stats(); st.Resident != 0 || st.Reserved != 0 || st.Errors != 1 {
		t.Fatalf("failed write accounting = %+v", st)
	}
}

func TestChunkCacheMetricsMatchReclaimedFiles(t *testing.T) {
	s := metricsTestServer(t)
	s.cfg.UFFDChunkCacheBytes = 4096
	c := s.memoryChunkCache()
	for i := range 2 {
		raw := bytes.Repeat([]byte{byte(i + 1)}, 4096)
		c.put(testChunkHash(raw), raw)
	}
	w := httptest.NewRecorder()
	s.handleMetrics(w, httptest.NewRequest("GET", "/metrics", nil))
	m := parseMetrics(t, w.Body.String())
	for key, want := range map[string]int64{
		"sandbox_chunk_cache_limit_bytes":           4096,
		"sandbox_chunk_cache_resident_bytes":        4096,
		"sandbox_chunk_cache_reclaimed_bytes_total": 4096,
		"sandbox_chunk_cache_reserved_bytes":        0,
		"sandbox_chunk_cache_ready":                 1,
	} {
		if got := m[key]; got != want {
			t.Errorf("%s = %d, want %d", key, got, want)
		}
	}
	assertCacheFiles(t, c)
}

func TestPeerHydrationPinsUntilAcknowledgmentThenRefetches(t *testing.T) {
	a, b := bytes.Repeat([]byte{21}, 4096), bytes.Repeat([]byte{22}, 4096)
	f := newHibPeerClientFixture(t, a, b)
	f.server.cfg.UFFDChunkCacheBytes = 8192
	atAck, allowAck := make(chan struct{}), make(chan struct{})
	var once sync.Once
	releaseAck := func() { once.Do(func() { close(allowAck) }) }
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			close(atAck)
			<-allowAck
		}
		f.serveHTTP(w, r)
	}))
	t.Cleanup(peer.Close)
	t.Cleanup(releaseAck)
	f.record.Peer.URL = peer.URL
	f.descriptor.Ref.URL = peer.URL
	p, err := f.server.openHibernationPeer(context.Background(), &f.record)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.hydrateAndAcknowledge(ctx) }()
	select {
	case <-atAck:
	case err := <-done:
		t.Fatalf("hydration never reached acknowledgment: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c := f.server.memoryChunkCache()
	for i := range 10 {
		raw := bytes.Repeat([]byte{byte(i + 50)}, 4096)
		c.put(testChunkHash(raw), raw)
	}
	if c.get(testChunkHash(a), 4096) == nil || c.get(testChunkHash(b), 4096) == nil {
		t.Fatal("pressure invalidated an in-flight acknowledgment")
	}
	releaseAck()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		raw := bytes.Repeat([]byte{byte(i + 80)}, 4096)
		c.put(testChunkHash(raw), raw)
	}
	if c.get(testChunkHash(a), 4096) != nil || c.get(testChunkHash(b), 4096) != nil {
		t.Fatal("completed acknowledgment left permanent cache pins")
	}
	checkLoad(t, p.load, 0, a)
	checkLoad(t, p.load, 1, b)
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.acks != 1 || !f.deleted {
		t.Fatal("cloud fallback was not checked after peer retirement")
	}
	assertCacheFiles(t, c)
}

func TestChunkCacheCloseDuringFetchCannotWriteUnderNewOwner(t *testing.T) {
	dir := t.TempDir()
	old := testChunkCache(t, dir, 4096)
	raw := bytes.Repeat([]byte{8}, 4096)
	compressed, _ := gzipBytes(raw)
	entered, finish := make(chan struct{}), make(chan struct{})
	load := newChunkLoad(cacheTestManifest(raw), old, func(string) ([]byte, error) {
		close(entered)
		<-finish
		return compressed, nil
	})
	done := make(chan struct{})
	go func() { defer close(done); checkLoad(t, load, 0, raw) }()
	<-entered
	old.close()
	current := testChunkCache(t, dir, 4096)
	close(finish)
	<-done
	if current.stats().Resident != 0 {
		t.Fatal("late fetch populated a closed cache")
	}
	assertCacheFiles(t, current)
}

func TestPeerHydrationPendingBackupRetainsReservation(t *testing.T) {
	a, b := bytes.Repeat([]byte{31}, 4096), bytes.Repeat([]byte{32}, 4096)
	f := newHibPeerClientFixture(t, a, b)
	f.server.cfg.UFFDChunkCacheBytes = 8192
	f.record.Generation = f.record.Peer.Generation
	f.descriptor.Record.Generation = f.record.Generation
	f.store.mu.Lock()
	f.store.objects = map[string]uploadTestObject{}
	f.store.mu.Unlock()
	p, err := f.server.openHibernationPeer(context.Background(), &f.record)
	if err != nil {
		t.Fatal(err)
	}
	waiting := make(chan struct{})
	var once sync.Once
	f.store.mu.Lock()
	f.store.beforeGet = func(_ *http.Request, name string) {
		if name == p.backupObject {
			once.Do(func() { close(waiting) })
		}
	}
	f.store.mu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- p.hydrateAndAcknowledge(ctx) }()
	select {
	case <-waiting:
	case err := <-done:
		t.Fatalf("hydration did not wait for backup: %v", err)
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	c := f.server.memoryChunkCache()
	for i := range 3 {
		raw := bytes.Repeat([]byte{byte(i + 100)}, 4096)
		c.put(testChunkHash(raw), raw)
	}
	f.mu.Lock()
	acks := f.acks
	f.mu.Unlock()
	if acks != 0 || c.get(testChunkHash(a), 4096) == nil || c.get(testChunkHash(b), 4096) == nil {
		t.Fatal("pending backup lost its complete reserved image or acknowledged early")
	}
	for _, raw := range [][]byte{a, b} {
		compressed, _ := gzipBytes(raw)
		if err := f.store.client.PutBytes(ctx, chunkObj(testChunkHash(raw)), compressed); err != nil {
			t.Fatal(err)
		}
	}
	record, _ := json.Marshal(f.descriptor)
	if err := f.store.client.PutBytes(ctx, p.backupObject, record); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		raw := bytes.Repeat([]byte{byte(i + 110)}, 4096)
		c.put(testChunkHash(raw), raw)
	}
	if c.get(testChunkHash(a), 4096) != nil || c.get(testChunkHash(b), 4096) != nil {
		t.Fatal("durable backup left permanent cache reservations")
	}
	checkLoad(t, p.load, 0, a)
	checkLoad(t, p.load, 1, b)
	assertCacheFiles(t, c)
}

func TestPeerHydrationUnconfirmedBackupDoesNotAcknowledge(t *testing.T) {
	for _, mode := range []string{"cancelled", "mismatched"} {
		t.Run(mode, func(t *testing.T) {
			raw := bytes.Repeat([]byte{33}, 4096)
			f := newHibPeerClientFixture(t, raw)
			f.server.cfg.UFFDChunkCacheBytes = 4096
			f.record.Generation = f.record.Peer.Generation
			f.descriptor.Record.Generation = f.record.Generation
			p, err := f.server.openHibernationPeer(context.Background(), &f.record)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancelled" {
				f.store.mu.Lock()
				f.store.beforeGet = func(_ *http.Request, name string) {
					if name == p.backupObject {
						cancel()
					}
				}
				f.store.mu.Unlock()
			} else if err := f.store.client.PutBytes(ctx, p.backupObject, []byte(`{}`)); err != nil {
				t.Fatal(err)
			}
			if err := p.hydrateAndAcknowledge(ctx); err == nil {
				t.Fatal("unconfirmed backup acknowledged")
			}
			f.mu.Lock()
			acks := f.acks
			f.mu.Unlock()
			if acks != 0 {
				t.Fatal("unconfirmed backup set cache-ready")
			}
			c := f.server.memoryChunkCache()
			other := bytes.Repeat([]byte{34}, 4096)
			c.put(testChunkHash(other), other)
			if c.get(testChunkHash(other), 4096) == nil {
				t.Fatal("failed hydration leaked its reservation")
			}
			assertCacheFiles(t, c)
		})
	}
}
