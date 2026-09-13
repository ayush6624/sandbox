package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/google/uuid"
)

func seedOwnedHandoffBackup(t *testing.T, published bool) (*Server, *uploadTestStore, *retainedHibernation, []byte) {
	t.Helper()
	return seedOwnedHandoffBackupWithDescriptor(t, published, nil)
}

func seedOwnedHandoffBackupWithDescriptor(t *testing.T, published bool, configure func(*Server, *uploadTestStore, *retainedHibernation)) (*Server, *uploadTestStore, *retainedHibernation, []byte) {
	t.Helper()
	ctx := context.Background()
	s, reg := testLifecycleServer(t)
	store := newUploadTestStore(t)
	store.generation = 1000
	s.blob = store.client
	id := uuid.NewString()
	if _, err := reg.Create(ctx, id, "owned-backup", "unused-rootfs", nil, "", -1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.Hibernate(ctx, id); err != nil {
		t.Fatal(err)
	}
	d, _ := writeRetainedTestBundle(t, s, id)
	raw := bytes.Repeat([]byte("owned-generation"), (1<<20)/len("owned-generation")+1)[:1<<20]
	memPath := filepath.Join(s.hibPeerDir(), d.Ref.Generation, "mem.bin")
	if err := os.WriteFile(memPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	d.Record.Generation = d.Ref.Generation
	d.Record.Version = hibRecordVersion
	d.Record.MemMIB = 1
	m, err := buildRawChunkManifest(ctx, memPath, 4096)
	if err != nil {
		t.Fatal(err)
	}
	m.Version = chunkManifestOwnedVersion
	m.Storage = &chunkStorageRef{SetID: d.Ref.Generation, RootID: handoffControlObj(id)}
	d.Manifest = *m
	d.Ref.ManifestSHA256, err = hibPeerManifestDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	if configure != nil {
		configure(s, store, d)
	}
	writeRetainedTestDescriptor(t, s, d)
	if err := s.reserveLocalHandoff(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.ensureHandoffStorage(ctx, d); err != nil {
		t.Fatal(err)
	}
	job, err := s.prepareHandoffOffer(ctx, id, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	if published {
		if err := reg.MarkHibernationHandoffPublished(ctx, d.Ref.Generation); err != nil {
			t.Fatal(err)
		}
	}
	return s, store, d, raw
}

func uploadObject(store *uploadTestStore, name string) (uploadTestObject, bool) {
	store.mu.Lock()
	defer store.mu.Unlock()
	object, ok := store.objects[name]
	return object, ok
}

func TestOwnedHandoffBackupRetriesImmutablePayloadsAndRestoresThroughReader(t *testing.T) {
	s, store, d, raw := seedOwnedHandoffBackup(t, true)
	record := d.artifactObject("record.json")
	store.hook(func(r *http.Request, name string) int {
		if name == record && r.URL.Query().Get("ifGenerationMatch") != "0" {
			return http.StatusInternalServerError
		}
		return 0
	})
	failed, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	if err := s.backupHandoff(failed, d.Ref.Generation); err == nil {
		t.Fatal("failed record upload completed backup")
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(context.Background(), d.Manifest.Storage.SetID)
	if err != nil || view.Publisher.Released {
		t.Fatalf("failed backup publisher=%+v error=%v", view.Publisher, err)
	}
	before := make(map[string]int64)
	for _, name := range []string{d.Manifest.chunkObject(d.Manifest.Chunks[0].Hash), d.artifactObject("state.sz"), d.artifactObject("rootfs.sz"), d.artifactObject("backup-receipt.json")} {
		object, ok := uploadObject(store, name)
		if !ok || len(object.data) == 0 {
			t.Fatalf("partial backup missing %s", name)
		}
		before[name] = object.generation
	}
	store.hook(nil)
	if err := s.backupHandoff(context.Background(), d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	for name, generation := range before {
		if object, _ := uploadObject(store, name); object.generation != generation {
			t.Fatalf("immutable retry rewrote %s", name)
		}
	}
	view, err = catalog.Inspect(context.Background(), d.Manifest.Storage.SetID)
	if err != nil || !view.Publisher.Released {
		t.Fatalf("completed backup publisher=%+v error=%v", view.Publisher, err)
	}
	store.mu.Lock()
	var payloadWrites []string
	for _, name := range store.writes {
		if strings.HasPrefix(name, chunkstore.DataPrefix(d.Manifest.Storage.SetID)) {
			payloadWrites = append(payloadWrites, name)
		}
	}
	store.mu.Unlock()
	if len(payloadWrites) == 0 || payloadWrites[len(payloadWrites)-1] != record {
		t.Fatalf("record was not final payload: %v", payloadWrites)
	}

	staged, err := s.reconstructHandoff(context.Background(), d, filepath.Join(t.TempDir(), "restored.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	got, err := os.ReadFile(staged.MemPath)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("owned backup reader restored %d bytes: %v", len(got), err)
	}
}

func TestOwnedHandoffCleanupWaitsForPublicationAndPublisherRelease(t *testing.T) {
	s, store, d, _ := seedOwnedHandoffBackup(t, false)
	ctx := context.Background()
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := s.reg.GetHibernationHandoff(ctx, d.Ref.Generation)
	if err != nil || !job.BackupComplete || job.Published {
		t.Fatalf("unsettled job=%+v error=%v", job, err)
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Manifest.Storage.SetID)
	if err != nil || view.Publisher.Released {
		t.Fatalf("unsettled publication released publisher: %+v, %v", view.Publisher, err)
	}
	if err := s.reg.MarkHibernationHandoffPublished(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.AckHibernationHandoffCache(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	catalogObject := "chunksets/catalog/" + d.Manifest.Storage.SetID + ".json"
	store.hook(func(_ *http.Request, name string) int {
		if name == catalogObject {
			return http.StatusInternalServerError
		}
		return 0
	})
	failed, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if err := s.backupHandoff(failed, d.Ref.Generation); err == nil {
		t.Fatal("cleanup ignored publisher release failure")
	}
	if _, err := s.reg.GetHibernationHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatalf("release failure removed journal: %v", err)
	}
	if _, err := os.Stat(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); err != nil {
		t.Fatalf("release failure removed local generation: %v", err)
	}
	store.hook(nil)
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	view, err = catalog.Inspect(ctx, d.Manifest.Storage.SetID)
	if err != nil || !view.Publisher.Released {
		t.Fatalf("cleanup did not release publisher: %+v, %v", view.Publisher, err)
	}
	if _, err := s.reg.GetHibernationHandoff(ctx, d.Ref.Generation); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("released cleanup retained journal: %v", err)
	}
}

func TestOwnedHandoffCleanupRetiresRootAfterAuthorityMoves(t *testing.T) {
	s, store, d, _ := seedOwnedHandoffBackup(t, false)
	ctx := context.Background()
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	current, revision, err := s.readHandoff(ctx, d.Record.ID)
	if err != nil || current == nil {
		t.Fatalf("read current control: %+v, %v", current, err)
	}
	next := *current
	next.Generation = uuid.NewString()
	next.Phase = handoffDestroyed
	next.Record.Generation = next.Generation
	next.Descriptor = nil
	store.mu.Lock()
	store.generation = 100
	store.mu.Unlock()
	if _, err := s.casHandoff(ctx, next, revision); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.MarkHibernationHandoffPublished(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Manifest.Storage.SetID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Phase != chunkstore.Retired || !view.Publisher.Released || len(view.Roots) != 1 || !view.Roots[0].Retired {
		t.Fatalf("moved authority retained holders: %+v", view)
	}
}

func TestHandoffJobDescriptorRejectsMalformedOrMismatchedOffer(t *testing.T) {
	s, _, d, _ := seedOwnedHandoffBackup(t, false)
	job, err := s.reg.GetHibernationHandoff(context.Background(), d.Ref.Generation)
	if err != nil {
		t.Fatal(err)
	}
	job.Generation = uuid.NewString()
	if _, err := handoffJobDescriptor(job); err == nil {
		t.Fatal("journal generation mismatch was accepted")
	}
	job.Offer = []byte("{")
	if _, err := handoffJobDescriptor(job); err == nil {
		t.Fatal("malformed journal offer was accepted")
	}
}
