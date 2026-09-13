package server

import (
	"bytes"
	"context"
	"errors"
	"github.com/ayush6624/sandbox/internal/registry"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestOwnedHandoffBackupKeepsPublisherWhileAnotherAttemptIsLive(t *testing.T) {
	s, store, d, _ := seedOwnedHandoffBackup(t, true)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	var held atomic.Bool
	key := d.Manifest.chunkObject(d.Manifest.Chunks[0].Hash)
	store.hook(func(r *http.Request, name string) int {
		if name == key && r.URL.Query().Get("ifGenerationMatch") == "0" && held.CompareAndSwap(false, true) {
			close(entered)
			<-unblock
		}
		return 0
	})
	done := make(chan error, 1)
	go func() { done <- s.backupHandoff(ctx, d.Ref.Generation) }()
	<-entered
	competing := New(s.cfg, s.reg)
	competing.blob = store.client
	defer competing.pf.CloseAll()
	err := competing.backupHandoff(ctx, d.Ref.Generation)
	catalog, _ := s.chunkStorage()
	view, inspectErr := catalog.Inspect(ctx, d.Ref.Generation)
	release()
	firstErr := <-done
	if inspectErr != nil {
		t.Fatal(inspectErr)
	}
	if view.Publisher.Released {
		t.Fatal("competing backup released publisher while first attempt still had payload capability")
	}
	if !errors.Is(err, registry.ErrHandoffBusy) {
		t.Fatalf("competing backup: %v, want busy", err)
	}
	if firstErr != nil {
		t.Fatalf("original backup: %v", firstErr)
	}
}

func TestOwnedHandoffBackupRejectsDescriptorDriftBeforePayload(t *testing.T) {
	for _, change := range []string{"working set", "manifest", "storage downgrade"} {
		t.Run(change, func(t *testing.T) {
			s, store, d, _ := seedOwnedHandoffBackup(t, true)
			switch change {
			case "working set":
				d.WorkingSet = []uint64{0, 1}
			case "manifest":
				// Keep the local descriptor self-consistent while changing its offered content.
				d.Manifest.Chunks[0].Hash = strings.Repeat("a", 64)
				var err error
				d.Ref.ManifestSHA256, err = hibPeerManifestDigest(&d.Manifest)
				if err != nil {
					t.Fatal(err)
				}
			case "storage downgrade":
				d.Manifest.Version = chunkManifestRawVersion
				d.Manifest.Storage = nil
			}
			if change == "storage downgrade" {
				var err error
				d.Ref.ManifestSHA256, err = hibPeerManifestDigest(&d.Manifest)
				if err != nil {
					t.Fatal(err)
				}
			}
			writeRetainedTestDescriptor(t, s, d)
			if err := s.backupHandoff(context.Background(), d.Ref.Generation); err == nil || !strings.Contains(err.Error(), "descriptor differs") {
				t.Fatalf("changed descriptor admitted: %v", err)
			}
			store.mu.Lock()
			for key := range store.puts {
				if strings.HasPrefix(key, "chunksets/data/") || strings.HasPrefix(key, "chunks/") {
					t.Errorf("descriptor mismatch uploaded %s", key)
				}
			}
			store.mu.Unlock()
			job, err := s.reg.GetHibernationHandoff(context.Background(), d.Ref.Generation)
			if err != nil || job.BackupComplete {
				t.Fatalf("mismatched backup marked complete: %+v, %v", job, err)
			}
			if _, err := os.Stat(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestOwnedPublisherCleanupDoesNotBlockPeerReads(t *testing.T) {
	s, store, d, raw := seedOwnedHandoffBackup(t, false)
	ctx := context.Background()
	if err := s.backupHandoff(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.MarkHibernationHandoffPublished(ctx, d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	entered, unblock := make(chan struct{}), make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(unblock) }) }
	defer release()
	key := "chunksets/catalog/" + d.Ref.Generation + ".json"
	store.hook(func(_ *http.Request, name string) int {
		if name == key {
			close(entered)
			<-unblock
		}
		return 0
	})
	done := make(chan error, 1)
	go func() { done <- s.cleanupHandoffGeneration(ctx, d.Ref.Generation, time.Now()) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("cleanup did not reach remote release")
	}
	response := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		response <- retainedTestRequest(s, http.MethodGet, d.Ref.Generation, "", d.Manifest.Chunks[0].Hash)
	}()
	var blocked bool
	select {
	case got := <-response:
		if got.Code != http.StatusOK || !bytes.Equal(got.Body.Bytes(), raw[:4096]) {
			t.Errorf("peer bytes unavailable during cleanup: HTTP %d", got.Code)
		}
	case <-time.After(time.Second):
		blocked = true
	}
	release()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if blocked {
		<-response
		t.Fatal("cloud publisher cleanup blocked local peer fault read")
	}
}
