//go:build linux

package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"sync/atomic"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
)

func seedPeerRelease(t *testing.T) (*Server, *uploadTestStore, registry.Sandbox, map[string][]byte) {
	t.Helper()
	s, reg := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	s.cfg.AdvertiseAddr = "http://10.128.0.28:8080"
	store := newUploadTestStore(t)
	s.blob = store.client
	ctx := context.Background()
	id := "peer-release-sandbox"
	mem, state, rootfs, err := s.cfg.Provisioner.SnapshotPaths(hibID(id))
	if err != nil {
		t.Fatal(err)
	}
	sb, err := reg.Create(ctx, id, "release fixture", s.cfg.Provisioner.RootfsPathFor(id), nil, "", -1, 1, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.Hibernate(ctx, id); err != nil {
		t.Fatal(err)
	}
	sb.Status = registry.StatusHibernated
	files := map[string][]byte{
		mem: bytes.Repeat([]byte{23}, 1<<20), state: []byte("frozen-state"),
		rootfs: []byte("frozen-rootfs"), sb.RootfsPath: []byte("serving-rootfs"),
	}
	for path, data := range files {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.uploadMemChunks(ctx, id, mem, 1<<20, nil); err != nil {
		t.Fatal(err)
	}
	for _, artifact := range []struct{ object, path string }{{hibStateObj(id), state}, {hibRootfsObj(id), rootfs}} {
		if _, err := s.blob.PutSparse(ctx, artifact.object, artifact.path); err != nil {
			t.Fatal(err)
		}
	}
	rec := buildHibRecord(sb, nil, memFormChunked, "", rootfsFormFull, "", 0)
	data, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.blob.PutBytes(ctx, hibRecordObj(id), data); err != nil {
		t.Fatal(err)
	}
	return s, store, sb, files
}

func assertNoPeerReleaseGeneration(t *testing.T, s *Server) {
	t.Helper()
	entries, err := os.ReadDir(s.hibPeerDir())
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("retained generations survived: %v", entries)
	}
}

func TestPeerReleasePublishesHintOnlyAfterLocalRemoval(t *testing.T) {
	s, store, sb, files := seedPeerRelease(t)
	var published atomic.Bool
	store.hook(func(r *http.Request, object string) int {
		if object != hibRecordObj(sb.ID) {
			return 0
		}
		if r.URL.Query().Get("ifGenerationMatch") == "" {
			t.Error("peer hint publication was not conditional")
		}
		if _, err := s.reg.Get(context.Background(), sb.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("peer hint published before row removal: %v", err)
		}
		for path := range files {
			if _, err := os.Stat(path); !os.IsNotExist(err) {
				t.Errorf("peer hint published before local artifact removal %s: %v", path, err)
			}
		}
		published.Store(true)
		return 0
	})
	peer, err := s.releaseHibernation(context.Background(), sb)
	if err != nil || peer == nil {
		t.Fatalf("release = %v, %v", peer, err)
	}
	if !published.Load() {
		t.Fatal("peer hint was not published")
	}
	rec, err := s.fetchHibRecord(context.Background(), sb.ID)
	if err != nil || rec.Peer == nil || *rec.Peer != *peer {
		t.Fatalf("durable peer reference differs: record=%+v error=%v", rec, err)
	}
	desc, err := s.readRetainedHibernation(peer.Generation)
	if err != nil || desc.Record.ID != sb.ID {
		t.Fatalf("released bytes unavailable through retained generation: %+v, %v", desc, err)
	}
}

func TestPeerReleaseCASConflictDoesNotResurrectInvalidatedRecord(t *testing.T) {
	s, store, sb, _ := seedPeerRelease(t)
	var invalidated atomic.Bool
	store.hook(func(_ *http.Request, object string) int {
		if object == hibRecordObj(sb.ID) {
			store.mu.Lock()
			delete(store.objects, object)
			store.mu.Unlock()
			invalidated.Store(true)
		}
		return 0
	})
	peer, err := s.releaseHibernation(context.Background(), sb)
	if err != nil || peer != nil || !invalidated.Load() {
		t.Fatalf("release after invalidation = %v, %v, invalidated=%t", peer, err, invalidated.Load())
	}
	if _, err := s.blob.GetBytes(context.Background(), hibRecordObj(sb.ID)); !errors.Is(err, gcsblob.ErrNotExist) {
		t.Fatalf("conditional publication resurrected old record: %v", err)
	}
	if _, err := s.reg.Get(context.Background(), sb.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed release retained a serving row: %v", err)
	}
	assertNoPeerReleaseGeneration(t, s)
}

func TestPeerReleaseRegistryFailurePreservesOriginalFiles(t *testing.T) {
	s, store, sb, files := seedPeerRelease(t)
	db, err := sql.Open("sqlite", s.reg.Path())
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TRIGGER reject_peer_release BEFORE DELETE ON sandboxes BEGIN SELECT RAISE(FAIL, 'injected release failure'); END`); err != nil {
		t.Fatal(err)
	}
	peer, err := s.releaseHibernation(context.Background(), sb)
	if err == nil || peer != nil {
		t.Fatalf("release ignored registry failure: %v, %v", peer, err)
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("failed release modified original artifact %s: %v", path, err)
		}
	}
	current, err := s.reg.Get(context.Background(), sb.ID)
	if err != nil || current.Status != registry.StatusHibernated {
		t.Fatalf("failed release lost hibernated row: %+v, %v", current, err)
	}
	if store.putCount(hibRecordObj(sb.ID)) != 1 {
		t.Fatal("failed release published a peer hint")
	}
	assertNoPeerReleaseGeneration(t, s)
}

func TestPeerReleaseWithoutPrivateAddressKeepsGCSFallback(t *testing.T) {
	s, store, sb, _ := seedPeerRelease(t)
	s.cfg.AdvertiseAddr = ""
	s.cfg.ListenAddr = ":8080"
	peer, err := s.releaseHibernation(context.Background(), sb)
	if err != nil || peer != nil {
		t.Fatalf("durable-only release = %v, %v", peer, err)
	}
	rec, err := s.fetchHibRecord(context.Background(), sb.ID)
	if err != nil || rec.Peer != nil {
		t.Fatalf("GCS record not preserved: %+v, %v", rec, err)
	}
	if store.putCount(hibRecordObj(sb.ID)) != 1 {
		t.Fatal("retention failure changed the durable record")
	}
	if _, err := s.reg.Get(context.Background(), sb.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("GCS release kept source row: %v", err)
	}
	target, _ := testLifecycleServer(t)
	target.cfg.Provisioner.RootfsDir = t.TempDir()
	target.blob = store.client
	if _, err := target.reconstructHibArtifacts(context.Background(), rec, target.cfg.Provisioner.RootfsPathFor(sb.ID)); err != nil {
		t.Fatalf("remaining GCS record cannot reconstruct artifacts: %v", err)
	}
	assertNoPeerReleaseGeneration(t, s)
}
