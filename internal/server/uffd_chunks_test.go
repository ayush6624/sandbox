package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
)

// TestBuildChunkManifest covers the write-path core: geometry, the all-zero
// sentinel, content-hash dedup of identical chunks, and a short last chunk.
func TestBuildChunkManifest(t *testing.T) {
	const chunkSz = 4096
	// 5 chunks: [data A][zeros][data A again][data B][short tail]
	a := bytes.Repeat([]byte{0xAB}, chunkSz)
	b := bytes.Repeat([]byte{0xCD}, chunkSz)
	tail := bytes.Repeat([]byte{0xEE}, 100)
	image := bytes.Join([][]byte{a, make([]byte, chunkSz), a, b, tail}, nil)

	path := filepath.Join(t.TempDir(), "mem")
	if err := os.WriteFile(path, image, 0o644); err != nil {
		t.Fatal(err)
	}

	m, comp, err := buildChunkManifest(path, chunkSz)
	if err != nil {
		t.Fatal(err)
	}
	if m.MemSize != uint64(len(image)) || m.ChunkSize != chunkSz || m.Codec != chunkCodecGzip {
		t.Fatalf("bad geometry: %+v", m)
	}
	if len(m.Chunks) != 5 {
		t.Fatalf("got %d chunks, want 5", len(m.Chunks))
	}
	// Chunk 1 is the zero sentinel.
	if m.Chunks[1].Hash != chunkZeroHash {
		t.Errorf("chunk 1 hash = %q, want zero sentinel", m.Chunks[1].Hash)
	}
	// Chunks 0 and 2 are identical data → same hash, and dedup to one comp entry.
	if m.Chunks[0].Hash != m.Chunks[2].Hash {
		t.Error("identical chunks 0 and 2 should share a hash")
	}
	if m.Chunks[0].Hash == m.Chunks[3].Hash {
		t.Error("different chunks 0 and 3 must not share a hash")
	}
	// comp holds exactly the unique non-zero chunks: A, B, and the tail (the
	// duplicate A dedups; the zero chunk is excluded).
	if len(comp) != 3 {
		t.Fatalf("comp has %d unique chunks, want 3 (A, B, tail; zero excluded, A deduped)", len(comp))
	}
	if _, ok := comp[chunkZeroHash]; ok {
		t.Error("zero sentinel must not be in the compressed set")
	}
	// chunkLen: last chunk is short.
	if got := m.chunkLen(4); got != 100 {
		t.Errorf("chunkLen(4) = %d, want 100", got)
	}
	if got := m.chunkLen(5); got != 0 {
		t.Errorf("chunkLen(past end) = %d, want 0", got)
	}
}

// TestGzipRoundTrip pins the codec helpers used on both ends of the chunk path.
func TestGzipRoundTrip(t *testing.T) {
	raw := bytes.Repeat([]byte("firecracker page bytes "), 1000)
	gz, err := gzipBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(gz) >= len(raw) {
		t.Errorf("gzip did not shrink repetitive data: %d >= %d", len(gz), len(raw))
	}
	back, err := gunzipBytes(gz)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, raw) {
		t.Fatal("gunzip(gzip(x)) != x")
	}
}

// TestNewChunkLoad covers the read-path core with a fake fetcher: zero sentinel,
// GCS fetch + decompress + write-through cache, warm cache hit (no re-fetch),
// past-image nil, and error propagation (the unservable-fault case).
func TestNewChunkLoad(t *testing.T) {
	const chunkSz = 4096
	cacheDir := t.TempDir()
	dataA := bytes.Repeat([]byte{0x11}, chunkSz)
	gzA, _ := gzipBytes(dataA)
	hashA := testChunkHash(dataA)

	m := &chunkManifest{
		Version: chunkManifestVersion, MemSize: chunkSz * 3, ChunkSize: chunkSz, Codec: chunkCodecGzip,
		Chunks: []chunkEntry{
			{Hash: hashA, CLen: len(gzA)},
			{Hash: chunkZeroHash},
			{Hash: "missing", CLen: 5},
		},
	}
	fetches := map[string]int{}
	fetch := func(hash string) ([]byte, error) {
		fetches[hash]++
		switch hash {
		case hashA:
			return gzA, nil
		default:
			return nil, errors.New("no such object")
		}
	}
	load := newChunkLoad(m, testChunkCache(t, cacheDir, defaultChunkCacheBytes), fetch)

	// Chunk 0: fetch + decompress + cache.
	got, err := load(0)
	if err != nil || !bytes.Equal(got, dataA) {
		t.Fatalf("load(0): err=%v equal=%v", err, bytes.Equal(got, dataA))
	}
	if _, err := os.Stat(filepath.Join(cacheDir, hashA)); err != nil {
		t.Errorf("chunk not written through to cache: %v", err)
	}
	// Second load hits the warm cache — no additional fetch.
	if _, err := load(0); err != nil {
		t.Fatal(err)
	}
	if fetches[hashA] != 1 {
		t.Errorf("hashA fetched %d times, want 1 (cache hit second time)", fetches[hashA])
	}
	// Chunk 1: zero sentinel → zeroed buffer, no fetch.
	z, err := load(1)
	if err != nil || len(z) != chunkSz || !allZero(z) {
		t.Fatalf("load(1) zero chunk: err=%v len=%d zero=%v", err, len(z), allZero(z))
	}
	// Chunk 2: fetch fails → error propagates (handler must kill the VM).
	if _, err := load(2); err == nil {
		t.Fatal("load(2) should error when fetch fails (unservable fault)")
	}
	// Past the image → nil, nil.
	if b, err := load(3); err != nil || b != nil {
		t.Fatalf("load(past end): b=%v err=%v", b, err)
	}
}

// TestNewChunkLoadConcurrentSameHash stresses the write-through cache with two
// chunk indices that dedup to one content hash, loaded concurrently (as prewarm
// does at high concurrency). The cached file must end up complete and correct,
// with no leftover temp files — the unique-temp-per-writer guarantee.
func TestNewChunkLoadConcurrentSameHash(t *testing.T) {
	cacheDir := t.TempDir()
	const chunkSz = 1 << 20 // 1 MiB — big enough that a torn write would be visible
	data := bytes.Repeat([]byte{0x5A}, chunkSz)
	gz, err := gzipBytes(data)
	if err != nil {
		t.Fatal(err)
	}
	hash := testChunkHash(data)
	// Two indices, one shared hash.
	m := &chunkManifest{
		Version: chunkManifestVersion, MemSize: chunkSz * 2, ChunkSize: chunkSz, Codec: chunkCodecGzip,
		Chunks: []chunkEntry{{Hash: hash, CLen: len(gz)}, {Hash: hash, CLen: len(gz)}},
	}
	fetch := func(string) ([]byte, error) { return gz, nil }
	load := newChunkLoad(m, testChunkCache(t, cacheDir, defaultChunkCacheBytes), fetch)

	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(2)
		go func() { defer wg.Done(); checkLoad(t, load, 0, data) }()
		go func() { defer wg.Done(); checkLoad(t, load, 1, data) }()
	}
	wg.Wait()

	// The published cache file is complete and correct (not a torn interleave).
	cached, err := os.ReadFile(filepath.Join(cacheDir, hash))
	if err != nil {
		t.Fatalf("cache file missing: %v", err)
	}
	if !bytes.Equal(cached, data) {
		t.Fatalf("cached chunk corrupt: len %d, want %d", len(cached), len(data))
	}
	// No leftover temp files.
	ents, _ := os.ReadDir(cacheDir)
	for _, e := range ents {
		if e.Name() != hash && e.Name() != ".lock" {
			t.Errorf("leftover temp file in cache dir: %s", e.Name())
		}
	}
}

func checkLoad(t *testing.T, load func(uint64) ([]byte, error), idx uint64, want []byte) {
	t.Helper()
	b, err := load(idx)
	if err != nil || !bytes.Equal(b, want) {
		t.Errorf("load(%d): err=%v equal=%v", idx, err, bytes.Equal(b, want))
	}
}

func TestRoundChunkSize(t *testing.T) {
	cases := []struct{ in, want uint64 }{
		{0, defaultChunkBytes}, // default
		{5000, 4096},           // rounds down to a page multiple
		{2 << 20, 2 << 20},     // already aligned
		{1, 4096},              // floored at one page
	}
	for _, c := range cases {
		if got := roundChunkSize(c.in); got != c.want {
			t.Errorf("roundChunkSize(%d) = %d, want %d", c.in, got, c.want)
		}
	}
}

func testChunkHash(raw []byte) string {
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])
}

func TestChunkLoadRejectsCorruption(t *testing.T) {
	raw := bytes.Repeat([]byte{17}, 4096)
	bad := bytes.Repeat([]byte{18}, 4096)
	hash := testChunkHash(raw)
	goodGzip, _ := gzipBytes(raw)
	badGzip, _ := gzipBytes(bad)
	m := &chunkManifest{Version: 1, Codec: "gzip", MemSize: 4096, ChunkSize: 4096,
		Chunks: []chunkEntry{{Hash: hash, CLen: len(goodGzip)}}}
	cache := t.TempDir()
	if err := os.WriteFile(filepath.Join(cache, hash), bad, 0600); err != nil {
		t.Fatal(err)
	}
	fetches := 0
	load := newChunkLoad(m, testChunkCache(t, cache, defaultChunkCacheBytes), func(string) ([]byte, error) { fetches++; return goodGzip, nil })
	got, err := load(0)
	if err != nil || !bytes.Equal(got, raw) || fetches != 1 {
		t.Fatalf("corrupt cache was not repaired: fetches=%d err=%v", fetches, err)
	}
	load = newChunkLoad(m, testChunkCache(t, t.TempDir(), defaultChunkCacheBytes), func(string) ([]byte, error) { return badGzip, nil })
	if _, err := load(0); err == nil {
		t.Fatal("same-size corrupt object accepted")
	}
	oversized, _ := gzipBytes(append(raw, 1))
	load = newChunkLoad(m, testChunkCache(t, t.TempDir(), defaultChunkCacheBytes), func(string) ([]byte, error) { return oversized, nil })
	if _, err := load(0); err == nil {
		t.Fatal("oversized object accepted")
	}
}

func TestChunkManifestValidation(t *testing.T) {
	valid := func() chunkManifest {
		return chunkManifest{Version: 1, Codec: "gzip", MemSize: 8192, ChunkSize: 4096,
			Chunks: []chunkEntry{{Hash: testChunkHash(make([]byte, 4096)), CLen: 40}, {Hash: chunkZeroHash}}}
	}
	m := valid()
	if err := m.validate(); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*chunkManifest){
		"version":        func(m *chunkManifest) { m.Version++ },
		"codec":          func(m *chunkManifest) { m.Codec = "raw" },
		"empty":          func(m *chunkManifest) { m.MemSize = 0 },
		"alignment":      func(m *chunkManifest) { m.ChunkSize = 4095 },
		"missing chunk":  func(m *chunkManifest) { m.Chunks = m.Chunks[:1] },
		"extra chunk":    func(m *chunkManifest) { m.Chunks = append(m.Chunks, chunkEntry{Hash: chunkZeroHash}) },
		"path traversal": func(m *chunkManifest) { m.Chunks[0].Hash = "../memory" },
		"zero payload":   func(m *chunkManifest) { m.Chunks[1].CLen = 1 },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			m := valid()
			mutate(&m)
			if m.validate() == nil {
				t.Fatal("invalid manifest accepted")
			}
		})
	}
}

func TestHibChunkSourceSurvivesRequestAndManifestReplacement(t *testing.T) {
	store := newUploadTestStore(t)
	s, _ := testLifecycleServer(t)
	s.blob = store.client
	first := bytes.Repeat([]byte{31}, 4096)
	second := bytes.Repeat([]byte{32}, 4096)
	hash := testChunkHash(first)
	gz, _ := gzipBytes(first)
	m := chunkManifest{Version: 1, Codec: "gzip", MemSize: 4096, ChunkSize: 4096,
		Chunks: []chunkEntry{{Hash: hash, CLen: len(gz)}}}
	ctx := context.Background()
	data, _ := json.Marshal(m)
	if err := store.client.PutBytes(ctx, hibManifestObj("frozen"), data); err != nil {
		t.Fatal(err)
	}
	if err := store.client.PutBytes(ctx, chunkObj(hash), gz); err != nil {
		t.Fatal(err)
	}
	request, cancel := context.WithCancel(ctx)
	source, err := s.loadHibChunkSource(request, "frozen")
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	m.Chunks[0].Hash = testChunkHash(second)
	data, _ = json.Marshal(m)
	if err := store.client.PutBytes(ctx, hibManifestObj("frozen"), data); err != nil {
		t.Fatal(err)
	}
	got, err := source.Load(0)
	if err != nil || !bytes.Equal(got, first) {
		t.Fatalf("live source changed generation or used expired request: %v", err)
	}
}
