package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/google/uuid"
)

func writeRetainedTestBundle(t *testing.T, s *Server, id string) (*retainedHibernation, []byte) {
	t.Helper()
	generation := uuid.NewString()
	dir := filepath.Join(s.hibPeerDir(), generation)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte("generation-memory"), 1024)
	raw = raw[:8192]
	for name, data := range map[string][]byte{"mem.bin": raw, "state.bin": []byte("state"), "rootfs.ext4": bytes.Repeat([]byte("disk"), 2048)} {
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m, _, err := buildChunkManifest(filepath.Join(dir, "mem.bin"), 4096)
	if err != nil {
		t.Fatal(err)
	}
	digest, err := hibPeerManifestDigest(m)
	if err != nil {
		t.Fatal(err)
	}
	d := &retainedHibernation{
		Record:   hibRecord{ID: id, MemForm: memFormChunked, RootfsForm: rootfsFormFull},
		Manifest: *m,
		Ref:      hibPeerRef{URL: "http://10.0.0.1:8080", Generation: generation, ManifestSHA256: digest, ExpiresAtUnix: time.Now().Add(time.Hour).Unix()},
	}
	writeRetainedTestDescriptor(t, s, d)
	return d, raw
}

func writeRetainedTestDescriptor(t *testing.T, s *Server, d *retainedHibernation) {
	t.Helper()
	b, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(s.hibPeerDir(), d.Ref.Generation, "descriptor.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func retainedTestRequest(s *Server, method, generation, artifact, hash string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, "/", nil)
	r.SetPathValue("generation", generation)
	r.SetPathValue("artifact", artifact)
	r.SetPathValue("hash", hash)
	w := httptest.NewRecorder()
	s.handlePeerHibernation(w, r)
	return w
}

func TestPeerHibernationServesCapturedChunkAndRejectsMissing(t *testing.T) {
	s, _ := testLifecycleServer(t)
	d, raw := writeRetainedTestBundle(t, s, "sandbox")
	w := retainedTestRequest(s, http.MethodGet, d.Ref.Generation, "", d.Manifest.Chunks[1].Hash)
	if w.Code != 200 || !bytes.Equal(w.Body.Bytes(), raw[4096:]) {
		t.Fatalf("chunk response status=%d bytes=%d", w.Code, w.Body.Len())
	}
	if s.met.hibPeerServes.Load() != 1 || s.met.hibPeerServeBytes.Load() != 4096 {
		t.Fatal("raw chunk metrics do not report served bytes")
	}
	for _, hash := range []string{strings.Repeat("a", 64), "../../mem.bin", "garbage"} {
		if got := retainedTestRequest(s, http.MethodGet, d.Ref.Generation, "", hash).Code; got != 404 {
			t.Fatalf("missing hash %q: status %d", hash, got)
		}
	}
	if err := os.Remove(filepath.Join(s.hibPeerDir(), d.Ref.Generation, "mem.bin")); err != nil {
		t.Fatal(err)
	}
	if got := retainedTestRequest(s, http.MethodGet, d.Ref.Generation, "", d.Manifest.Chunks[0].Hash).Code; got != 404 {
		t.Fatalf("missing memory: status %d", got)
	}
}

func TestPeerHibernationSparseStateAndDisk(t *testing.T) {
	s, _ := testLifecycleServer(t)
	d, _ := writeRetainedTestBundle(t, s, "sandbox")
	for artifact, expected := range map[string][]byte{"state": []byte("state"), "rootfs": bytes.Repeat([]byte("disk"), 2048)} {
		w := retainedTestRequest(s, http.MethodGet, d.Ref.Generation, artifact, "")
		if w.Code != 200 {
			t.Fatalf("%s status %d", artifact, w.Code)
		}
		out := filepath.Join(t.TempDir(), artifact)
		if err := gcsblob.ReadSparse(bytes.NewReader(w.Body.Bytes()), out); err != nil {
			t.Fatal(err)
		}
		actual, err := os.ReadFile(out)
		if err != nil || !bytes.Equal(actual, expected) {
			t.Fatalf("%s bytes mismatch: %v", artifact, err)
		}
	}
	// The immutable descriptor carries diff ranges; serving does not consult a base registry row.
	d.Record.RootfsForm = rootfsFormDiff
	d.RootfsRanges = []gcsblob.Range{{Off: 4096, Len: 4096}}
	writeRetainedTestDescriptor(t, s, d)
	w := retainedTestRequest(s, http.MethodGet, d.Ref.Generation, "rootfs", "")
	out := filepath.Join(t.TempDir(), "diff")
	if err := os.WriteFile(out, bytes.Repeat([]byte("b"), 8192), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := gcsblob.ReadSparse(bytes.NewReader(w.Body.Bytes()), out); err != nil {
		t.Fatal(err)
	}
	actual, err := os.ReadFile(out)
	if err != nil || !bytes.Equal(actual[:4096], bytes.Repeat([]byte("b"), 4096)) || !bytes.Equal(actual[4096:], bytes.Repeat([]byte("disk"), 1024)) {
		t.Fatalf("diff sparse overlay mismatch: %v", err)
	}
}

func TestRemovePeerGenerationPreservesReturnedSandbox(t *testing.T) {
	s, reg := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	d, _ := writeRetainedTestBundle(t, s, "returned")
	rootfs := s.cfg.Provisioner.RootfsPathFor("returned")
	if err := os.WriteFile(rootfs, []byte("new mutable rootfs"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := reg.CreateStarting(context.Background(), "returned", "", rootfs, nil, "", 0, 1, 1); err != nil {
		t.Fatal(err)
	}
	mem, _, _, err := s.cfg.Provisioner.SnapshotPaths(hibID("returned"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(mem, []byte("new generation"), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := s.removePeerHibernation(d.Ref.Generation); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := reg.Get(context.Background(), "returned"); err != nil {
		t.Fatal("cleanup removed returned row", err)
	}
	for path, want := range map[string]string{rootfs: "new mutable rootfs", mem: "new generation"} {
		got, err := os.ReadFile(path)
		if err != nil || string(got) != want {
			t.Fatalf("cleanup changed %s: %q %v", path, got, err)
		}
	}
	if err := s.removePeerHibernation("../../"); err == nil {
		t.Fatal("accepted invalid generation")
	}
}

type blockedPeerWriter struct {
	*httptest.ResponseRecorder
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockedPeerWriter) Write(b []byte) (int, error) {
	w.once.Do(func() { close(w.entered); <-w.release })
	return w.ResponseRecorder.Write(b)
}

func TestPeerHibernationExpiryWaitsForActiveReader(t *testing.T) {
	s, _ := testLifecycleServer(t)
	d, raw := writeRetainedTestBundle(t, s, "sandbox")
	w := &blockedPeerWriter{ResponseRecorder: httptest.NewRecorder(), entered: make(chan struct{}), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(w.release) })
	r := httptest.NewRequest(http.MethodGet, "/", nil)
	r.SetPathValue("generation", d.Ref.Generation)
	r.SetPathValue("hash", d.Manifest.Chunks[0].Hash)
	readDone := make(chan struct{})
	go func() { defer close(readDone); s.handlePeerHibernation(w, r) }()
	select {
	case <-w.entered:
	case <-time.After(time.Second):
		t.Fatal("reader did not enter")
	}
	sweepDone := make(chan struct{})
	go func() { defer close(sweepDone); s.sweepPeerHibernations(time.Now().Add(2 * time.Hour)) }()
	select {
	case <-sweepDone:
		t.Fatal("expiry removed bundle during active response")
	case <-time.After(30 * time.Millisecond):
	}
	release.Do(func() { close(w.release) })
	select {
	case <-readDone:
	case <-time.After(time.Second):
		t.Fatal("reader did not finish")
	}
	select {
	case <-sweepDone:
	case <-time.After(time.Second):
		t.Fatal("sweep did not finish")
	}
	if !bytes.Equal(w.Body.Bytes(), raw[:4096]) {
		t.Fatal("active reader lost immutable bytes")
	}
	if _, err := os.Stat(filepath.Join(s.hibPeerDir(), d.Ref.Generation)); !os.IsNotExist(err) {
		t.Fatalf("expired bundle remains: %v", err)
	}
	if s.snapshotLocks.len() != 0 {
		t.Fatal("generation lock reference leaked")
	}
}

func TestPeerHibernationRestartSweepsTemporaryAndExpired(t *testing.T) {
	s, reg := testLifecycleServer(t)
	expired, _ := writeRetainedTestBundle(t, s, "expired")
	expired.Ref.ExpiresAtUnix = time.Now().Add(-time.Minute).Unix()
	writeRetainedTestDescriptor(t, s, expired)
	live, _ := writeRetainedTestBundle(t, s, "live")
	tmp := filepath.Join(s.hibPeerDir(), uuid.NewString()+".tmp")
	if err := os.MkdirAll(tmp, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tmp, "partial"), []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	restarted := New(s.cfg, reg)
	t.Cleanup(restarted.pf.CloseAll)
	restarted.sweepPeerHibernations(time.Now())
	for _, path := range []string{tmp, filepath.Join(s.hibPeerDir(), expired.Ref.Generation)} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("startup retained stale path %s: %v", path, err)
		}
	}
	if !restarted.hasRetainedHibernation("live") || restarted.hasRetainedHibernation("expired") {
		t.Fatal("release retry lookup failed")
	}
	if got := retainedTestRequest(restarted, http.MethodGet, live.Ref.Generation, "", "").Code; got != 200 {
		t.Fatalf("retained descriptor after restart: %d", got)
	}
}

func TestRetainHibernationClonesFrozenFiles(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("CloneFile uses Linux cp reflink options")
	}
	s, _ := testLifecycleServer(t)
	s.cfg.AdvertiseAddr = "http://10.0.0.1:8080"
	store := newUploadTestStore(t)
	s.blob = store.client
	mem, state, rootfs, err := s.cfg.Provisioner.SnapshotPaths(hibID("capture"))
	if err != nil {
		t.Fatal(err)
	}
	raw := bytes.Repeat([]byte("original"), 1024)
	for path, data := range map[string][]byte{mem: raw, state: []byte("state"), rootfs: []byte("rootfs")} {
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.uploadMemChunks(context.Background(), "capture", mem, 4096, nil); err != nil {
		t.Fatal(err)
	}
	rec := &hibRecord{ID: "capture", MemForm: memFormChunked, RootfsForm: rootfsFormFull, Peer: &hibPeerRef{Generation: "old"}}
	ref, err := s.retainHibernation(context.Background(), rec)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Peer.Generation != "old" {
		t.Fatal("retention mutated caller record")
	}
	for _, path := range []string{mem, state, rootfs} {
		if err := os.WriteFile(path, bytes.Repeat([]byte("changed"), 2048), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	w := retainedTestRequest(s, http.MethodGet, ref.Generation, "", "")
	var d retainedHibernation
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Record.Peer != nil {
		t.Fatal("retained descriptor kept stale peer hint")
	}
	w = retainedTestRequest(s, http.MethodGet, ref.Generation, "", d.Manifest.Chunks[0].Hash)
	if !bytes.Equal(w.Body.Bytes(), raw[:4096]) {
		t.Fatal("source mutation changed retained chunk")
	}
}

func TestReconcileReleasedHibernationArtifacts(t *testing.T) {
	s, reg := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	orphan, returned := uuid.NewString(), uuid.NewString()
	for _, id := range []string{orphan, returned, "not-a-uuid"} {
		writeRetainedTestBundle(t, s, id)
		mem, _, _, err := s.cfg.Provisioner.SnapshotPaths(hibID(id))
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{mem, s.cfg.Provisioner.RootfsPathFor(id)} {
			if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
				t.Fatal(err)
			}
		}
	}
	if _, err := reg.CreateStarting(context.Background(), returned, "", s.cfg.Provisioner.RootfsPathFor(returned), nil, "", 0, 1, 1); err != nil {
		t.Fatal(err)
	}
	s.reconcileReleasedHibernationArtifacts(context.Background())
	for _, id := range []string{orphan, returned, "not-a-uuid"} {
		for _, path := range []string{filepath.Join(s.cfg.Provisioner.SnapshotDir, hibID(id), "mem.bin"), s.cfg.Provisioner.RootfsPathFor(id)} {
			b, err := os.ReadFile(path)
			if id == orphan {
				if !os.IsNotExist(err) {
					t.Fatalf("orphan %s remains: %v", path, err)
				}
			} else if err != nil || string(b) != id {
				t.Fatalf("protected %s changed: %q %v", path, b, err)
			}
		}
		if !s.hasRetainedHibernation(id) {
			t.Fatalf("startup removed retained copy %s", id)
		}
	}
	if _, err := reg.Get(context.Background(), returned); err != nil {
		t.Fatal("returned row removed", err)
	}
	if s.snapshotLocks.len() != 0 {
		t.Fatal("generation lock reference leaked")
	}
}

func TestReconcileReleasedHibernationRequiresMissingRowProof(t *testing.T) {
	s, reg := testLifecycleServer(t)
	s.cfg.Provisioner.RootfsDir = t.TempDir()
	id := uuid.NewString()
	writeRetainedTestBundle(t, s, id)
	path := s.cfg.Provisioner.RootfsPathFor(id)
	if err := os.WriteFile(path, []byte("preserve on registry error"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	s.reconcileReleasedHibernationArtifacts(context.Background())
	b, err := os.ReadFile(path)
	if err != nil || string(b) != "preserve on registry error" {
		t.Fatalf("registry failure permitted deletion: %q %v", b, err)
	}
}

func TestPeerHibernationStatsCountRetainedLogicalBytes(t *testing.T) {
	s, _ := testLifecycleServer(t)
	d, _ := writeRetainedTestBundle(t, s, "first")
	writeRetainedTestBundle(t, s, "second")
	if err := os.MkdirAll(filepath.Join(s.hibPeerDir(), uuid.NewString()+".tmp"), 0o700); err != nil {
		t.Fatal(err)
	}
	count, size := s.peerHibernationStats()
	if count != 2 || size != 2*(8192+5+8192) {
		t.Fatalf("retained stats = %d, %d", count, size)
	}
	if err := s.removePeerHibernation(d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
	count, size = s.peerHibernationStats()
	if count != 1 || size != 8192+5+8192 {
		t.Fatalf("stats after cleanup = %d, %d", count, size)
	}
}
