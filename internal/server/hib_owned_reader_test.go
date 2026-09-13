package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/google/uuid"
)

type ownedPeerFixture struct {
	*hibPeerClientFixture
	catalog   *chunkstore.Store
	publisher string
	raw       []byte
}

func newOwnedPeerFixture(t *testing.T, wholeMiB bool) *ownedPeerFixture {
	t.Helper()
	raw := bytes.Repeat([]byte{53}, 4096)
	pages := [][]byte{raw}
	if wholeMiB {
		pages = make([][]byte, 256)
		for i := range pages {
			pages[i] = make([]byte, 4096)
		}
		pages[0] = raw
	}
	f := newHibPeerClientFixture(t, pages...)
	f.record.Generation = f.record.Peer.Generation
	if wholeMiB {
		f.record.MemMIB = 1
	}
	f.descriptor.Record = f.record
	f.descriptor.Record.Peer = nil
	m := &f.descriptor.Manifest
	m.Version, m.Codec = 3, "raw-sha256"
	m.Storage = &chunkStorageRef{SetID: f.record.Generation, RootID: handoffControlObj(f.record.ID)}
	for i := range m.Chunks {
		m.Chunks[i].CLen = 0
	}
	digest, err := hibPeerManifestDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	f.record.Peer.ManifestSHA256 = digest
	f.descriptor.Ref = *f.record.Peer
	catalog := chunkstore.New(f.store.client)
	publisher := uuid.NewString()
	if err := catalog.Begin(context.Background(), m.Storage.SetID, publisher); err != nil {
		t.Fatal(err)
	}
	if err := catalog.AttachRoot(context.Background(), m.Storage.SetID, publisher, m.Storage.RootID); err != nil {
		t.Fatal(err)
	}
	compressed, err := gzipBytes(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.client.PutBytes(context.Background(), m.chunkObject(testChunkHash(raw)), compressed); err != nil {
		t.Fatal(err)
	}
	f.store.mu.Lock()
	delete(f.store.objects, chunkObj(testChunkHash(raw)))
	f.store.mu.Unlock()
	return &ownedPeerFixture{hibPeerClientFixture: f, catalog: catalog, publisher: publisher, raw: raw}
}

func (f *ownedPeerFixture) view(t *testing.T) chunkstore.View {
	t.Helper()
	v, err := f.catalog.Inspect(context.Background(), f.record.Generation)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func (f *ownedPeerFixture) retire(t *testing.T) {
	t.Helper()
	ctx := context.Background()
	if err := f.catalog.RetireRoot(ctx, f.record.Generation, f.descriptor.Manifest.Storage.RootID); err != nil {
		t.Fatal(err)
	}
	if err := f.catalog.ReleasePublisher(ctx, f.record.Generation, f.publisher); err != nil {
		t.Fatal(err)
	}
}

func (f *ownedPeerFixture) artifacts(t *testing.T) {
	t.Helper()
	for _, name := range []string{"state", "rootfs"} {
		path := filepath.Join(t.TempDir(), name)
		data := []byte("owned " + name)
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		var wire bytes.Buffer
		if _, err := gcsblob.WriteRanges(&wire, path, []gcsblob.Range{{Off: 0, Len: int64(len(data))}}); err != nil {
			t.Fatal(err)
		}
		f.mu.Lock()
		f.hibPeerClientFixture.artifacts[name] = bytes.Clone(wire.Bytes())
		f.mu.Unlock()
		if err := f.store.client.PutBytes(context.Background(), f.descriptor.artifactObject(name+".sz"), wire.Bytes()); err != nil {
			t.Fatal(err)
		}
	}
	data, err := json.Marshal(f.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.client.PutBytes(context.Background(), f.descriptor.artifactObject("record.json"), data); err != nil {
		t.Fatal(err)
	}
}

func (f *ownedPeerFixture) interceptPeer(t *testing.T, intercept func(http.ResponseWriter, *http.Request) bool) {
	t.Helper()
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !intercept(w, r) {
			f.serveHTTP(w, r)
		}
	}))
	t.Cleanup(peer.Close)
	f.record.Peer.URL = peer.URL
	f.descriptor.Ref.URL = peer.URL
}

func TestOwnedPeerReaderRetainsLateFaultAfterRootRetirement(t *testing.T) {
	f := newOwnedPeerFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	p, err := f.server.openHibernationPeer(ctx, &f.record)
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if v := f.view(t); len(v.Readers) != 1 || v.Readers[0].OwnerID == "" {
		t.Fatalf("peer exposed without an attributed reader: %+v", v)
	}
	f.retire(t)
	if err := f.catalog.MarkDeleting(context.Background(), f.record.Generation); !errors.Is(err, chunkstore.ErrInvalidTransition) {
		t.Fatalf("retirement discarded the captured reader: %v", err)
	}
	f.mu.Lock()
	f.deleted = true
	f.mu.Unlock()
	got, err := p.load(0)
	if err != nil || !bytes.Equal(got, f.raw) {
		t.Fatalf("late owned cloud fault: %x, %v", got, err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}
	if v := f.view(t); v.Phase != chunkstore.Retired || len(v.Readers) != 0 {
		t.Fatalf("reader closure did not permit retirement: %+v", v)
	}
}

func TestOwnedPeerStageTransfersTwoIndependentReaders(t *testing.T) {
	f := newOwnedPeerFixture(t, true)
	f.artifacts(t)
	f.server.cfg.UFFDRestore = true
	ctx, cancel := context.WithCancel(context.Background())
	staged, err := f.server.reconstructPeerHibernation(ctx, &f.record, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
	cancel()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staged.Close() })
	if v := f.view(t); len(v.Readers) != 2 {
		t.Fatalf("VM and hydration did not get separate holders: %+v", v)
	}
	chunks, hydration := staged.takeChunks(), staged.takeHydration()
	t.Cleanup(func() { _ = chunks.Close(); _ = hydration.Close() })
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	f.retire(t)
	if v := f.view(t); len(v.Readers) != 2 {
		t.Fatalf("staging close released transferred holders: %+v", v)
	}
	if got, err := chunks.Load(0); err != nil || !bytes.Equal(got, f.raw) {
		t.Fatalf("load after request cancellation and root retirement: %v", err)
	}
	if err := chunks.Close(); err != nil {
		t.Fatal(err)
	}
	if v := f.view(t); len(v.Readers) != 1 || v.Phase != chunkstore.Live {
		t.Fatalf("source close retired independent hydration: %+v", v)
	}
	if err := hydration.Close(); err != nil {
		t.Fatal(err)
	}
	if v := f.view(t); len(v.Readers) != 0 || v.Phase != chunkstore.Retired {
		t.Fatalf("hydration close did not retire last owner: %+v", v)
	}
}

func TestOwnedPeerEagerStageReleasesMaterializationReader(t *testing.T) {
	f := newOwnedPeerFixture(t, true)
	f.artifacts(t)
	f.server.cfg.UFFDRestore = false
	staged, err := f.server.reconstructPeerHibernation(context.Background(), &f.record, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staged.Close() })
	if v := f.view(t); len(v.Readers) != 1 || staged.Chunks != nil {
		t.Fatalf("materialization retained a reader beyond completion: %+v", v)
	}
	data, err := os.ReadFile(staged.MemPath)
	if err != nil || len(data) < 4096 || !bytes.Equal(data[:4096], f.raw) {
		t.Fatalf("materialized owned memory mismatch: %v", err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if v := f.view(t); len(v.Readers) != 0 {
		t.Fatalf("staging close leaked hydration: %+v", v)
	}
}

func TestOwnedPeerStageFailureReleasesPrimary(t *testing.T) {
	for _, scenario := range []string{"artifact failure", "cancelled artifact", "root retires before hydration"} {
		t.Run(scenario, func(t *testing.T) {
			f := newOwnedPeerFixture(t, true)
			f.artifacts(t)
			f.server.cfg.UFFDRestore = true
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var intercepted atomic.Bool
			f.interceptPeer(t, func(w http.ResponseWriter, r *http.Request) bool {
				if !strings.HasSuffix(r.URL.Path, "/state") || !intercepted.CompareAndSwap(false, true) {
					return false
				}
				v, inspectErr := f.catalog.Inspect(context.Background(), f.record.Generation)
				if inspectErr != nil || len(v.Readers) != 1 {
					t.Errorf("artifact requested before primary acquisition: %+v, %v", v, inspectErr)
				}
				switch scenario {
				case "artifact failure":
					w.WriteHeader(http.StatusServiceUnavailable)
					return true
				case "cancelled artifact":
					cancel()
					return true
				default:
					if err := f.catalog.RetireRoot(context.Background(), f.record.Generation, f.descriptor.Manifest.Storage.RootID); err != nil {
						t.Error(err)
					}
					return false
				}
			})
			staged, err := f.server.reconstructPeerHibernation(ctx, &f.record, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
			_ = staged.Close()
			if err == nil || !intercepted.Load() {
				t.Fatalf("staging failure not exercised: %v", err)
			}
			if v := f.view(t); len(v.Readers) != 0 {
				t.Fatalf("failed staging leaked reader: %+v", v)
			}
		})
	}
}

func TestOwnedPeerRejectsRetiredRootBeforePayload(t *testing.T) {
	f := newOwnedPeerFixture(t, true)
	f.artifacts(t)
	f.retire(t)
	var payloadRequests atomic.Int32
	f.interceptPeer(t, func(_ http.ResponseWriter, r *http.Request) bool {
		if strings.HasSuffix(r.URL.Path, "/state") || strings.HasSuffix(r.URL.Path, "/rootfs") || strings.Contains(r.URL.Path, "/chunks/") {
			payloadRequests.Add(1)
		}
		return false
	})
	_, err := f.server.reconstructPeerHibernation(context.Background(), &f.record, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
	if !errors.Is(err, chunkstore.ErrRetired) {
		t.Fatalf("retired source acquired: %v", err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.chunkRequests != 0 || payloadRequests.Load() != 0 {
		t.Fatal("read retired generation bytes")
	}
}

func TestOwnedPendingPlaceholdersRemainPending(t *testing.T) {
	f := newOwnedPeerFixture(t, false)
	m := &f.descriptor.Manifest
	if err := f.store.client.PutBytes(context.Background(), m.chunkObject(testChunkHash(f.raw)), nil); err != nil {
		t.Fatal(err)
	}
	if err := f.store.client.PutBytes(context.Background(), f.descriptor.artifactObject("record.json"), nil); err != nil {
		t.Fatal(err)
	}
	f.transientChunkFailures = 1
	p, err := f.server.openHibernationPeer(context.Background(), &f.record)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = p.Close() })
	if got, err := p.load(0); err != nil || !bytes.Equal(got, f.raw) {
		t.Fatalf("empty cloud placeholder prevented pending peer retry: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := p.waitForBackup(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("empty record reported corruption or completion: %v", err)
	}
	data, err := json.Marshal(f.descriptor)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.client.PutBytes(context.Background(), p.backupObject, data); err != nil {
		t.Fatal(err)
	}
	if err := p.waitForBackup(context.Background()); err != nil {
		t.Fatalf("completed owned backup not recognized: %v", err)
	}
}

func TestOwnedCloudHandoffUsesProtectedGenerationObjects(t *testing.T) {
	f := newOwnedPeerFixture(t, true)
	f.artifacts(t)
	f.server.cfg.UFFDRestore = true
	f.mu.Lock()
	f.deleted = true
	f.mu.Unlock()
	var payloadReads atomic.Int32
	f.store.beforeGet = func(_ *http.Request, key string) {
		if strings.HasPrefix(key, "chunksets/data/") {
			payloadReads.Add(1)
			v, inspectErr := f.catalog.Inspect(context.Background(), f.record.Generation)
			if inspectErr != nil || len(v.Readers) != 1 {
				t.Errorf("cloud payload read without source protection: %+v, %v", v, inspectErr)
			}
		}
	}
	staged, err := f.server.reconstructHandoff(context.Background(), &f.descriptor, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staged.Close() })
	if payloadReads.Load() < 3 || staged.Chunks == nil {
		t.Fatalf("owned record/state/rootfs were not staged: %d", payloadReads.Load())
	}
	f.retire(t)
	if got, err := staged.Chunks.Load(0); err != nil || !bytes.Equal(got, f.raw) {
		t.Fatalf("protected owned chunk failed: %v", err)
	}
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	if v := f.view(t); v.Phase != chunkstore.Retired {
		t.Fatalf("cloud source retained after close: %+v", v)
	}
}

func TestOwnedCloudRecordPlaceholderClosesReader(t *testing.T) {
	f := newOwnedPeerFixture(t, true)
	f.mu.Lock()
	f.deleted = true
	f.mu.Unlock()
	if err := f.store.client.PutBytes(context.Background(), f.descriptor.artifactObject("record.json"), nil); err != nil {
		t.Fatal(err)
	}
	_, err := f.server.reconstructHandoff(context.Background(), &f.descriptor, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
	if err == nil || !strings.Contains(err.Error(), "pending") {
		t.Fatalf("placeholder treated as corruption/completion: %v", err)
	}
	if v := f.view(t); len(v.Readers) != 0 {
		t.Fatalf("incomplete cloud fallback leaked reader: %+v", v)
	}
}

func TestOwnedHydrationCancellationRetainsHolderUntilLoaderJoins(t *testing.T) {
	f := newOwnedPeerFixture(t, true)
	f.artifacts(t)
	f.server.cfg.UFFDRestore = true
	staged, err := f.server.reconstructPeerHibernation(context.Background(), &f.record, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staged.Close() })
	hydration := staged.takeHydration()
	if err := staged.Close(); err != nil {
		t.Fatal(err)
	}
	f.retire(t)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	hydration.peer.load = func(uint64) ([]byte, error) {
		close(entered)
		<-unblock
		return nil, context.Canceled
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- hydration.Run(ctx) }()
	<-entered
	cancel()
	if v := f.view(t); len(v.Readers) != 1 || v.Phase != chunkstore.Live {
		t.Fatalf("cancel released hydration before its load joined: %+v", v)
	}
	select {
	case err := <-done:
		t.Fatalf("hydration returned while its loader was blocked: %v", err)
	default:
	}
	release()
	if err := <-done; err == nil {
		t.Fatal("cancelled hydration succeeded")
	}
	if v := f.view(t); len(v.Readers) != 0 || v.Phase != chunkstore.Retired {
		t.Fatalf("joined hydration retained its holder: %+v", v)
	}
}

func TestOwnedCloudEagerHandoffReleasesReader(t *testing.T) {
	f := newOwnedPeerFixture(t, true)
	f.artifacts(t)
	f.server.cfg.UFFDRestore = false
	f.mu.Lock()
	f.deleted = true
	f.mu.Unlock()
	staged, err := f.server.reconstructHandoff(context.Background(), &f.descriptor, f.server.cfg.Provisioner.RootfsPathFor(f.record.ID))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = staged.Close() })
	if v := f.view(t); len(v.Readers) != 0 || staged.Chunks != nil || staged.MemPath == "" {
		t.Fatalf("eager cloud reader retained after materialization: %+v", v)
	}
	data, err := os.ReadFile(staged.MemPath)
	if err != nil || len(data) < len(f.raw) || !bytes.Equal(data[:len(f.raw)], f.raw) {
		t.Fatalf("owned cloud materialization mismatch: %v", err)
	}
}
