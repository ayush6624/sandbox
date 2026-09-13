package server

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

func ownedControlFixture(t *testing.T) (*Server, *uploadTestStore, *retainedHibernation, registry.HibernationHandoff) {
	t.Helper()
	ctx := context.Background()
	store := newUploadTestStore(t)
	store.generation = 1000
	s := handoffTestServer(t, store, "source")
	id, generation := uuid.NewString(), uuid.NewString()
	if _, err := s.reg.Create(ctx, id, "owned", "unused", nil, "", -1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.Hibernate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.reserveLocalHandoff(ctx, id); err != nil {
		t.Fatal(err)
	}
	d := &retainedHibernation{
		Record:   hibRecord{Version: hibRecordVersion, ID: id, Generation: generation, MemMIB: 1, MemForm: memFormChunked, RootfsForm: rootfsFormFull},
		Manifest: chunkManifest{Version: chunkManifestOwnedVersion, Codec: chunkCodecRawSHA256, MemSize: 1 << 20, ChunkSize: 1 << 20, Chunks: []chunkEntry{{Hash: chunkZeroHash}}, Storage: &chunkStorageRef{SetID: generation, RootID: handoffControlObj(id)}},
		Ref:      hibPeerRef{Generation: generation},
	}
	digest, err := hibPeerManifestDigest(&d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	d.Ref.ManifestSHA256 = digest
	job, err := s.prepareHandoffOffer(ctx, id, d)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reg.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	return s, store, d, job
}

func TestOwnedHandoffControlFencesRootBeforeCollection(t *testing.T) {
	ctx := context.Background()
	s, store, d, job := ownedControlFixture(t)
	catalog, payloads := s.chunkStorage()
	var checked atomic.Bool
	store.hook(func(_ *http.Request, name string) int {
		if name != handoffControlObj(d.Record.ID) {
			return 0
		}
		view, err := catalog.Inspect(ctx, d.Ref.Generation)
		if err != nil || len(view.Roots) != 1 || view.Roots[0].Retired || view.Publisher.Released {
			t.Errorf("offer visible without ownership: %+v, %v", view, err)
		}
		checked.Store(true)
		return http.StatusForbidden
	})
	if err := s.publishHandoffOffer(ctx, job); err == nil {
		t.Fatal("failed offer unexpectedly published")
	}
	if !checked.Load() {
		t.Fatal("offer did not reach ownership check")
	}
	store.hook(nil)
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	closeReader, err := s.acquireChunkReader(ctx, &d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	writer, err := payloads.OpenWriter(ctx, d.Ref.Generation, handoffPublisherID(d.Ref.Generation))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.PutBytes(ctx, "state.sz", []byte("retained reader bytes")); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(ctx); err != nil {
		t.Fatal(err)
	}
	store.hook(func(_ *http.Request, name string) int {
		if name == handoffControlObj(d.Record.ID) {
			return http.StatusForbidden
		}
		return 0
	})
	if err := s.destroyHandoff(ctx, d.Record.ID); err == nil {
		t.Fatal("destroy should fail before authority changes")
	}
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || view.Roots[0].Retired {
		t.Fatalf("failed destroy retired root: %+v, %v", view, err)
	}
	store.hook(nil)
	if err := s.destroyHandoff(ctx, d.Record.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	view, err = catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || !view.Roots[0].Retired || len(view.Readers) != 1 {
		t.Fatalf("destroy/replay lost reader or restored root: %+v, %v", view, err)
	}
	if _, err := payloads.Collect(ctx, d.Ref.Generation); !errors.Is(err, chunkstore.ErrInvalidTransition) {
		t.Fatalf("collected live reader: %v", err)
	}
	store.hook(func(_ *http.Request, name string) int {
		if name == "chunksets/catalog/"+d.Ref.Generation+".json" {
			return http.StatusForbidden
		}
		return 0
	})
	if err := closeReader(); err == nil {
		t.Fatal("failed reader release reported success")
	}
	store.hook(nil)
	s.retryChunkReleases(ctx)
	result, err := payloads.Collect(ctx, d.Ref.Generation)
	if err != nil || result.Objects != 1 || result.Bytes != int64(len("retained reader bytes")) {
		t.Fatalf("collection after release retry: %+v, %v", result, err)
	}
	if _, err := s.blob.GetBytes(ctx, d.artifactObject("state.sz")); !errors.Is(err, gcsblob.ErrNotExist) {
		t.Fatalf("payload remained: %v", err)
	}
	view, err = catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || view.Phase != chunkstore.Deleted {
		t.Fatalf("tombstone: %+v, %v", view, err)
	}
	if _, err := s.acquireChunkReader(ctx, &d.Manifest); err == nil {
		t.Fatal("retired root admitted reader")
	}
}

func TestOwnedManifestCannotEnterLegacyNamespace(t *testing.T) {
	s, _, d, _ := ownedControlFixture(t)
	ctx := context.Background()
	data, err := json.Marshal(d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.blob.PutBytes(ctx, hibManifestObj(d.Record.ID), data); err != nil {
		t.Fatal(err)
	}
	if _, err := s.fetchChunkManifest(ctx, d.Record.ID); err == nil {
		t.Fatal("stable legacy path admitted owned manifest without descriptor")
	}
	for _, version := range []int{chunkManifestVersion, chunkManifestRawVersion} {
		m := d.Manifest
		m.Version = version
		if version == chunkManifestVersion {
			m.Codec = chunkCodecGzip
		}
		if err := m.validate(); err == nil {
			t.Fatalf("legacy version %d accepted storage identity", version)
		}
	}
	d.Manifest.Storage.SetID = uuid.NewString()
	if err := d.validateStorage(); err == nil {
		t.Fatal("descriptor accepted unrelated data set")
	}
}

func TestOwnedDurabilityWaitRejectsPlaceholderAndWrongCommit(t *testing.T) {
	s, _, d, job := ownedControlFixture(t)
	ctx := context.Background()
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	object := d.artifactObject("record.json")
	if err := s.blob.PutBytes(ctx, object, nil); err != nil {
		t.Fatal(err)
	}
	waitCtx, cancel := context.WithTimeout(ctx, 30*time.Millisecond)
	defer cancel()
	if err := s.awaitHandoffBackup(waitCtx, d.Record.ID, d.Ref.Generation); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("placeholder counted as durable: %v", err)
	}
	wrong := *d
	wrong.WorkingSet = []uint64{0}
	data, err := json.Marshal(wrong)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.blob.PutBytes(ctx, object, data); err != nil {
		t.Fatal(err)
	}
	if err := s.awaitHandoffBackup(ctx, d.Record.ID, d.Ref.Generation); err == nil {
		t.Fatal("different backup descriptor counted as durable")
	}
	data, err = json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.blob.PutBytes(ctx, object, data); err != nil {
		t.Fatal(err)
	}
	if err := s.awaitHandoffBackup(ctx, d.Record.ID, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
}

func TestOwnedRootRetiresWhenOrdinaryCheckpointReplacesControl(t *testing.T) {
	s, _, d, job := ownedControlFixture(t)
	ctx := context.Background()
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	claim, err := s.claimHandoff(ctx, d.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeHandoffRun(ctx, claim); err != nil {
		t.Fatal(err)
	}
	closeReader, err := s.acquireChunkReader(ctx, &d.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	defer closeReader()
	rec := d.Record
	rec.Generation = ""
	if err := s.publishLocalHibernation(ctx, &rec); err != nil {
		t.Fatal(err)
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, d.Ref.Generation)
	if err != nil || !view.Roots[0].Retired || view.Publisher.Released || len(view.Readers) != 1 {
		t.Fatalf("replacement disturbed live obligations: %+v, %v", view, err)
	}
}

func TestHandoffPublicationAcceptsReplacedOpaqueGeneration(t *testing.T) {
	s, store, d, job := ownedControlFixture(t)
	ctx := context.Background()
	current, revision, err := s.readHandoff(ctx, d.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	store.mu.Lock()
	store.generation = 100
	store.mu.Unlock()
	next := *current
	next.Phase = handoffDestroyed
	written, err := s.casHandoff(ctx, next, revision)
	if err != nil {
		t.Fatal(err)
	}
	if written >= job.ExpectedRevision {
		t.Fatal("fixture did not decrease opaque generation")
	}
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatalf("replaced CAS did not settle publication: %v", err)
	}
	settled, err := s.reg.GetHibernationHandoff(ctx, job.Generation)
	if err != nil || !settled.Published {
		t.Fatalf("publication unsettled: %+v, %v", settled, err)
	}
}
