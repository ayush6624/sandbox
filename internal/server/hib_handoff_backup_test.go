package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

func seedHandoffBackup(t *testing.T) (*Server, *uploadTestStore, *retainedHibernation, []byte) {
	t.Helper()
	s, reg := testLifecycleServer(t)
	store := newUploadTestStore(t)
	s.blob = store.client
	id := uuid.NewString()
	if _, err := reg.Create(context.Background(), id, "backup", "unused-rootfs", nil, "", -1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := reg.Hibernate(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	d, raw := writeRetainedTestBundle(t, s, id)
	d.Record.Generation = d.Ref.Generation
	d.Record.Version = hibRecordVersion
	m, err := buildRawChunkManifest(context.Background(), filepath.Join(s.hibPeerDir(), d.Ref.Generation, "mem.bin"), 4096)
	if err != nil {
		t.Fatal(err)
	}
	d.Manifest = *m
	d.Ref.ManifestSHA256, err = hibPeerManifestDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	writeRetainedTestDescriptor(t, s, d)
	offer := handoffControl{Version: handoffControlVersion, Generation: d.Ref.Generation, Phase: handoffOffered, SourceHostID: s.hostID(), HostID: s.hostID(), RegistryID: reg.RegistryID(), Record: d.Record, Descriptor: d}
	offerData, err := json.Marshal(offer)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.CommitHibernationHandoff(context.Background(), registry.HibernationHandoff{Generation: d.Ref.Generation, SandboxID: id, Offer: offerData}); err != nil {
		t.Fatal(err)
	}
	if err := reg.MarkHibernationHandoffPublished(context.Background(), d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	return s, store, d, raw
}

func TestHandoffAckAndExpiryPreserveBlockedBackupAndPeerReads(t *testing.T) {
	s, store, d, raw := seedHandoffBackup(t)
	d.Ref.ExpiresAtUnix = time.Now().Add(-time.Minute).Unix()
	writeRetainedTestDescriptor(t, s, d)
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	store.hook(func(_ *http.Request, name string) int {
		if strings.HasPrefix(name, "chunks/") {
			once.Do(func() { close(entered); <-unblock })
		}
		return 0
	})
	done := make(chan error, 1)
	go func() { done <- s.backupHandoff(context.Background(), d.Ref.Generation) }()
	defer func() {
		select {
		case <-unblock:
		default:
			close(unblock)
		}
	}()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("backup did not start")
	}
	acked := make(chan error, 1)
	go func() { acked <- s.removePeerHibernation(d.Ref.Generation) }()
	select {
	case err := <-acked:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("backup blocked cache acknowledgment")
	}
	s.sweepPeerHibernations(time.Now())
	if count, err := s.pendingHandoffCount(context.Background()); err != nil || count != 1 {
		t.Fatalf("pending = %d, %v", count, err)
	}
	response := retainedTestRequest(s, http.MethodGet, d.Ref.Generation, "", d.Manifest.Chunks[0].Hash)
	if response.Code != http.StatusOK || !bytes.Equal(response.Body.Bytes(), raw[:4096]) {
		t.Fatalf("pending backup lost peer bytes: HTTP %d", response.Code)
	}
	close(unblock)
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("backup did not complete")
	}
	if _, err := os.Stat(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); !os.IsNotExist(err) {
		t.Fatalf("acknowledged complete generation retained: %v", err)
	}
	if _, err := s.reg.GetHibernationHandoff(context.Background(), d.Ref.Generation); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed journal retained: %v", err)
	}
	data, err := store.client.GetBytes(context.Background(), handoffRecordObj(d.Record.ID, d.Ref.Generation))
	if err != nil {
		t.Fatal(err)
	}
	var complete retainedHibernation
	if err := json.Unmarshal(data, &complete); err != nil || complete.Record.Generation != d.Ref.Generation {
		t.Fatalf("completion marker = %+v, %v", complete, err)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	for _, name := range store.writes {
		if strings.HasPrefix(name, "hib/") {
			t.Errorf("backup wrote mutable object %s", name)
		}
	}
	if store.writes[len(store.writes)-1] != handoffRecordObj(d.Record.ID, d.Ref.Generation) {
		t.Fatal("record was not the final payload")
	}
}

func TestHandoffBackupRejectsChangedMemory(t *testing.T) {
	s, store, d, raw := seedHandoffBackup(t)
	raw[0] ^= 255
	if err := os.WriteFile(filepath.Join(s.hibPeerDir(), d.Ref.Generation, "mem.bin"), raw, 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.backupHandoff(context.Background(), d.Ref.Generation); err == nil {
		t.Fatal("changed source memory was published")
	}
	if _, err := store.client.GetBytes(context.Background(), handoffRecordObj(d.Record.ID, d.Ref.Generation)); !errors.Is(err, gcsblob.ErrNotExist) {
		t.Fatalf("corrupt backup has completion marker: %v", err)
	}
	if err := s.removePeerHibernation(d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	job, err := s.reg.GetHibernationHandoff(context.Background(), d.Ref.Generation)
	if err != nil || job.BackupComplete || !job.CacheReady {
		t.Fatalf("corrupt backup retention = %+v, %v", job, err)
	}
}

func TestHandoffCleanupRestartFinishesAfterFilesWereRemoved(t *testing.T) {
	s, _, d, _ := seedHandoffBackup(t)
	ctx := context.Background()
	if err := s.reg.WithHibernationHandoff(ctx, d.Ref.Generation, func(a *registry.HandoffAttempt) error { return a.CompleteBackup(ctx) }); err != nil {
		t.Fatal(err)
	}
	// An expiry cleanup crashed after directory removal and before journal removal.
	if err := os.RemoveAll(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); err != nil {
		t.Fatal(err)
	}
	if err := s.cleanupHandoffGeneration(ctx, d.Ref.Generation, time.Now()); err != nil {
		t.Fatal(err)
	}
	if count, err := s.pendingHandoffCount(ctx); err != nil || count != 0 {
		t.Fatalf("cleanup left a permanent retirement pin: %d, %v", count, err)
	}
}

func TestHandoffBackupRestartRetainsOldGenerationAndNeverRepublishesSandbox(t *testing.T) {
	s, store, d, raw := seedHandoffBackup(t)
	ctx := context.Background()
	// A later local sandbox with the same identity must not affect this job.
	if _, err := s.reg.Create(ctx, d.Record.ID, "returned", "returned-rootfs", nil, "", -1, 1, 1); err != nil {
		t.Fatal(err)
	}
	path := s.reg.Path()
	if err := s.reg.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := registry.Open(path, registry.Pools{TapPrefix: "fc", TapMax: 2, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11", PortMin: 5200, PortMax: 5201})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	restarted := New(s.cfg, reopened)
	defer restarted.pf.CloseAll()
	restarted.blob = store.client
	runCtx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); restarted.runHandoffBackups(runCtx) }()
	defer func() { cancel(); <-done }()
	deadline := time.Now().Add(5 * time.Second)
	for {
		job, err := reopened.GetHibernationHandoff(ctx, d.Ref.Generation)
		if err != nil {
			t.Fatal(err)
		}
		if job.BackupComplete {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("restart did not complete pending backup")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if row, err := reopened.Get(ctx, d.Record.ID); err != nil || row.Name != "returned" {
		t.Fatalf("backup touched returned sandbox: %+v, %v", row, err)
	}
	for idx, entry := range d.Manifest.Chunks {
		data, err := store.client.GetBytes(ctx, chunkObj(entry.Hash))
		if err != nil {
			t.Fatal(err)
		}
		got, err := gunzipBytes(data)
		if err != nil || !bytes.Equal(got, raw[idx*4096:(idx+1)*4096]) {
			t.Fatalf("old chunk %d changed: %v", idx, err)
		}
	}
	if _, err := store.client.GetBytes(ctx, hibRecordObj(d.Record.ID)); !errors.Is(err, gcsblob.ErrNotExist) {
		t.Fatalf("backup resurrected current record: %v", err)
	}
	if err := restarted.removePeerHibernation(d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	if count, err := restarted.pendingHandoffCount(ctx); err != nil || count != 0 {
		t.Fatalf("completed source not retireable: %d, %v", count, err)
	}
}
