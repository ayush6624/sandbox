package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/management"
	"github.com/google/uuid"
)

type hibPeerClientFixture struct {
	mu                     sync.Mutex
	descriptor             retainedHibernation
	chunks                 map[string][]byte
	artifacts              map[string][]byte
	deleted                bool
	chunkRequests          int
	transientChunkFailures int
	acks                   int
	ackBeforeCache         bool
	server                 *Server
	store                  *uploadTestStore
	record                 hibRecord
}

func newHibPeerClientFixture(t *testing.T, pages ...[]byte) *hibPeerClientFixture {
	t.Helper()
	s, _ := testLifecycleServer(t)
	t.Cleanup(func() {
		if s.chunkCache != nil {
			s.chunkCache.close()
		}
	})
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	store := newUploadTestStore(t)
	s.blob = store.client
	creds, err := management.NewCredentials([]string{"hib-peer-test"}, "")
	if err != nil {
		t.Fatal(err)
	}
	s.workerCredentials = creds
	f := &hibPeerClientFixture{server: s, store: store, chunks: map[string][]byte{}, artifacts: map[string][]byte{}}
	m := chunkManifest{Version: 1, Codec: "gzip", MemSize: uint64(len(pages)) * 4096, ChunkSize: 4096}
	for _, page := range pages {
		if len(page) != 4096 {
			t.Fatal("fixture page must be 4096 bytes")
		}
		hash := testChunkHash(page)
		if bytes.Equal(page, make([]byte, 4096)) {
			m.Chunks = append(m.Chunks, chunkEntry{Hash: chunkZeroHash})
			continue
		}
		gz, err := gzipBytes(page)
		if err != nil {
			t.Fatal(err)
		}
		m.Chunks = append(m.Chunks, chunkEntry{Hash: hash, CLen: len(gz)})
		f.chunks[hash] = append([]byte(nil), page...)
		if err := store.client.PutBytes(context.Background(), chunkObj(hash), gz); err != nil {
			t.Fatal(err)
		}
	}
	peer := httptest.NewServer(bearerAuth(nil, creds, http.HandlerFunc(f.serveHTTP)))
	t.Cleanup(peer.Close)
	digest, err := hibPeerManifestDigest(&m)
	if err != nil {
		t.Fatal(err)
	}
	ref := hibPeerRef{URL: peer.URL, Generation: uuid.NewString(), ManifestSHA256: digest, ExpiresAtUnix: time.Now().Add(time.Hour).Unix()}
	f.record = hibRecord{ID: uuid.NewString(), MemForm: memFormChunked, RootfsForm: rootfsFormFull, Peer: &ref}
	f.descriptor = retainedHibernation{Record: f.record, Manifest: m, Ref: ref}
	f.descriptor.Record.Peer = nil
	return f
}

func (f *hibPeerClientFixture) serveHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if r.Method == http.MethodDelete {
		f.acks++
		for i, entry := range f.descriptor.Manifest.Chunks {
			if entry.Hash == chunkZeroHash {
				continue
			}
			raw, err := os.ReadFile(filepath.Join(f.server.chunkCacheDir(), entry.Hash))
			if err != nil || !chunkMatches(raw, f.descriptor.Manifest.chunkLen(uint64(i)), entry.Hash) {
				f.ackBeforeCache = true
			}
		}
		f.deleted = true
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if f.deleted {
		http.NotFound(w, r)
		return
	}
	if _, hash, ok := strings.Cut(r.URL.Path, "/chunks/"); ok {
		f.chunkRequests++
		if f.transientChunkFailures > 0 {
			f.transientChunkFailures--
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		raw, ok := f.chunks[hash]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write(raw)
		return
	}
	if raw, ok := f.artifacts[filepath.Base(r.URL.Path)]; ok {
		_, _ = w.Write(raw)
		return
	}
	if filepath.Base(r.URL.Path) != f.record.Peer.Generation {
		http.NotFound(w, r)
		return
	}
	_ = json.NewEncoder(w).Encode(f.descriptor)
}

func TestPendingHandoffRetriesPeerWhileCloudChunkIsAbsent(t *testing.T) {
	data := bytes.Repeat([]byte{79}, 4096)
	f := newHibPeerClientFixture(t, data)
	f.record.Generation = f.record.Peer.Generation
	f.descriptor.Record.Generation = f.record.Generation
	f.transientChunkFailures = 1
	f.store.mu.Lock()
	delete(f.store.objects, chunkObj(testChunkHash(data)))
	f.store.mu.Unlock()
	p, err := f.server.openHibernationPeer(context.Background(), &f.record)
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.load(0)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("pending chunk retry = %x, %v", got, err)
	}
	f.mu.Lock()
	requests := f.chunkRequests
	f.mu.Unlock()
	if requests != 2 || p.unavailable.Load() {
		t.Fatalf("peer abandoned after transient failure: requests=%d", requests)
	}
	if f.server.met.hibPeerFallbacks.Load() != 1 {
		t.Fatal("missing failed-attempt evidence")
	}
}

func TestHibPeerClientLateFaultSurvivesRequestAndSourceRemoval(t *testing.T) {
	first, late := bytes.Repeat([]byte{17}, 4096), bytes.Repeat([]byte{29}, 4096)
	f := newHibPeerClientFixture(t, first, late)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := f.server.openHibernationPeer(ctx, &f.record)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	got, err := p.load(0)
	if err != nil || !bytes.Equal(got, first) {
		t.Fatalf("peer load after request cancellation: %v", err)
	}
	lateHash := testChunkHash(late)
	if _, err := os.Stat(filepath.Join(f.server.chunkCacheDir(), lateHash)); !os.IsNotExist(err) {
		t.Fatalf("late chunk already cached: %v", err)
	}
	f.mu.Lock()
	f.deleted = true
	f.descriptor.Manifest.Chunks[1].Hash = testChunkHash(bytes.Repeat([]byte{99}, 4096))
	replacement, _ := json.Marshal(f.descriptor.Manifest)
	f.mu.Unlock()
	if err := f.store.client.PutBytes(context.Background(), hibManifestObj(f.record.ID), replacement); err != nil {
		t.Fatal(err)
	}
	got, err = p.load(1)
	if err != nil || !bytes.Equal(got, late) {
		t.Fatalf("late fault changed generation or lost durable fallback: %v", err)
	}
	if f.server.met.hibPeerChunks.Load() != 1 || f.server.met.hibPeerFallbacks.Load() != 1 {
		t.Fatal("expected first chunk from peer and uncached late chunk through fallback")
	}
}

func TestHibPeerClientCorruptionFallsBackToCapturedCloudHash(t *testing.T) {
	for name, size := range map[string]int{"wrong hash": 4096, "oversized": 4097} {
		t.Run(name, func(t *testing.T) {
			want := bytes.Repeat([]byte{42}, 4096)
			f := newHibPeerClientFixture(t, want)
			hash := testChunkHash(want)
			f.mu.Lock()
			f.chunks[hash] = bytes.Repeat([]byte{88}, size)
			f.mu.Unlock()
			if err := os.MkdirAll(f.server.chunkCacheDir(), 0700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(f.server.chunkCacheDir(), hash), bytes.Repeat([]byte{91}, 4096), 0600); err != nil {
				t.Fatal(err)
			}
			p, err := f.server.openHibernationPeer(context.Background(), &f.record)
			if err != nil {
				t.Fatal(err)
			}
			got, err := p.load(0)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("corrupt cache/peer did not recover original cloud bytes: %v", err)
			}
			cached, err := os.ReadFile(filepath.Join(f.server.chunkCacheDir(), hash))
			if err != nil || !bytes.Equal(cached, want) {
				t.Fatalf("cache was not repaired: %v", err)
			}
			if f.server.met.hibPeerChunks.Load() != 0 || f.server.met.hibPeerFallbacks.Load() != 1 {
				t.Fatal("corrupt peer was counted as successful transfer")
			}
		})
	}
}

func TestHibPeerClientRejectsUnboundDescriptor(t *testing.T) {
	for name, mutate := range map[string]func(*retainedHibernation){
		"record": func(d *retainedHibernation) { d.Record.ID = uuid.NewString() },
		"manifest": func(d *retainedHibernation) {
			d.Manifest.Chunks[0].Hash = testChunkHash(bytes.Repeat([]byte{73}, 4096))
		},
		"generation": func(d *retainedHibernation) { d.Ref.Generation = uuid.NewString() },
	} {
		t.Run(name, func(t *testing.T) {
			f := newHibPeerClientFixture(t, bytes.Repeat([]byte{7}, 4096))
			f.mu.Lock()
			mutate(&f.descriptor)
			f.mu.Unlock()
			if _, err := f.server.openHibernationPeer(context.Background(), &f.record); err == nil {
				t.Fatal("accepted descriptor not bound to committed record")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.chunkRequests != 0 {
				t.Fatal("fetched RAM before descriptor validation")
			}
		})
	}
}

func TestHibPeerClientHydrationAcknowledgesVerifiedCache(t *testing.T) {
	first, second := bytes.Repeat([]byte{31}, 4096), bytes.Repeat([]byte{32}, 4096)
	f := newHibPeerClientFixture(t, first, first, make([]byte, 4096), second)
	p, err := f.server.openHibernationPeer(context.Background(), &f.record)
	if err != nil {
		t.Fatal(err)
	}
	if err := p.hydrateAndAcknowledge(context.Background()); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if f.acks != 1 || f.ackBeforeCache || f.chunkRequests != 2 {
		t.Errorf("acks=%d before_cache=%v chunk_requests=%d", f.acks, f.ackBeforeCache, f.chunkRequests)
	}
	f.mu.Unlock()
	f.store.mu.Lock()
	f.store.objects = map[string]uploadTestObject{}
	f.store.mu.Unlock()
	for i, want := range [][]byte{first, first, make([]byte, 4096), second} {
		got, err := p.load(uint64(i))
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("chunk %d not independently usable after ack and cloud removal: %v", i, err)
		}
	}
}

func TestHibPeerClientHydrationDoesNotAcknowledgeIncompleteCache(t *testing.T) {
	for _, failure := range []string{"missing chunk", "cache unavailable"} {
		t.Run(failure, func(t *testing.T) {
			f := newHibPeerClientFixture(t, bytes.Repeat([]byte{61}, 4096))
			if failure == "missing chunk" {
				f.mu.Lock()
				f.chunks = map[string][]byte{}
				f.mu.Unlock()
				f.store.mu.Lock()
				f.store.objects = map[string]uploadTestObject{}
				f.store.mu.Unlock()
			} else {
				cache := f.server.chunkCacheDir()
				if err := os.MkdirAll(filepath.Dir(cache), 0700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(cache, []byte("not a directory"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			p, err := f.server.openHibernationPeer(context.Background(), &f.record)
			if err != nil {
				t.Fatal(err)
			}
			if err := p.hydrateAndAcknowledge(context.Background()); err == nil {
				t.Fatal("incomplete cache acknowledged")
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if f.acks != 0 || f.deleted {
				t.Fatal("source released before destination had a verified cache")
			}
		})
	}
}
