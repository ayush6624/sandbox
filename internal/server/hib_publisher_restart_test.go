package server

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

type handoffPublisherChildConfig struct {
	RegistryPath string
	Pools        registry.Pools
	SnapshotDir  string
	Endpoint     string
	Generation   string
}

func TestHandoffPublisherRestartChild(t *testing.T) {
	encoded := os.Getenv("SANDBOX_HANDOFF_PUBLISHER_RESTART_CHILD")
	if encoded == "" {
		return
	}
	var cfg handoffPublisherChildConfig
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
	s := New(Config{Provisioner: &provisioner.Provisioner{SnapshotDir: cfg.SnapshotDir}}, reg)
	s.blob = gcsblob.NewWithHTTPClient("upload-test", &http.Client{
		Transport: uploadTestTransport{target: endpoint, base: http.DefaultTransport},
	})
	done := make(chan error, 1)
	go func() { done <- s.backupHandoff(context.Background(), cfg.Generation) }()
	in := bufio.NewScanner(os.Stdin)
	if !in.Scan() || in.Text() != "close-registry" {
		t.Fatalf("expected close-registry command: %v", in.Err())
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		t.Fatalf("backup returned before registry closed: %v", err)
	default:
	}
	if err := json.NewEncoder(os.Stdout).Encode("registry-closed"); err != nil {
		t.Fatal(err)
	}
	if !in.Scan() || in.Text() != "exit" {
		t.Fatalf("expected exit command: %v", in.Err())
	}
	// The blocked upload and its attempt never run their deferred cleanup.
	os.Exit(0)
}

func TestOwnedHandoffPublisherRecoveryAcrossProcessExit(t *testing.T) {
	ctx := context.Background()
	original, store, descriptor, raw := seedOwnedHandoffBackup(t, true)
	endpoint := httptest.NewServer(http.HandlerFunc(store.serveHTTP))
	t.Cleanup(endpoint.Close)
	cfg := handoffPublisherChildConfig{
		RegistryPath: original.reg.Path(), Pools: original.reg.Pools(),
		SnapshotDir: original.cfg.Provisioner.SnapshotDir,
		Endpoint:    endpoint.URL, Generation: descriptor.Ref.Generation,
	}
	if err := original.reg.Close(); err != nil {
		t.Fatal(err)
	}
	entered, unblock, returned := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(unblock) }) }
	t.Cleanup(release)
	store.hook(func(r *http.Request, name string) int {
		if name == descriptor.artifactObject("record.json") && r.URL.Query().Get("ifGenerationMatch") != "0" {
			close(entered)
			<-unblock
			close(returned)
			return http.StatusForbidden
		}
		return 0
	})
	encoded, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	childCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	t.Cleanup(cancel)
	cmd := exec.CommandContext(childCtx, os.Args[0], "-test.run=^TestHandoffPublisherRestartChild$")
	cmd.Env = append(os.Environ(), "SANDBOX_HANDOFF_PUBLISHER_RESTART_CHILD="+string(encoded))
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
			_ = cmd.Wait()
		}
	})
	select {
	case <-entered:
	case <-childCtx.Done():
		t.Fatal("child did not reach the final payload upload")
	}
	partial := make(map[string]int64)
	for _, name := range []string{descriptor.Manifest.chunkObject(descriptor.Manifest.Chunks[0].Hash), descriptor.artifactObject("state.sz"), descriptor.artifactObject("rootfs.sz")} {
		object, ok := uploadObject(store, name)
		if !ok || len(object.data) == 0 {
			t.Fatalf("child left no useful payload at %s", name)
		}
		partial[name] = object.generation
	}
	reg, err := registry.Open(cfg.RegistryPath, cfg.Pools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	s := New(original.cfg, reg)
	s.blob = store.client
	t.Cleanup(s.pf.CloseAll)
	assertBusy := func(stage string) {
		t.Helper()
		if err := s.backupHandoff(ctx, cfg.Generation); !errors.Is(err, registry.ErrHandoffBusy) {
			t.Fatalf("%s admitted competing backup: %v", stage, err)
		}
		catalog, _ := s.chunkStorage()
		view, err := catalog.Inspect(ctx, cfg.Generation)
		if err != nil || view.Publisher.Released {
			t.Fatalf("%s released active publisher: %+v, %v", stage, view.Publisher, err)
		}
		job, err := reg.GetHibernationHandoff(ctx, cfg.Generation)
		if err != nil || job.BackupComplete {
			t.Fatalf("%s completed active backup: %+v, %v", stage, job, err)
		}
	}
	assertBusy("open child registry")
	if _, err := fmt.Fprintln(stdin, "close-registry"); err != nil {
		t.Fatal(err)
	}
	var event string
	if err := json.NewDecoder(stdout).Decode(&event); err != nil || event != "registry-closed" {
		t.Fatalf("child close event: %q, %v", event, err)
	}
	assertBusy("closed child registry")
	if _, err := fmt.Fprintln(stdin, "exit"); err != nil {
		t.Fatal(err)
	}
	if err := cmd.Wait(); err != nil {
		waited = true
		t.Fatalf("child exit: %v: %s", err, stderr.String())
	}
	waited = true
	release()
	<-returned
	store.hook(nil)
	s = restartChunkRetirementServer(t, s, store)
	if err := s.backupHandoff(ctx, cfg.Generation); err != nil {
		t.Fatal(err)
	}
	for name, generation := range partial {
		if object, _ := uploadObject(store, name); object.generation != generation {
			t.Fatalf("restart rewrote immutable payload %s", name)
		}
	}
	job, err := s.reg.GetHibernationHandoff(ctx, cfg.Generation)
	if err != nil || !job.BackupComplete {
		t.Fatalf("restart did not finish retained job: %+v, %v", job, err)
	}
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(ctx, cfg.Generation)
	if err != nil || !view.Publisher.Released {
		t.Fatalf("restart retained completed publisher: %+v, %v", view.Publisher, err)
	}
	staged, err := s.reconstructHandoff(ctx, descriptor, filepath.Join(t.TempDir(), "restored.ext4"))
	if err != nil {
		t.Fatal(err)
	}
	defer staged.Close()
	got, err := os.ReadFile(staged.MemPath)
	if err != nil || !bytes.Equal(got, raw) {
		t.Fatalf("restart restored %d incorrect bytes: %v", len(got), err)
	}
}

func TestOwnedHandoffCompletedRestartNeverReadmitsPayloads(t *testing.T) {
	ctx := context.Background()
	s, store, descriptor, _ := seedOwnedHandoffBackup(t, false)
	if err := s.backupHandoff(ctx, descriptor.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(s.hibPeerDir(), descriptor.Ref.Generation, "mem.bin")); err != nil {
		t.Fatal(err)
	}
	payloadAttempts := func() int {
		store.mu.Lock()
		defer store.mu.Unlock()
		total := 0
		for name, count := range store.puts {
			if strings.HasPrefix(name, chunkstore.DataPrefix(descriptor.Ref.Generation)) {
				total += count
			}
		}
		return total
	}
	before := payloadAttempts()
	if err := s.reg.MarkHibernationHandoffPublished(ctx, descriptor.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	catalogKey := "chunksets/catalog/" + descriptor.Ref.Generation + ".json"
	store.hook(func(_ *http.Request, name string) int {
		if name == catalogKey {
			return http.StatusForbidden
		}
		return 0
	})
	s = restartChunkRetirementServer(t, s, store)
	if err := s.backupHandoff(ctx, descriptor.Ref.Generation); err == nil {
		t.Fatal("failed publisher release was accepted")
	}
	store.hook(nil)
	for _, stage := range []string{"completion persisted, release pending", "release already committed"} {
		s = restartChunkRetirementServer(t, s, store)
		if err := s.backupHandoff(ctx, descriptor.Ref.Generation); err != nil {
			t.Fatalf("%s: %v", stage, err)
		}
		if got := payloadAttempts(); got != before {
			t.Fatalf("%s readmitted payload attempts: %d -> %d", stage, before, got)
		}
		catalog, _ := s.chunkStorage()
		view, err := catalog.Inspect(ctx, descriptor.Ref.Generation)
		if err != nil || !view.Publisher.Released {
			t.Fatalf("%s left publisher active: %+v, %v", stage, view.Publisher, err)
		}
	}
}
