package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
)

type uploadTestObject struct {
	data       []byte
	generation int64
}

type uploadTestStore struct {
	mu         sync.Mutex
	objects    map[string]uploadTestObject
	puts       map[string]int
	writes     []string
	generation int64
	beforePut  func(*http.Request, string) int
	beforeGet  func(*http.Request, string)
	client     *gcsblob.Client
}

type uploadTestTransport struct {
	target *url.URL
	base   http.RoundTripper
}

func (tr uploadTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rewritten := req.Clone(req.Context())
	rewritten.URL.Scheme = tr.target.Scheme
	rewritten.URL.Host = tr.target.Host
	return tr.base.RoundTrip(rewritten)
}

func newUploadTestStore(t *testing.T) *uploadTestStore {
	t.Helper()
	store := &uploadTestStore{objects: map[string]uploadTestObject{}, puts: map[string]int{}}
	srv := httptest.NewServer(http.HandlerFunc(store.serveHTTP))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	t.Cleanup(transport.CloseIdleConnections)
	store.client = gcsblob.NewWithHTTPClient("upload-test", &http.Client{
		Transport: uploadTestTransport{target: target, base: transport},
	})
	return store
}

func (store *uploadTestStore) serveHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/service-accounts/default/token") {
		_, _ = io.WriteString(w, `{"access_token":"test-token","expires_in":3600}`)
		return
	}
	if r.Method == http.MethodPost {
		name := r.URL.Query().Get("name")
		data, err := io.ReadAll(r.Body)
		if err != nil {
			http.Error(w, err.Error(), 400)
			return
		}
		store.mu.Lock()
		store.puts[name]++
		hook := store.beforePut
		store.mu.Unlock()
		if hook != nil {
			if status := hook(r, name); status != 0 {
				w.WriteHeader(status)
				return
			}
		}
		store.mu.Lock()
		defer store.mu.Unlock()
		if want := r.URL.Query().Get("ifGenerationMatch"); want != "" {
			generation, err := strconv.ParseInt(want, 10, 64)
			if err != nil {
				w.WriteHeader(400)
				return
			}
			if store.objects[name].generation != generation {
				w.WriteHeader(412)
				return
			}
		}
		store.generation++
		store.objects[name] = uploadTestObject{data: data, generation: store.generation}
		store.writes = append(store.writes, name)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"generation":"%d"}`, store.generation)
		return
	}
	if r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/o") {
		store.mu.Lock()
		defer store.mu.Unlock()
		var names []string
		for name := range store.objects {
			if strings.HasPrefix(name, r.URL.Query().Get("prefix")) {
				names = append(names, name)
			}
		}
		sort.Strings(names)
		items := make([]map[string]string, 0, len(names))
		for _, name := range names {
			obj := store.objects[name]
			items = append(items, map[string]string{"name": name, "generation": strconv.FormatInt(obj.generation, 10), "size": strconv.Itoa(len(obj.data))})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"items": items})
		return
	}
	_, name, ok := strings.Cut(r.URL.Path, "/o/")
	if !ok {
		w.WriteHeader(404)
		return
	}
	store.mu.Lock()
	beforeGet := store.beforeGet
	store.mu.Unlock()
	if r.Method == http.MethodGet && beforeGet != nil {
		beforeGet(r, name)
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	obj, exists := store.objects[name]
	if !exists {
		w.WriteHeader(404)
		return
	}
	if want := r.URL.Query().Get("ifGenerationMatch"); want != "" && want != strconv.FormatInt(obj.generation, 10) {
		w.WriteHeader(412)
		return
	}
	if r.Method == http.MethodDelete {
		delete(store.objects, name)
		w.WriteHeader(204)
		return
	}
	if r.Method == http.MethodGet && r.URL.Query().Get("alt") != "media" {
		_ = json.NewEncoder(w).Encode(map[string]string{"name": name, "generation": strconv.FormatInt(obj.generation, 10), "size": strconv.Itoa(len(obj.data))})
		return
	}
	w.Header().Set("X-Goog-Generation", strconv.FormatInt(obj.generation, 10))
	_, _ = w.Write(obj.data)
}

func (store *uploadTestStore) hook(fn func(*http.Request, string) int) {
	store.mu.Lock()
	store.beforePut = fn
	store.mu.Unlock()
}

func (store *uploadTestStore) putCount(name string) int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.puts[name]
}

func seedUploadSnapshot(t *testing.T, s *Server, id string) registry.Snapshot {
	t.Helper()
	mem, state, rootfs, err := s.cfg.Provisioner.SnapshotPaths(id)
	if err != nil {
		t.Fatal(err)
	}
	for i, path := range []string{mem, state, rootfs} {
		data := append(bytes.Repeat([]byte{byte(i + 1)}, 512), make([]byte, 8192)...)
		data = append(data, []byte("snapshot-preserved-tail")...)
		if err := os.WriteFile(path, data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	snap := registry.Snapshot{ID: id, SourceID: "terminated-source", MemPath: mem, StatePath: state, RootfsPath: rootfs, CreatedAt: time.Now(), Format: registry.FormatFull}
	if err := s.reg.CreateCapturedSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	return snap
}

func awaitUploadCondition(t *testing.T, check func() bool) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for !check() {
		if time.Now().After(deadline) {
			t.Fatal("snapshot upload condition timed out")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func runUploadAttempt(t *testing.T, s *Server, id string) {
	t.Helper()
	if err := s.startSnapshotUpload(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	awaitUploadCondition(t, func() bool {
		s.snapshotUpMu.Lock()
		defer s.snapshotUpMu.Unlock()
		return s.snapshotUploads[id] == nil
	})
}

func startUploadDispatcher(t *testing.T, s *Server) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); s.runSnapshotUploads(ctx) }()
	stop := func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("upload dispatcher did not join on cancellation")
		}
	}
	t.Cleanup(stop)
	return stop
}

func assertSnapshotPulled(t *testing.T, store *uploadTestStore, source registry.Snapshot) {
	t.Helper()
	target, _ := testLifecycleServer(t)
	target.blob = store.client
	pulled, err := target.ensureSnapshotLocal(context.Background(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if pulled.Durability != "durable" || pulled.Upload != nil {
		t.Fatalf("pulled metadata = %+v", pulled)
	}
	for _, pair := range [][2]string{{source.MemPath, pulled.MemPath}, {source.StatePath, pulled.StatePath}, {source.RootfsPath, pulled.RootfsPath}} {
		want, err := os.ReadFile(pair[0])
		if err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(got, want) {
			t.Fatalf("pulled artifact %s differs", pair[1])
		}
	}
}

func TestSnapshotUploadRetriesAfterTransportExhaustion(t *testing.T) {
	store := newUploadTestStore(t)
	s, _ := testLifecycleServer(t)
	s.blob = store.client
	snap := seedUploadSnapshot(t, s, "retry")
	store.hook(func(_ *http.Request, name string) int {
		if name == snapObj(snap.ID, "rootfs.sz") {
			return 503
		}
		return 0
	})
	runUploadAttempt(t, s, snap.ID)
	failed, err := s.reg.GetSnapshot(context.Background(), snap.ID)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Upload == nil || failed.Upload.State != "retrying" || failed.Upload.Attempts != 1 || failed.Upload.NextAttemptAt == nil || failed.Durability != "local" {
		t.Fatalf("exhausted transport did not persist retry: %+v", failed)
	}
	if got := store.putCount(snapObj(snap.ID, "rootfs.sz")); got != 3 {
		t.Fatalf("transport attempts = %d, want 3", got)
	}
	if committed, err := s.snapshotCommitted(context.Background(), snap.ID); err != nil || committed {
		t.Fatalf("partial attempt committed: %v, %v", committed, err)
	}
	store.hook(nil)
	stop := startUploadDispatcher(t, s)
	awaitUploadCondition(t, func() bool {
		row, err := s.reg.GetSnapshot(context.Background(), snap.ID)
		return err == nil && row.Durability == "durable" && row.Upload == nil
	})
	stop()
	store.mu.Lock()
	writes := append([]string(nil), store.writes...)
	store.mu.Unlock()
	if len(writes) != 4 || writes[len(writes)-1] != snapObj(snap.ID, "meta.json") {
		t.Fatalf("commit ordering = %v", writes)
	}
	metadata, err := store.client.GetBytes(context.Background(), snapObj(snap.ID, "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	var committed registry.Snapshot
	if err := json.Unmarshal(metadata, &committed); err != nil || committed.Upload != nil {
		t.Fatalf("upload ownership leaked into transferable metadata: %s, %v", metadata, err)
	}
	assertSnapshotPulled(t, store, snap)
}

func TestSnapshotUploadDispatcherRecoversAfterRestart(t *testing.T) {
	store := newUploadTestStore(t)
	s, reg := testLifecycleServer(t)
	s.blob = store.client
	snap := seedUploadSnapshot(t, s, "restart")
	entered := make(chan struct{}, 1)
	store.hook(func(r *http.Request, name string) int {
		if name != snapObj(snap.ID, "rootfs.sz") {
			return 0
		}
		select {
		case entered <- struct{}{}:
		default:
		}
		<-r.Context().Done()
		return 499
	})
	stop := startUploadDispatcher(t, s)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher never began upload")
	}
	stop()
	row, err := reg.GetSnapshot(context.Background(), snap.ID)
	if err != nil || row.Upload == nil || row.Upload.State != "uploading" {
		t.Fatalf("interrupted intent = %+v, %v", row, err)
	}
	if err := reg.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := registry.Open(filepath.Join(filepath.Dir(s.cfg.Provisioner.SnapshotDir), "registry.db"), reg.Pools())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reopened.Close() })
	restarted := New(s.cfg, reopened)
	t.Cleanup(restarted.pf.CloseAll)
	restarted.blob = store.client
	store.hook(nil)
	finish := startUploadDispatcher(t, restarted)
	awaitUploadCondition(t, func() bool {
		row, err := reopened.GetSnapshot(context.Background(), snap.ID)
		return err == nil && row.Durability == "durable" && row.Upload == nil
	})
	finish()
	assertSnapshotPulled(t, store, snap)
}

func TestSnapshotUploadReconcilesExistingCommitWithoutPayloadWrites(t *testing.T) {
	store := newUploadTestStore(t)
	s, _ := testLifecycleServer(t)
	s.blob = store.client
	snap := seedUploadSnapshot(t, s, "committed")
	if err := s.uploadSnapshot(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{snap.MemPath, snap.StatePath, snap.RootfsPath} {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	runUploadAttempt(t, s, snap.ID)
	row, err := s.reg.GetSnapshot(context.Background(), snap.ID)
	if err != nil || row.Durability != "durable" || row.Upload != nil {
		t.Fatalf("commit reconciliation = %+v, %v", row, err)
	}
	for _, name := range []string{"rootfs.sz", "mem.sz", "state.sz", "meta.json"} {
		if n := store.putCount(snapObj(snap.ID, name)); n != 1 {
			t.Fatalf("%s written %d times", name, n)
		}
	}
}

func TestSnapshotUploadTombstoneWinsPublicationRaces(t *testing.T) {
	for _, first := range []string{"delete", "upload"} {
		t.Run(first, func(t *testing.T) {
			store := newUploadTestStore(t)
			s, _ := testLifecycleServer(t)
			s.blob = store.client
			snap := seedUploadSnapshot(t, s, "race")
			if first == "delete" {
				entered, release := make(chan struct{}), make(chan struct{})
				var once sync.Once
				store.hook(func(r *http.Request, name string) int {
					if name != snapObj(snap.ID, "meta.json") || r.URL.Query().Get("ifGenerationMatch") == "" {
						return 0
					}
					once.Do(func() { close(entered) })
					select {
					case <-release:
						return 0
					case <-r.Context().Done():
						return 499
					}
				})
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				result := make(chan error, 1)
				go func() { result <- s.uploadSnapshot(ctx, snap) }()
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("upload never reached publication")
				}
				if err := s.tombstoneSnapshot(context.Background(), snap.ID); err != nil {
					t.Fatal(err)
				}
				close(release)
				if err := <-result; !errors.Is(err, errSnapshotDeleted) {
					t.Fatalf("late publication = %v", err)
				}
			} else {
				if err := s.uploadSnapshot(context.Background(), snap); err != nil {
					t.Fatal(err)
				}
				if err := s.tombstoneSnapshot(context.Background(), snap.ID); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.uploadSnapshot(context.Background(), snap); !errors.Is(err, errSnapshotDeleted) {
				t.Fatalf("republishing deleted snapshot = %v", err)
			}
			fresh, _ := testLifecycleServer(t)
			fresh.blob = store.client
			if _, err := fresh.ensureSnapshotLocal(context.Background(), snap.ID); !errors.Is(err, errSnapshotDeleted) {
				t.Fatalf("tombstone restored: %v", err)
			}
			data, err := store.client.GetBytes(context.Background(), snapObj(snap.ID, "meta.json"))
			if err != nil {
				t.Fatal(err)
			}
			var marker snapshotCommit
			if err := json.Unmarshal(data, &marker); err != nil || marker.DeletedAt == nil {
				t.Fatalf("deleted marker lost: %s, %v", data, err)
			}
		})
	}
}

func TestSnapshotUploadDeleteRemovesPendingAndActiveIntent(t *testing.T) {
	for _, active := range []bool{false, true} {
		t.Run(fmt.Sprintf("active=%t", active), func(t *testing.T) {
			store := newUploadTestStore(t)
			s, _ := testLifecycleServer(t)
			s.blob = store.client
			snap := seedUploadSnapshot(t, s, "delete")
			if active {
				entered := make(chan struct{}, 1)
				store.hook(func(r *http.Request, name string) int {
					if name != snapObj(snap.ID, "rootfs.sz") {
						return 0
					}
					select {
					case entered <- struct{}{}:
					default:
					}
					<-r.Context().Done()
					return 499
				})
				if err := s.startSnapshotUpload(context.Background(), snap.ID); err != nil {
					t.Fatal(err)
				}
				select {
				case <-entered:
				case <-time.After(5 * time.Second):
					t.Fatal("active upload did not begin")
				}
			}
			if err := s.deleteSnapshot(context.Background(), snap.ID); err != nil {
				t.Fatal(err)
			}
			if _, err := s.reg.GetSnapshot(context.Background(), snap.ID); !errors.Is(err, sql.ErrNoRows) {
				t.Fatalf("deleted row remains: %v", err)
			}
			for _, path := range []string{snap.MemPath, snap.RootfsPath, snap.StatePath} {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("artifact remains: %s, %v", path, err)
				}
			}
			ids, err := s.reg.DueSnapshotUploads(context.Background(), time.Now().Add(time.Hour), 10)
			if err != nil || len(ids) != 0 {
				t.Fatalf("deleted intent remains: %v, %v", ids, err)
			}
			if err := s.startSnapshotUpload(context.Background(), snap.ID); err != nil {
				t.Fatal(err)
			}
			if committed, err := s.snapshotCommitted(context.Background(), snap.ID); committed || !errors.Is(err, errSnapshotDeleted) {
				t.Fatalf("deleted snapshot republished: %v, %v", committed, err)
			}
		})
	}
}

func TestSnapshotUploadMissingArtifactFailsPermanently(t *testing.T) {
	store := newUploadTestStore(t)
	s, _ := testLifecycleServer(t)
	s.blob = store.client
	snap := seedUploadSnapshot(t, s, "missing")
	if err := os.Remove(snap.MemPath); err != nil {
		t.Fatal(err)
	}
	runUploadAttempt(t, s, snap.ID)
	row, err := s.reg.GetSnapshot(context.Background(), snap.ID)
	if err != nil || row.Upload == nil || row.Upload.State != "failed" || row.Upload.Error == "" || row.Durability != "local" {
		t.Fatalf("missing artifact status = %+v, %v", row, err)
	}
	if strings.Contains(row.Upload.Error, snap.MemPath) {
		t.Fatalf("public failure exposed private artifact path: %s", row.Upload.Error)
	}
	ids, err := s.reg.DueSnapshotUploads(context.Background(), time.Now().Add(time.Hour), 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("terminal failure rescheduled: %v, %v", ids, err)
	}
	if committed, err := s.snapshotCommitted(context.Background(), snap.ID); committed || err != nil {
		t.Fatalf("missing artifact committed: %v, %v", committed, err)
	}
}

func TestSnapshotUploadBaseGateCancellation(t *testing.T) {
	s, _ := testLifecycleServer(t)
	// A different uploader retains admission throughout the cancellation.
	s.baseUploadGate <- struct{}{}
	defer func() { <-s.baseUploadGate }()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	entered := make(chan struct{})
	joined := make(chan error, 1)
	go func() {
		close(entered)
		joined <- s.ensureBaseUploaded(ctx, registry.Snapshot{ID: "blocked-base"})
	}()
	<-entered
	cancel()
	select {
	case err := <-joined:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("base admission cancellation = %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("cancelled base upload waited for another uploader to release admission")
	}
	if len(s.baseUploadGate) != 1 {
		t.Fatal("cancelled waiter released another uploader's admission")
	}
}

func TestSnapshotUploadDeleteStorageFailureReturnsServerError(t *testing.T) {
	store := newUploadTestStore(t)
	s, _ := testLifecycleServer(t)
	s.blob = store.client
	snap := seedUploadSnapshot(t, s, "delete-unavailable")
	store.hook(func(_ *http.Request, name string) int {
		if name == snapObj(snap.ID, "meta.json") {
			return http.StatusServiceUnavailable
		}
		return 0
	})
	request := httptest.NewRequest(http.MethodDelete, "/snapshots/"+snap.ID, nil)
	request.SetPathValue("id", snap.ID)
	response := httptest.NewRecorder()
	s.handleDeleteSnapshot(response, request)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("unavailable storage delete = %d: %s", response.Code, response.Body.String())
	}
	retained, err := s.reg.GetSnapshot(context.Background(), snap.ID)
	if err != nil || retained.Durability != "local" || retained.Upload == nil || retained.Upload.State != "pending" {
		t.Fatalf("failed delete lost its recoverable snapshot: %+v, %v", retained, err)
	}
	for _, path := range []string{snap.MemPath, snap.StatePath, snap.RootfsPath} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("failed delete removed local artifact %s: %v", path, err)
		}
	}
	if attempts := store.putCount(snapObj(snap.ID, "meta.json")); attempts != 3 {
		t.Fatalf("delete transport retries = %d, want 3", attempts)
	}
	store.hook(nil)
	response = httptest.NewRecorder()
	s.handleDeleteSnapshot(response, request)
	if response.Code != http.StatusNoContent {
		t.Fatalf("retry after storage recovery = %d: %s", response.Code, response.Body.String())
	}
	response = httptest.NewRecorder()
	s.handleDeleteSnapshot(response, request)
	if response.Code != http.StatusNotFound {
		t.Fatalf("missing snapshot delete = %d, want 404", response.Code)
	}
}
