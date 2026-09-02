package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/management"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

func peerTestRegistry(t *testing.T, dir string) *registry.Registry {
	t.Helper()
	reg, err := registry.Open(filepath.Join(dir, "registry.db"), registry.Pools{
		TapPrefix: "fc", TapMax: 2,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.11",
		PortMin: 5200, PortMax: 5201,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	return reg
}

func writePeerArtifact(t *testing.T, path string, size int64, off int64, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.Truncate(size); err == nil {
		_, err = f.WriteAt([]byte(data), off)
	}
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
}

func TestPeerSnapshotPullSingleFlightsAndPreservesArtifacts(t *testing.T) {
	const token = "worker-secret"
	creds, err := management.NewCredentials([]string{token}, "")
	if err != nil {
		t.Fatal(err)
	}

	sourceDir := t.TempDir()
	sourceReg := peerTestRegistry(t, sourceDir)
	sourceProvisioner := &provisioner.Provisioner{SnapshotDir: filepath.Join(sourceDir, "snapshots")}
	mem, state, rootfs, err := sourceProvisioner.SnapshotPaths("snap-peer")
	if err != nil {
		t.Fatal(err)
	}
	writePeerArtifact(t, mem, 2<<20, 1<<20, "memory-pages")
	writePeerArtifact(t, state, 64<<10, 8<<10, "device-state")
	writePeerArtifact(t, rootfs, 4<<20, 3<<20, "rootfs-blocks")
	snap := registry.Snapshot{
		ID: "snap-peer", SourceID: "source", MemPath: mem, StatePath: state,
		RootfsPath: rootfs, CreatedAt: time.Now(), Format: registry.FormatFull,
		Vcpus: 2, MemMIB: 1024, Durability: "local",
	}
	if err := sourceReg.CreateSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	source := New(Config{Provisioner: sourceProvisioner}, sourceReg)
	t.Cleanup(source.pf.CloseAll)

	var requests atomic.Int64
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/v1/snapshots/{id}", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		source.handlePeerSnapshotMeta(w, r)
	})
	mux.HandleFunc("GET /internal/v1/snapshots/{id}/{artifact}", func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		source.handlePeerSnapshotArtifact(w, r)
	})
	peer := httptest.NewServer(bearerAuth(nil, creds, mux))
	t.Cleanup(peer.Close)

	targetDir := t.TempDir()
	targetReg := peerTestRegistry(t, targetDir)
	target := New(Config{Provisioner: &provisioner.Provisioner{SnapshotDir: filepath.Join(targetDir, "snapshots")}}, targetReg)
	target.workerCredentials = creds
	t.Cleanup(target.pf.CloseAll)

	const callers = 8
	rows := make([]registry.Snapshot, callers)
	errs := make([]error, callers)
	var wg sync.WaitGroup
	for index := range rows {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rows[index], errs[index] = target.ensureSnapshotLocalFrom(context.Background(), snap.ID, peer.URL)
		}()
	}
	wg.Wait()
	for index, err := range errs {
		if err != nil {
			t.Fatalf("pull %d: %v", index, err)
		}
	}
	if got := requests.Load(); got != 4 {
		t.Fatalf("peer requests = %d, want one metadata + three artifacts", got)
	}
	if got := target.met.snapshotPeerPulls.Load(); got != 1 {
		t.Fatalf("peer pulls = %d, want 1", got)
	}
	for _, check := range []struct {
		path string
		off  int64
		want string
	}{
		{rows[0].MemPath, 1 << 20, "memory-pages"},
		{rows[0].StatePath, 8 << 10, "device-state"},
		{rows[0].RootfsPath, 3 << 20, "rootfs-blocks"},
	} {
		f, err := os.Open(check.path)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(check.want))
		_, err = f.ReadAt(got, check.off)
		_ = f.Close()
		if err != nil || string(got) != check.want {
			t.Fatalf("artifact %s @%d = %q, %v; want %q", check.path, check.off, got, err, check.want)
		}
	}
}

func TestPeerSnapshotCorruptionCleansUpAndCountsFailure(t *testing.T) {
	creds, err := management.NewCredentials([]string{"worker-secret"}, "")
	if err != nil {
		t.Fatal(err)
	}
	meta := registry.Snapshot{ID: "snap-bad", Format: registry.FormatFull, CreatedAt: time.Now()}
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/internal/v1/snapshots/snap-bad" {
			_ = json.NewEncoder(w).Encode(meta)
			return
		}
		_, _ = w.Write([]byte("not-a-sparse-stream"))
	}))
	t.Cleanup(peer.Close)

	dir := t.TempDir()
	reg := peerTestRegistry(t, dir)
	p := &provisioner.Provisioner{SnapshotDir: filepath.Join(dir, "snapshots")}
	target := New(Config{Provisioner: p}, reg)
	target.workerCredentials = creds
	t.Cleanup(target.pf.CloseAll)

	_, err = target.ensureSnapshotLocalFrom(context.Background(), meta.ID, peer.URL)
	if err == nil {
		t.Fatal("corrupt peer stream unexpectedly succeeded")
	}
	if got := target.met.snapshotPeerFailures.Load(); got != 1 {
		t.Fatalf("peer failures = %d, want 1", got)
	}
	if _, err := reg.GetSnapshot(context.Background(), meta.ID); err == nil {
		t.Fatal("corrupt peer pull created a registry row")
	}
	if _, err := os.Stat(filepath.Join(p.SnapshotDir, meta.ID, "mem.bin.peer.tmp")); !os.IsNotExist(err) {
		t.Fatalf("temporary artifact survived corrupt pull: %v", err)
	}
}

func TestPeerDiffSnapshotStagesLocalBaseBeforeOverlay(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("diff snapshot staging uses Linux reflink and FIEMAP")
	}
	const token = "worker-secret"
	creds, err := management.NewCredentials([]string{token}, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	sourceDir := t.TempDir()
	sourceReg := peerTestRegistry(t, sourceDir)
	sourceProvisioner := &provisioner.Provisioner{SnapshotDir: filepath.Join(sourceDir, "snapshots")}
	baseMem := filepath.Join(sourceDir, "base.mem")
	baseRootfs := filepath.Join(sourceDir, "base.ext4")
	writePeerArtifact(t, baseMem, 2<<20, 64<<10, "base-memory")
	writePeerArtifact(t, baseRootfs, 4<<20, 128<<10, "base-rootfs")
	base := registry.Snapshot{
		ID: "golden-peer", SourceID: "golden", MemPath: baseMem,
		StatePath: filepath.Join(sourceDir, "base.state"), RootfsPath: baseRootfs,
		CreatedAt: time.Now(), Golden: true, Format: registry.FormatFull,
	}
	if err := sourceReg.CreateSnapshot(ctx, base); err != nil {
		t.Fatal(err)
	}
	mem, state, rootfs, err := sourceProvisioner.SnapshotPaths("snap-diff")
	if err != nil {
		t.Fatal(err)
	}
	writePeerArtifact(t, mem, 2<<20, 1<<20, "dirty-memory")
	writePeerArtifact(t, state, 64<<10, 4<<10, "diff-state")
	if err := provisioner.CloneFile(baseRootfs, rootfs); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(rootfs, os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteAt([]byte("changed-rootfs"), 3<<20)
	_ = f.Close()
	if err != nil {
		t.Fatal(err)
	}
	diff := registry.Snapshot{
		ID: "snap-diff", SourceID: "source", MemPath: mem, StatePath: state,
		RootfsPath: rootfs, CreatedAt: time.Now(), Format: registry.FormatDiff,
		BaseID: base.ID, Vcpus: 2, MemMIB: 1024,
	}
	if err := sourceReg.CreateSnapshot(ctx, diff); err != nil {
		t.Fatal(err)
	}
	source := New(Config{Provisioner: sourceProvisioner}, sourceReg)
	t.Cleanup(source.pf.CloseAll)
	mux := http.NewServeMux()
	mux.HandleFunc("GET /internal/v1/snapshots/{id}", source.handlePeerSnapshotMeta)
	mux.HandleFunc("GET /internal/v1/snapshots/{id}/{artifact}", source.handlePeerSnapshotArtifact)
	peer := httptest.NewServer(bearerAuth(nil, creds, mux))
	t.Cleanup(peer.Close)

	targetDir := t.TempDir()
	targetReg := peerTestRegistry(t, targetDir)
	targetBaseMem := filepath.Join(targetDir, "base.mem")
	targetBaseRootfs := filepath.Join(targetDir, "base.ext4")
	writePeerArtifact(t, targetBaseMem, 2<<20, 64<<10, "base-memory")
	writePeerArtifact(t, targetBaseRootfs, 4<<20, 128<<10, "base-rootfs")
	targetBase := base
	targetBase.MemPath, targetBase.RootfsPath = targetBaseMem, targetBaseRootfs
	if err := targetReg.CreateSnapshot(ctx, targetBase); err != nil {
		t.Fatal(err)
	}
	target := New(Config{Provisioner: &provisioner.Provisioner{SnapshotDir: filepath.Join(targetDir, "snapshots")}}, targetReg)
	target.workerCredentials = creds
	t.Cleanup(target.pf.CloseAll)

	pulled, err := target.ensureSnapshotLocalFrom(ctx, diff.ID, peer.URL)
	if err != nil {
		t.Fatal(err)
	}
	if pulled.Format != registry.FormatDiff || pulled.BaseID != base.ID {
		t.Fatalf("pulled lineage = format %q base %q", pulled.Format, pulled.BaseID)
	}
	for _, check := range []struct {
		off  int64
		want string
	}{
		{128 << 10, "base-rootfs"},
		{3 << 20, "changed-rootfs"},
	} {
		f, err := os.Open(pulled.RootfsPath)
		if err != nil {
			t.Fatal(err)
		}
		got := make([]byte, len(check.want))
		_, err = f.ReadAt(got, check.off)
		_ = f.Close()
		if err != nil || string(got) != check.want {
			t.Fatalf("rootfs @%d = %q, %v; want %q", check.off, got, err, check.want)
		}
	}
}

func TestSnapshotPeerBaseRejectsPublicAndDecoratedURLs(t *testing.T) {
	for _, raw := range []string{
		"https://8.8.8.8:8080",
		"http://worker.internal:8080",
		"http://10.0.0.2:8080/path",
		"http://user@10.0.0.2:8080",
		"file:///tmp/snapshot",
	} {
		if _, err := snapshotPeerBase(raw); err == nil {
			t.Fatalf("snapshotPeerBase(%q) unexpectedly succeeded", raw)
		}
	}
	for _, raw := range []string{"http://10.0.0.2:8080", "http://127.0.0.1:1234", "https://[fd00::1]:8080"} {
		if _, err := snapshotPeerBase(raw); err != nil {
			t.Fatalf("snapshotPeerBase(%q): %v", raw, err)
		}
	}
}

func TestPeerSnapshotClientDoesNotFollowRedirects(t *testing.T) {
	var redirected atomic.Int64
	destination := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		redirected.Add(1)
	}))
	t.Cleanup(destination.Close)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, destination.URL, http.StatusTemporaryRedirect)
	}))
	t.Cleanup(peer.Close)

	req, err := http.NewRequest(http.MethodGet, peer.URL, nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := peerSnapshotClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusTemporaryRedirect {
		t.Fatalf("status = %d, want redirect response", resp.StatusCode)
	}
	if redirected.Load() != 0 {
		t.Fatal("peer client followed a redirect and leaked the request boundary")
	}
}
