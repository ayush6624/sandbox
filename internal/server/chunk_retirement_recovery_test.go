package server

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

func restartChunkRetirementServer(t *testing.T, s *Server, store *uploadTestStore) *Server {
	t.Helper()
	path, pools := s.reg.Path(), s.reg.Pools()
	if err := s.reg.Close(); err != nil {
		t.Fatal(err)
	}
	reg, err := registry.Open(path, pools)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = reg.Close() })
	restarted := New(s.cfg, reg)
	restarted.blob = store.client
	t.Cleanup(restarted.pf.CloseAll)
	return restarted
}

func TestChunkRootRetirementSurvivesLostResponseAndRestart(t *testing.T) {
	for _, mode := range []string{"ordinary", "owned", "destroy"} {
		t.Run(mode, func(t *testing.T) {
			s, store, old, replace := ownedReplacementFixture(t, mode == "owned")
			if mode == "destroy" {
				replace = func(ctx context.Context) error { return s.destroyHandoff(ctx, old.Record.ID) }
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var intercepted atomic.Bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Query().Get("name") == handoffControlObj(old.Record.ID) && intercepted.CompareAndSwap(false, true) {
					rows, err := s.reg.ListChunkRootReleases(context.Background())
					if err != nil || len(rows) != 1 || rows[0].SetID != old.Ref.Generation {
						t.Errorf("authority changed without journal: %+v, %v", rows, err)
					}
					response := httptest.NewRecorder()
					store.serveHTTP(response, r)
					if response.Code != http.StatusOK {
						t.Errorf("control commit: %d", response.Code)
					}
					cancel()
					return
				}
				store.serveHTTP(w, r)
			}))
			t.Cleanup(srv.Close)
			endpoint, err := url.Parse(srv.URL)
			if err != nil {
				t.Fatal(err)
			}
			s.blob = gcsblob.NewWithHTTPClient("upload-test", &http.Client{Transport: uploadTestTransport{target: endpoint, base: http.DefaultTransport}})
			if err := replace(ctx); err == nil {
				t.Fatal("lost response unexpectedly confirmed")
			}
			if !intercepted.Load() {
				t.Fatal("control request not intercepted")
			}
			assertReplacementRoot(t, s, old, false)
			restarted := restartChunkRetirementServer(t, s, store)
			restarted.retryChunkReleases(context.Background())
			assertReplacementRoot(t, restarted, old, true)
			rows, err := restarted.reg.ListChunkRootReleases(context.Background())
			if err != nil || len(rows) != 0 {
				t.Fatalf("retired root journal remains: %+v, %v", rows, err)
			}
		})
	}
}

func TestChunkRootRetirementRestartKeepsUnchangedAuthority(t *testing.T) {
	s, store, old, replace := ownedReplacementFixture(t, false)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store.hook(func(_ *http.Request, name string) int {
		if name == handoffControlObj(old.Record.ID) {
			cancel()
			return http.StatusForbidden
		}
		return 0
	})
	if err := replace(ctx); err == nil {
		t.Fatal("rejected replacement succeeded")
	}
	store.hook(nil)
	restarted := restartChunkRetirementServer(t, s, store)
	restarted.retryChunkReleases(context.Background())
	assertReplacementRoot(t, restarted, old, false)
	rows, err := restarted.reg.ListChunkRootReleases(context.Background())
	if err != nil || len(rows) != 1 {
		t.Fatalf("pending root check lost: %+v, %v", rows, err)
	}
	if err := restarted.destroyHandoff(context.Background(), old.Record.ID); err != nil {
		t.Fatal(err)
	}
	assertReplacementRoot(t, restarted, old, true)
}

func TestChunkRootJournalFailurePreventsAuthorityChange(t *testing.T) {
	for _, mode := range []string{"ordinary", "owned", "destroy"} {
		t.Run(mode, func(t *testing.T) {
			s, _, old, replace := ownedReplacementFixture(t, mode == "owned")
			if mode == "destroy" {
				replace = func(ctx context.Context) error { return s.destroyHandoff(ctx, old.Record.ID) }
			}
			ctx := context.Background()
			before, revision, err := s.readHandoff(ctx, old.Record.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := s.reg.Close(); err != nil {
				t.Fatal(err)
			}
			if err := replace(ctx); err == nil {
				t.Fatal("authority changed without durable root journal")
			}
			after, nextRevision, err := s.readHandoff(ctx, old.Record.ID)
			if err != nil || revision != nextRevision || !sameHandoff(before, after) {
				t.Fatalf("control changed after journal failure: %d -> %d, %v", revision, nextRevision, err)
			}
			assertReplacementRoot(t, s, old, false)
		})
	}
}

func TestChunkRootRetirementRetryAfterLocalCompletionFailure(t *testing.T) {
	s, store, old, _ := ownedReplacementFixture(t, false)
	var closed atomic.Bool
	store.hook(func(_ *http.Request, name string) int {
		if name == "chunksets/catalog/"+old.Ref.Generation+".json" && closed.CompareAndSwap(false, true) {
			if err := s.reg.Close(); err != nil {
				t.Errorf("interrupt local completion: %v", err)
			}
		}
		return 0
	})
	if err := s.destroyHandoff(context.Background(), old.Record.ID); err == nil {
		t.Fatal("closed journal unexpectedly recorded completion")
	}
	if !closed.Load() {
		t.Fatal("retirement did not reach catalog")
	}
	store.hook(nil)
	assertReplacementRoot(t, s, old, true)
	restarted := restartChunkRetirementServer(t, s, store)
	rows, err := restarted.reg.ListChunkRootReleases(context.Background())
	if err != nil || len(rows) != 1 || !rows[0].Fenced {
		t.Fatalf("incomplete retirement receipt lost: %+v, %v", rows, err)
	}
	restarted.retryChunkReleases(context.Background())
	rows, err = restarted.reg.ListChunkRootReleases(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("confirmed retirement did not finish: %+v, %v", rows, err)
	}
}

func TestChunkRootRetirementTimeoutDoesNotStarveLaterEntries(t *testing.T) {
	s, store, _, _ := ownedControlFixture(t)
	catalog, _ := s.chunkStorage()
	ctx := context.Background()
	for range 2 {
		setID := uuid.NewString()
		rootID := handoffControlObj(uuid.NewString())
		if err := catalog.Begin(ctx, setID, "unpublished-test"); err != nil {
			t.Fatal(err)
		}
		if err := catalog.AttachRoot(ctx, setID, "unpublished-test", rootID); err != nil {
			t.Fatal(err)
		}
		if err := s.reg.QueueChunkRootRelease(ctx, registry.ChunkRootRelease{SetID: setID, RootID: rootID, SandboxID: uuid.NewString(), Fenced: true}); err != nil {
			t.Fatal(err)
		}
	}
	rows, err := s.reg.ListChunkRootReleases(ctx)
	if err != nil || len(rows) != 2 {
		t.Fatalf("seed retirement queue: %+v, %v", rows, err)
	}
	attempt, cancel := context.WithCancel(ctx)
	defer cancel()
	store.mu.Lock()
	store.beforeGet = func(_ *http.Request, name string) {
		if name == "chunksets/catalog/"+rows[0].SetID+".json" {
			cancel()
		}
	}
	store.mu.Unlock()
	s.retryChunkReleases(attempt)
	store.mu.Lock()
	store.beforeGet = nil
	store.mu.Unlock()
	next, stop := context.WithCancel(ctx)
	defer stop()
	// If retry starts with the same first root again, this request consumes its
	// deadline again. The second root must already have completed by then.
	store.mu.Lock()
	store.beforeGet = func(_ *http.Request, name string) {
		if name == "chunksets/catalog/"+rows[0].SetID+".json" {
			stop()
		}
	}
	store.mu.Unlock()
	s.retryChunkReleases(next)
	store.mu.Lock()
	store.beforeGet = nil
	store.mu.Unlock()
	remaining, err := s.reg.ListChunkRootReleases(ctx)
	if err != nil || len(remaining) != 1 || remaining[0].SetID != rows[0].SetID {
		t.Fatalf("stalled first root starved queue: %+v, %v", remaining, err)
	}
}
