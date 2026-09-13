package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
)

type chunkReaderChildConfig struct {
	RegistryPath string
	Pools        registry.Pools
	Endpoint     string
	SetID        string
	RootID       string
}

type chunkReaderChildEvent struct {
	Stage    string
	Identity chunkstore.ReaderIdentity
}

func TestChunkReaderRestartChild(t *testing.T) {
	encoded := os.Getenv("SANDBOX_CHUNK_READER_RESTART_CHILD")
	if encoded == "" {
		return
	}
	var cfg chunkReaderChildConfig
	if err := json.Unmarshal([]byte(encoded), &cfg); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(cfg.RegistryPath, cfg.Pools)
	if err != nil {
		t.Fatal(err)
	}
	endpoint, err := url.Parse(cfg.Endpoint)
	if err != nil {
		t.Fatal(err)
	}
	blob := gcsblob.NewWithHTTPClient("upload-test", &http.Client{
		Transport: uploadTestTransport{target: endpoint, base: http.DefaultTransport},
	})
	ctx := context.Background()
	owner, err := reg.ChunkReaderOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	reader, err := chunkstore.New(blob).PrepareReader(cfg.SetID, cfg.RootID, owner)
	if err != nil {
		t.Fatal(err)
	}
	if err := reg.QueueChunkReader(ctx, reader.Identity()); err != nil {
		t.Fatal(err)
	}
	if err := reader.Acquire(ctx); err != nil {
		t.Fatal(err)
	}
	out := json.NewEncoder(os.Stdout)
	if err := out.Encode(chunkReaderChildEvent{Stage: "acquired", Identity: reader.Identity()}); err != nil {
		t.Fatal(err)
	}
	in := bufio.NewScanner(os.Stdin)
	if !in.Scan() || in.Text() != "close-registry" {
		t.Fatalf("expected close-registry command: %v", in.Err())
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	if err := out.Encode(chunkReaderChildEvent{Stage: "registry-closed", Identity: reader.Identity()}); err != nil {
		t.Fatal(err)
	}
	if !in.Scan() || in.Text() != "exit" {
		t.Fatalf("expected exit command: %v", in.Err())
	}
	runtime.KeepAlive(reader)
	// Exit without releasing the remote reader or running registry cleanup.
	os.Exit(0)
}

func TestChunkReaderRecoveryAcrossProcessExit(t *testing.T) {
	ctx := context.Background()
	original, store, descriptor, job := ownedControlFixture(t)
	if err := original.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(store.serveHTTP))
	t.Cleanup(endpoint.Close)
	cfg := chunkReaderChildConfig{
		RegistryPath: original.reg.Path(), Pools: original.reg.Pools(),
		Endpoint: endpoint.URL, SetID: descriptor.Ref.Generation,
		RootID: descriptor.Manifest.Storage.RootID,
	}
	if err := original.reg.Close(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestChunkReaderRestartChild$")
	cmd.Env = append(os.Environ(), "SANDBOX_CHUNK_READER_RESTART_CHILD="+string(encoded))
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waited := false
	t.Cleanup(func() {
		_ = stdin.Close()
		if !waited {
			cancel()
			if err := cmd.Wait(); err != nil {
				t.Logf("reader child cleanup: %v: %s", err, stderr.String())
			}
		}
	})
	decoder := json.NewDecoder(stdout)
	var acquired chunkReaderChildEvent
	if err := decoder.Decode(&acquired); err != nil || acquired.Stage != "acquired" {
		t.Fatalf("child acquisition event: %+v, %v", acquired, err)
	}
	reg, err := registry.Open(cfg.RegistryPath, cfg.Pools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := New(original.cfg, reg)
	s.blob = store.client
	t.Cleanup(s.pf.CloseAll)
	catalog, _ := s.chunkStorage()
	key := "chunksets/catalog/" + cfg.SetID + ".json"
	assertProtected := func(stage string) {
		t.Helper()
		before := store.putCount(key)
		// One row may be behind the previous pass's cursor; the empty page wraps.
		s.recoverChunkReaders(ctx)
		s.recoverChunkReaders(ctx)
		rows, err := s.reg.ListChunkReaders(ctx, "", 32)
		if err != nil || len(rows) != 1 || rows[0] != acquired.Identity {
			t.Fatalf("%s lost exact durable obligation: %+v, %v", stage, rows, err)
		}
		view, err := catalog.Inspect(ctx, cfg.SetID)
		if err != nil || len(view.Readers) != 1 || view.Readers[0].ID != acquired.Identity.ReaderID || view.Readers[0].OwnerID != acquired.Identity.OwnerID || view.Readers[0].RootID != acquired.Identity.RootID {
			t.Fatalf("%s released live child reader: %+v, %v", stage, view, err)
		}
		if store.putCount(key) != before {
			t.Fatalf("%s attempted a remote reader fence while child was alive", stage)
		}
	}
	assertProtected("open child registry")
	if _, err := fmt.Fprintln(stdin, "close-registry"); err != nil {
		t.Fatal(err)
	}
	var closed chunkReaderChildEvent
	if err := decoder.Decode(&closed); err != nil || closed.Stage != "registry-closed" || closed.Identity != acquired.Identity {
		t.Fatalf("child registry-close event: %+v, %v", closed, err)
	}
	assertProtected("closed child registry")
	if _, err := fmt.Fprintln(stdin, "exit"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		waited = true
		t.Fatalf("reader child exit: %v: %s", err, stderr.String())
	}
	waited = true
	// Reopen the server too, so recovery has no surviving in-memory handle.
	s = restartChunkRetirementServer(t, s, store)
	catalog, _ = s.chunkStorage()
	store.hook(func(_ *http.Request, name string) int {
		if name == key {
			return http.StatusForbidden
		}
		return 0
	})
	s.recoverChunkReaders(ctx)
	pending, err := s.reg.ListChunkReaders(ctx, "", 32)
	if err != nil || len(pending) != 1 || pending[0] != acquired.Identity {
		t.Fatalf("failed recovery fence lost obligation: %+v, %v", pending, err)
	}
	s.chunkOwnership.mu.Lock()
	cursor := s.chunkOwnership.recoveryCursor
	s.chunkOwnership.mu.Unlock()
	if cursor != acquired.Identity.ReaderID {
		t.Fatalf("failed recovery did not advance cursor: %q", cursor)
	}
	store.hook(nil)
	before := store.putCount(key)
	// The first call wraps the cursor; the second retries the durable row.
	s.recoverChunkReaders(ctx)
	s.recoverChunkReaders(ctx)
	rows, err := s.reg.ListChunkReaders(ctx, "", 32)
	if err != nil || len(rows) != 0 {
		t.Fatalf("process-exit recovery retained journal: %+v, %v", rows, err)
	}
	view, err := catalog.Inspect(ctx, cfg.SetID)
	if err != nil || len(view.Readers) != 0 || len(view.Roots) != 1 || view.Roots[0].Retired || view.Publisher.Released || view.Phase != chunkstore.Live {
		t.Fatalf("process-exit recovery changed unrelated holders: %+v, %v", view, err)
	}
	if got := store.putCount(key); got != before+1 {
		t.Fatalf("process-exit recovery did not confirm one remote fence: %d -> %d", before, got)
	}
}
