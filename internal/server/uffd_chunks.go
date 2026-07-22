package server

// GCS-backed chunk memory source for UFFD wake (roadmap Phase B2, design in
// docs/uffd-b2-design.md). At a FULL hibernation freeze the mem image is split
// into fixed-size, content-addressed, gzip-compressed chunks and uploaded to the
// snapshot bucket alongside a positional manifest; a same-identity UFFD wake then
// faults pages lazily from a local chunk cache → GCS instead of the local mem
// file, so wake I/O tracks the working set and works even off the creating host.
//
// Bucket layout (additive to snapshot_gcs.go):
//
//	chunks/<sha256-hex>            immutable, gzip-compressed chunk, SHARED across all VMs (dedup/CoW)
//	hib/<id>/manifest.json         ordered chunk list + geometry; written LAST as the commit marker
//
// The manifest's chunk array is positional: index i backs image bytes
// [i*chunk_size, …). All-zero chunks use a reserved sentinel hash and are never
// stored or fetched (the fresh-guest zero regions are huge). Chunk objects leak
// on snapshot delete for now (GC is a B4/ops follow-up) — the upload logs the
// dedup ratio so that's visible.

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/ayush6624/sandbox/internal/vm"
)

const (
	chunkManifestVersion = 1
	chunkCodecGzip       = "gzip"
	// chunkZeroHash marks an all-zero chunk: never uploaded, never fetched, served
	// as a freshly-zeroed buffer. Not a valid sha256 hex, so it can't collide with
	// a real content hash.
	chunkZeroHash = "zero"
	// defaultChunkBytes sizes chunks when UFFDChunkBytes is unset. 2 MiB: few
	// enough objects, good gzip ratio, still 512× the 4 KiB page so boundary-clamp
	// waste is negligible.
	defaultChunkBytes    = 2 << 20
	defaultChunkPrefetch = 4
)

func chunkObj(hash string) string       { return "chunks/" + hash }
func hibManifestObj(id string) string   { return "hib/" + id + "/manifest.json" }
func hibWorkingSetObj(id string) string { return "hib/" + id + "/workingset.json" }

// chunkEntry is one manifest entry: the content hash of chunk i and its
// compressed object size (for logging/prefetch budgeting). CLen is 0 for a zero
// chunk.
type chunkEntry struct {
	Hash string `json:"hash"`
	CLen int    `json:"clen"`
}

// chunkManifest is the geometry + chunk list for one hibernation mem image.
type chunkManifest struct {
	Version   int          `json:"version"`
	MemSize   uint64       `json:"mem_size"`
	ChunkSize uint64       `json:"chunk_size"`
	Codec     string       `json:"codec"`
	Chunks    []chunkEntry `json:"chunks"`
}

// chunkLen returns the byte length of chunk idx (ChunkSize, or short for the
// last chunk); 0 if idx is past the image.
func (m *chunkManifest) chunkLen(idx uint64) uint64 {
	start := idx * m.ChunkSize
	if start >= m.MemSize {
		return 0
	}
	if n := m.MemSize - start; n < m.ChunkSize {
		return n
	}
	return m.ChunkSize
}

// roundChunkSize forces a page-multiple chunk size (a boundary-clamped run must
// stay a whole number of pages — see chunkedSource) with a 2 MiB default.
func roundChunkSize(b uint64) uint64 {
	if b == 0 {
		b = defaultChunkBytes
	}
	b &^= 4096 - 1
	if b < 4096 {
		b = 4096
	}
	return b
}

func allZero(b []byte) bool {
	for _, x := range b {
		if x != 0 {
			return false
		}
	}
	return true
}

func gzipBytes(raw []byte) ([]byte, error) {
	var b bytes.Buffer
	zw, err := gzip.NewWriterLevel(&b, gzip.BestSpeed)
	if err != nil {
		return nil, err
	}
	if _, err := zw.Write(raw); err != nil {
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func gunzipBytes(comp []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(comp))
	if err != nil {
		return nil, err
	}
	defer zr.Close()
	return io.ReadAll(zr)
}

// buildChunkManifest walks memPath in chunkSz strides, producing the manifest and
// the compressed bytes of each unique non-zero chunk (keyed by content hash, so
// duplicate chunks compress once). Pure (no GCS) — the unit-tested core of the
// write path.
func buildChunkManifest(memPath string, chunkSz uint64) (*chunkManifest, map[string][]byte, error) {
	f, err := os.Open(memPath)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, nil, err
	}
	total := uint64(fi.Size())
	m := &chunkManifest{
		Version:   chunkManifestVersion,
		MemSize:   total,
		ChunkSize: chunkSz,
		Codec:     chunkCodecGzip,
	}
	comp := map[string][]byte{}
	buf := make([]byte, chunkSz)
	for start := uint64(0); start < total; start += chunkSz {
		n := chunkSz
		if n > total-start {
			n = total - start
		}
		if _, err := io.ReadFull(f, buf[:n]); err != nil {
			return nil, nil, fmt.Errorf("read chunk @%d: %w", start, err)
		}
		raw := buf[:n]
		if allZero(raw) {
			m.Chunks = append(m.Chunks, chunkEntry{Hash: chunkZeroHash})
			continue
		}
		sum := sha256.Sum256(raw)
		hash := hex.EncodeToString(sum[:])
		if _, ok := comp[hash]; !ok {
			gz, err := gzipBytes(raw) // gz is an independent buffer; safe though buf is reused
			if err != nil {
				return nil, nil, fmt.Errorf("gzip chunk @%d: %w", start, err)
			}
			comp[hash] = gz
		}
		m.Chunks = append(m.Chunks, chunkEntry{Hash: hash, CLen: len(comp[hash])})
	}
	return m, comp, nil
}

// chunkPresent reports whether a chunk object is already durable — the local
// known-set first (no round-trip), then a bucket Exists check that also warms
// the set.
func (s *Server) chunkPresent(ctx context.Context, hash string) bool {
	s.chunkUpMu.Lock()
	known := s.chunksUploaded[hash]
	s.chunkUpMu.Unlock()
	if known {
		return true
	}
	if ok, err := s.blob.Exists(ctx, chunkObj(hash)); err == nil && ok {
		s.markChunkUploaded(hash)
		return true
	}
	return false
}

func (s *Server) markChunkUploaded(hash string) {
	s.chunkUpMu.Lock()
	s.chunksUploaded[hash] = true
	s.chunkUpMu.Unlock()
}

// uploadHibChunks chunks a full hibernation mem image and ships the missing
// chunks + the manifest to GCS in the background (manifest last, as the commit
// marker). Failures log and leave the sandbox host-local-only — local wake still
// works — so this is purely additive durability.
func (s *Server) uploadHibChunks(id, memPath string, chunkSz uint64, workingSet []uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	t0 := time.Now()

	m, comp, err := buildChunkManifest(memPath, chunkSz)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] chunk upload aborted: build manifest: %v\n", id, err)
		return
	}
	// Persist the recorded working set (chunk indices the guest faulted last wake)
	// so the next wake can prewarm it. Uploaded before the manifest; a missing/
	// stale working set just means no prewarm (correctness unaffected). Roadmap B3.
	if len(workingSet) > 0 {
		if wsBytes, werr := json.Marshal(workingSet); werr == nil {
			if werr = s.blob.PutBytes(ctx, hibWorkingSetObj(id), wsBytes); werr != nil {
				fmt.Fprintf(os.Stderr, "[%s] working-set upload failed (no prewarm next wake): %v\n", id, werr)
			}
		}
	}
	var uploaded, skipped int
	var upBytes int64
	for hash, gz := range comp {
		if s.chunkPresent(ctx, hash) {
			skipped++
			continue
		}
		if err := s.blob.PutBytes(ctx, chunkObj(hash), gz); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] chunk upload aborted at %s: %v\n", id, hash[:12], err)
			return
		}
		s.markChunkUploaded(hash)
		uploaded++
		upBytes += int64(len(gz))
	}
	meta, err := json.Marshal(m)
	if err == nil {
		err = s.blob.PutBytes(ctx, hibManifestObj(id), meta)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] chunk upload: write manifest: %v\n", id, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[%s] chunk-uploaded to gs://%s: %d chunks (%d new, %d deduped) %dMiB in %s\n",
		id, s.blob.Bucket(), len(m.Chunks), uploaded, skipped, upBytes>>20, time.Since(t0).Round(time.Millisecond))
}

// fetchChunkManifest pulls the chunk manifest for a hibernated sandbox. Absent
// manifest (never uploaded, or a diff freeze) surfaces as an error so the wake
// falls back to the local mem file.
func (s *Server) fetchChunkManifest(ctx context.Context, id string) (*chunkManifest, error) {
	b, err := s.blob.GetBytes(ctx, hibManifestObj(id))
	if err != nil {
		return nil, err
	}
	var m chunkManifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("decode chunk manifest: %w", err)
	}
	if m.ChunkSize == 0 || m.MemSize == 0 {
		return nil, fmt.Errorf("chunk manifest for %s is degenerate (size=%d chunk=%d)", id, m.MemSize, m.ChunkSize)
	}
	return &m, nil
}

// fetchWorkingSet pulls the persisted working set (chunk indices from the last
// wake) to prewarm this one. Best-effort: a missing set (first wake) or any error
// returns nil (no prewarm). Indices past the current chunk count are dropped, so
// a changed image size can't feed prewarm an out-of-range chunk.
func (s *Server) fetchWorkingSet(ctx context.Context, id string, numChunks uint64) []uint64 {
	b, err := s.blob.GetBytes(ctx, hibWorkingSetObj(id))
	if err != nil {
		return nil
	}
	var ws []uint64
	if err := json.Unmarshal(b, &ws); err != nil {
		return nil
	}
	out := ws[:0]
	for _, idx := range ws {
		if idx < numChunks {
			out = append(out, idx)
		}
	}
	return out
}

// chunkCacheDir is the host-local cache of materialized (decompressed) chunks,
// content-addressed so it's shared across VMs. Under SnapshotDir to share the
// XFS reflink domain. No eviction yet (roadmap: B4/ops).
func (s *Server) chunkCacheDir() string {
	return filepath.Join(s.cfg.Provisioner.SnapshotDir, "chunkcache")
}

// newChunkLoad is the testable core of the GCS load path: given the manifest, a
// cache dir, and a fetch(hash) that returns the COMPRESSED chunk object, it
// returns a load(idx) that serves a decompressed chunk from local cache →
// fetch, write-through caching each miss. The zero sentinel is served directly.
// A fetch/decompress error propagates so the UFFD handler kills the VM rather
// than hang on an unserved fault.
func newChunkLoad(m *chunkManifest, cacheDir string, fetch func(hash string) ([]byte, error)) func(uint64) ([]byte, error) {
	return func(idx uint64) ([]byte, error) {
		if idx >= uint64(len(m.Chunks)) {
			return nil, nil // past the image
		}
		clen := m.chunkLen(idx)
		e := m.Chunks[idx]
		if e.Hash == chunkZeroHash {
			return make([]byte, clen), nil
		}
		cpath := filepath.Join(cacheDir, e.Hash)
		if raw, err := os.ReadFile(cpath); err == nil && uint64(len(raw)) == clen {
			return raw, nil // warm: cache holds decompressed bytes, no gunzip
		}
		comp, err := fetch(e.Hash)
		if err != nil {
			return nil, fmt.Errorf("fetch chunk %d (%s): %w", idx, e.Hash, err)
		}
		raw, err := gunzipBytes(comp)
		if err != nil {
			return nil, fmt.Errorf("decompress chunk %d (%s): %w", idx, e.Hash, err)
		}
		if uint64(len(raw)) != clen {
			return nil, fmt.Errorf("chunk %d (%s) is %d bytes, manifest says %d", idx, e.Hash, len(raw), clen)
		}
		// Write-through cache, best-effort (tmp+rename so a crash never leaves a
		// truncated chunk). A cache write failure is non-fatal — the fault is
		// already served from raw.
		if err := os.MkdirAll(cacheDir, 0o755); err == nil {
			tmp := cpath + ".tmp"
			if os.WriteFile(tmp, raw, 0o644) == nil {
				_ = os.Rename(tmp, cpath)
			}
		}
		return raw, nil
	}
}

// gcsChunkSource builds the vm.UFFDChunkSource for a hibernated sandbox from its
// GCS manifest, or returns nil (with a reason logged by the caller) when chunks
// aren't available — the wake then falls back to the local mem file.
func (s *Server) gcsChunkSource(ctx context.Context, id string) *vm.UFFDChunkSource {
	if s.blob == nil {
		return nil
	}
	m, err := s.fetchChunkManifest(ctx, id)
	if err != nil {
		return nil
	}
	prefetch := s.cfg.UFFDChunkPrefetch
	if prefetch <= 0 {
		prefetch = defaultChunkPrefetch
	}
	fetch := func(hash string) ([]byte, error) {
		fctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.blob.GetBytes(fctx, chunkObj(hash))
	}
	return &vm.UFFDChunkSource{
		Total:     m.MemSize,
		ChunkSize: m.ChunkSize,
		Prefetch:  uint64(prefetch),
		Load:      newChunkLoad(m, s.chunkCacheDir(), fetch),
		Prewarm:   s.fetchWorkingSet(ctx, id, uint64(len(m.Chunks))),
	}
}
