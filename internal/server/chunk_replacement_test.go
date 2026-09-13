package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync/atomic"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/google/uuid"
)

func ownedReplacementFixture(t *testing.T, owned bool) (*Server, *uploadTestStore, *retainedHibernation, func(context.Context) error) {
	t.Helper()
	ctx := context.Background()
	s, store, old, job := ownedControlFixture(t)
	if err := s.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	claim, err := s.claimHandoff(ctx, old.Record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.authorizeHandoffRun(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if !owned {
		rec := old.Record
		rec.Generation = ""
		return s, store, old, func(ctx context.Context) error { return s.publishLocalHibernation(ctx, &rec) }
	}
	data, _ := json.Marshal(old)
	var next retainedHibernation
	if err := json.Unmarshal(data, &next); err != nil {
		t.Fatal(err)
	}
	next.Ref.Generation = uuid.NewString()
	next.Record.Generation = next.Ref.Generation
	next.Manifest.Storage.SetID = next.Ref.Generation
	next.Ref.ManifestSHA256, err = hibPeerManifestDigest(&next.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	job, err = s.prepareHandoffOffer(ctx, old.Record.ID, &next)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.reg.Create(ctx, old.Record.ID, "owned", "unused", nil, "", -1, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.Hibernate(ctx, old.Record.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	return s, store, old, func(ctx context.Context) error { return s.publishHandoffOffer(ctx, job) }
}

func assertReplacementRoot(t *testing.T, s *Server, d *retainedHibernation, retired bool) {
	t.Helper()
	catalog, _ := s.chunkStorage()
	view, err := catalog.Inspect(context.Background(), d.Ref.Generation)
	if err != nil || len(view.Roots) != 1 || view.Roots[0].Retired != retired || view.Publisher.Released {
		t.Fatalf("previous root retired=%v: %+v, %v", retired, view, err)
	}
}

func TestOwnedReplacementLostResponseRetiresPreviousRoot(t *testing.T) {
	for _, owned := range []bool{false, true} {
		name := "ordinary"
		if owned {
			name = "owned"
		}
		for _, canceled := range []bool{false, true} {
			suffix := "/advanced-claim"
			if canceled {
				suffix = "/canceled-response"
			}
			t.Run(name+suffix, func(t *testing.T) {
				s, store, old, replace := ownedReplacementFixture(t, owned)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				target := handoffTestServer(t, store, "destination")
				var intercepted atomic.Bool
				srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.Method == http.MethodPost && r.URL.Query().Get("name") == handoffControlObj(old.Record.ID) && intercepted.CompareAndSwap(false, true) {
						response := httptest.NewRecorder()
						store.serveHTTP(response, r)
						if response.Code != http.StatusOK {
							t.Errorf("replacement commit: %d", response.Code)
						}
						if _, err := target.claimHandoff(context.Background(), old.Record.ID); err != nil {
							t.Errorf("advance replacement claim: %v", err)
						}
						if canceled {
							cancel()
						}
						http.Error(w, "lost successful response", http.StatusForbidden)
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
					t.Fatal("advanced control unexpectedly confirmed exact replacement")
				}
				if !intercepted.Load() {
					t.Fatal("replacement did not commit")
				}
				assertReplacementRoot(t, s, old, false)
				s.retryChunkReleases(context.Background())
				assertReplacementRoot(t, s, old, true)
			})
		}
	}
}

func TestOwnedFailedReplacementRetainsRootThroughSameGenerationChanges(t *testing.T) {
	for _, owned := range []bool{false, true} {
		name := "ordinary"
		if owned {
			name = "owned"
		}
		t.Run(name, func(t *testing.T) {
			s, store, old, replace := ownedReplacementFixture(t, owned)
			ctx := context.Background()
			store.hook(func(_ *http.Request, name string) int {
				if name == handoffControlObj(old.Record.ID) {
					return http.StatusForbidden
				}
				return 0
			})
			if err := replace(ctx); err == nil {
				t.Fatal("unapplied replacement reported success")
			}
			store.hook(nil)
			s.retryChunkReleases(ctx)
			assertReplacementRoot(t, s, old, false)
			current, revision, err := s.readHandoff(ctx, old.Record.ID)
			if err != nil {
				t.Fatal(err)
			}
			current.Phase, current.ClaimID = handoffOffered, ""
			if _, err := s.casHandoff(ctx, *current, revision); err != nil {
				t.Fatal(err)
			}
			s.retryChunkReleases(ctx)
			assertReplacementRoot(t, s, old, false)
			claim, err := s.claimHandoff(ctx, old.Record.ID)
			if err != nil {
				t.Fatal(err)
			}
			s.retryChunkReleases(ctx)
			assertReplacementRoot(t, s, old, false)
			if err := s.authorizeHandoffRun(ctx, claim); err != nil {
				t.Fatal(err)
			}
			s.retryChunkReleases(ctx)
			assertReplacementRoot(t, s, old, false)
		})
	}
}
