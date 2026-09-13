package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

func handoffTestServer(t *testing.T, store *uploadTestStore, host string) *Server {
	t.Helper()
	s, _ := testLifecycleServer(t)
	s.cfg.HostID = host
	s.blob = store.client
	return s
}

func seedHandoffOffer(t *testing.T, s *Server, id string) handoffControl {
	t.Helper()
	c := handoffControl{Version: handoffControlVersion, Generation: uuid.NewString(), Phase: handoffOffered, SourceHostID: "source", Record: hibRecord{Version: hibRecordVersion, ID: id, MemForm: memFormChunked, RootfsForm: rootfsFormFull}}
	if _, err := s.casHandoff(context.Background(), c, 0); err != nil {
		t.Fatal(err)
	}
	return c
}

func TestHandoffConcurrentAndStaggeredClaimsHaveOneWinner(t *testing.T) {
	store := newUploadTestStore(t)
	var servers []*Server
	for i := 0; i < 4; i++ {
		servers = append(servers, handoffTestServer(t, store, fmt.Sprintf("worker-%d", i)))
	}
	id := uuid.NewString()
	seedHandoffOffer(t, servers[0], id)
	start := make(chan struct{})
	claims := make(chan *handoffClaim, len(servers))
	var wg sync.WaitGroup
	for _, s := range servers {
		wg.Add(1)
		go func(s *Server) {
			defer wg.Done()
			<-start
			claim, err := s.claimHandoff(context.Background(), id)
			if err == nil {
				claims <- claim
			} else if !errors.Is(err, ErrOwnerContended) {
				t.Errorf("claim: %v", err)
			}
		}(s)
	}
	close(start)
	wg.Wait()
	close(claims)
	if len(claims) != 1 {
		t.Fatalf("claim winners=%d", len(claims))
	}
	winner := <-claims
	for _, s := range servers {
		if s.hostID() == winner.Control.HostID {
			retry, err := s.claimHandoff(context.Background(), id)
			if err != nil || retry.Control.ClaimID != winner.Control.ClaimID || retry.Revision != winner.Revision {
				t.Fatalf("own claimed retry changed rights: %+v %v", retry, err)
			}
			continue
		}
		if _, err := s.claimHandoff(context.Background(), id); !errors.Is(err, ErrOwnerContended) {
			t.Fatalf("staggered claim stole %s: %v", winner.Control.HostID, err)
		}
	}
	current, _, err := servers[0].readHandoff(context.Background(), id)
	if err != nil || current.ClaimID != winner.Control.ClaimID {
		t.Fatalf("claim changed: %+v %v", current, err)
	}
}

func TestHandoffCanceledClaimResponseRecoversOnlyUnstartedOwnClaim(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store := newUploadTestStore(t)
	owner := handoffTestServer(t, store, "owner")
	id := uuid.NewString()
	seedHandoffOffer(t, owner, id)
	var lost atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Query().Get("name") == handoffControlObj(id) && lost.CompareAndSwap(false, true) {
			response := httptest.NewRecorder()
			store.serveHTTP(response, r)
			if response.Code != http.StatusOK {
				t.Errorf("claim write failed before response loss: %d", response.Code)
			}
			cancel()
			return
		}
		store.serveHTTP(w, r)
	}))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	owner.blob = gcsblob.NewWithHTTPClient("upload-test", &http.Client{Transport: uploadTestTransport{target: target, base: http.DefaultTransport}})
	if _, err := owner.claimHandoff(ctx, id); !errors.Is(err, context.Canceled) {
		t.Fatalf("lost response did not cancel claim request: %v", err)
	}
	current, revision, err := owner.readHandoff(context.Background(), id)
	if err != nil || current == nil || current.Phase != handoffClaimed {
		t.Fatalf("claim was not committed: %+v %v", current, err)
	}
	foreignRegistry := handoffTestServer(t, store, "owner")
	if _, err := foreignRegistry.claimHandoff(context.Background(), id); !errors.Is(err, ErrOwnerContended) {
		t.Fatalf("same host with foreign registry recovered claim: %v", err)
	}
	for _, claim := range []func(context.Context, string) (*handoffClaim, error){owner.claimHandoff, owner.claimLocalHandoff} {
		recovered, err := claim(context.Background(), id)
		if err != nil || recovered == nil || recovered.Revision != revision || recovered.Control.ClaimID != current.ClaimID {
			t.Fatalf("retry changed committed claim: %+v %v", recovered, err)
		}
	}
	recovered, err := owner.claimHandoff(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.authorizeHandoffRun(context.Background(), recovered); err != nil {
		t.Fatal(err)
	}
	if _, err := owner.claimHandoff(context.Background(), id); !errors.Is(err, ErrOwnerContended) {
		t.Fatalf("running claim recovered without execution proof: %v", err)
	}
}

func TestHandoffAuthorizationAndStoppedAttemptReopen(t *testing.T) {
	ctx := context.Background()
	store := newUploadTestStore(t)
	first, second := handoffTestServer(t, store, "first"), handoffTestServer(t, store, "second")
	id := uuid.NewString()
	original := seedHandoffOffer(t, first, id)
	claim, err := first.claimHandoff(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	stale := *claim
	if err := first.authorizeHandoffRun(ctx, claim); err != nil {
		t.Fatal(err)
	}
	running := *claim
	if err := first.reopenHandoff(ctx, &stale); !errors.Is(err, ErrOwnerContended) {
		t.Fatalf("stale revision reopened running claim: %v", err)
	}
	if _, err := second.claimHandoff(ctx, id); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("running rights stolen", err)
	}
	// No VM is launched in this fixture; the integration caller must prove stop.
	if err := first.reopenHandoff(ctx, claim); err != nil {
		t.Fatal(err)
	}
	next, err := second.claimHandoff(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if next.Control.Generation != original.Generation {
		t.Fatal("retry changed checkpoint generation")
	}
	if err := first.reopenHandoff(ctx, &running); !errors.Is(err, ErrOwnerContended) {
		t.Fatalf("old owner reopened later claimant: %v", err)
	}
	if err := second.authorizeHandoffRun(ctx, next); err != nil {
		t.Fatal(err)
	}
	if err := second.destroyHandoff(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := second.destroyHandoff(ctx, id); err != nil {
		t.Fatal("destroy not idempotent", err)
	}
	if _, err := first.claimHandoff(ctx, id); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("destroyed checkpoint resurrected", err)
	}
}

func TestHandoffLocalReservationFencesLegacyBootstrap(t *testing.T) {
	ctx := context.Background()
	store := newUploadTestStore(t)
	source, target := handoffTestServer(t, store, "source"), handoffTestServer(t, store, "target")
	id := uuid.NewString()
	rec := hibRecord{Version: hibRecordVersion, ID: id}
	data, _ := json.Marshal(rec)
	if err := store.client.PutBytes(ctx, hibRecordObj(id), data); err != nil {
		t.Fatal(err)
	}
	if err := source.reserveLocalHandoff(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err := target.claimHandoff(ctx, id); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("legacy record bypassed local reservation", err)
	}
	if err := target.reserveLocalHandoff(ctx, id); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("foreign reservation stole rights", err)
	}
	if claim, err := source.claimLocalHandoff(ctx, id); err != nil || claim != nil {
		t.Fatalf("existing local rights=%+v %v", claim, err)
	}
	otherID := uuid.NewString()
	rec.ID = otherID
	data, _ = json.Marshal(rec)
	if err := store.client.PutBytes(ctx, hibRecordObj(otherID), data); err != nil {
		t.Fatal(err)
	}
	claim, err := target.claimHandoff(ctx, otherID)
	if err != nil {
		t.Fatal(err)
	}
	if claim.Control.Record.ID != otherID {
		t.Fatal("legacy bootstrap lost record")
	}
	if err := source.reserveLocalHandoff(ctx, otherID); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("late initial source bypassed bootstrapped claim", err)
	}
}

func seedJournaledHandoff(t *testing.T, s *Server, id string) registry.HibernationHandoff {
	t.Helper()
	ctx := context.Background()
	if _, err := s.reg.Create(ctx, id, "", "/tmp/"+id, nil, "", 0, 1, 1); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.Hibernate(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := s.reserveLocalHandoff(ctx, id); err != nil {
		t.Fatal(err)
	}
	desc, _ := writeRetainedTestBundle(t, s, id)
	desc.Record.Version = hibRecordVersion
	job, err := s.prepareHandoffOffer(ctx, id, desc)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.reg.CommitHibernationHandoff(ctx, job); err != nil {
		t.Fatal(err)
	}
	return job
}

func TestHandoffDelayedPublicationCannotResetClaimOrReturnMove(t *testing.T) {
	ctx := context.Background()
	store := newUploadTestStore(t)
	source, target := handoffTestServer(t, store, "source"), handoffTestServer(t, store, "target")
	id := uuid.NewString()
	job := seedJournaledHandoff(t, source, id)
	if err := source.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	claim, err := target.claimHandoff(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.authorizeHandoffRun(ctx, claim); err != nil {
		t.Fatal(err)
	}
	if err := source.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal("same generation replay failed", err)
	}
	current, _, err := source.readHandoff(ctx, id)
	if err != nil || current.Phase != handoffRunning || current.ClaimID != claim.Control.ClaimID {
		t.Fatalf("replay reset running claim: %+v %v", current, err)
	}
	newRecord := current.Record
	newRecord.Generation = ""
	if err := source.publishLocalHibernation(ctx, &newRecord); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("old uploader superseded new owner", err)
	}
	if err := target.publishLocalHibernation(ctx, &newRecord); err != nil {
		t.Fatal(err)
	}
	returned, _, err := source.readHandoff(ctx, id)
	if err != nil || returned.Generation == job.Generation || returned.SourceHostID != "target" || returned.Descriptor != nil {
		t.Fatalf("new ordinary generation=%+v %v", returned, err)
	}
	if err := source.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal("superseded publication did not settle", err)
	}
	final, _, err := source.readHandoff(ctx, id)
	if err != nil || !sameHandoff(returned, final) {
		t.Fatalf("return generation changed: %+v %v", final, err)
	}
}

func TestHandoffSupersededUnmarkedPublicationSettlesWithoutResettingReturn(t *testing.T) {
	ctx := context.Background()
	store := newUploadTestStore(t)
	source, target := handoffTestServer(t, store, "source"), handoffTestServer(t, store, "target")
	id := uuid.NewString()
	job := seedJournaledHandoff(t, source, id)
	offer, err := decodeHandoff(job.Offer, id)
	if err != nil {
		t.Fatal(err)
	}
	// Commit the GCS offer but lose the local Published update, as happens
	// when the request is canceled immediately after the CAS.
	if _, err := source.casHandoff(ctx, *offer, job.ExpectedRevision); err != nil {
		t.Fatal(err)
	}
	claim, err := target.claimHandoff(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.authorizeHandoffRun(ctx, claim); err != nil {
		t.Fatal(err)
	}
	rec := claim.Control.Record
	rec.Generation = ""
	if err := target.publishLocalHibernation(ctx, &rec); err != nil {
		t.Fatal(err)
	}
	before, revision, err := source.readHandoff(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err := source.publishHandoffOffer(ctx, job); err != nil {
		t.Fatal(err)
	}
	settled, err := source.reg.GetHibernationHandoff(ctx, job.Generation)
	if err != nil || !settled.Published {
		t.Fatalf("superseded journal remains pending: %+v %v", settled, err)
	}
	after, afterRevision, err := source.readHandoff(ctx, id)
	if err != nil || revision != afterRevision || !sameHandoff(before, after) {
		t.Fatalf("old offer changed current generation: %+v %v", after, err)
	}
}

func TestHandoffLocalWakeCannotClaimForeignGeneration(t *testing.T) {
	ctx := context.Background()
	store := newUploadTestStore(t)
	local, foreign := handoffTestServer(t, store, "local"), handoffTestServer(t, store, "foreign")
	id := uuid.NewString()
	offered := seedHandoffOffer(t, foreign, id)
	if _, err := local.claimLocalHandoff(ctx, id); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("stale local row claimed foreign offer", err)
	}
	_, revision, err := local.readHandoff(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	offered.HostID, offered.RegistryID = local.hostID(), local.reg.RegistryID()
	revision, err = local.casHandoff(ctx, offered, revision)
	if err != nil {
		t.Fatal(err)
	}
	// The exact control/revision captured for local wake cannot be replaced by a
	// second read that silently selects a newer checkpoint.
	captured := offered
	offered.Generation, offered.HostID, offered.RegistryID = uuid.NewString(), foreign.hostID(), foreign.reg.RegistryID()
	if _, err := foreign.casHandoff(ctx, offered, revision); err != nil {
		t.Fatal(err)
	}
	if _, err := local.claimOfferedHandoff(ctx, &captured, revision); !errors.Is(err, ErrOwnerContended) {
		t.Fatal("stale local revision claimed new checkpoint", err)
	}
	current, _, err := local.readHandoff(ctx, id)
	if err != nil || current.Generation != offered.Generation || current.Phase != handoffOffered {
		t.Fatalf("local wake changed foreign offer: %+v %v", current, err)
	}
}

func TestHandoffRejectsMismatchedDescriptorBeforePublication(t *testing.T) {
	ctx := context.Background()
	store := newUploadTestStore(t)
	source := handoffTestServer(t, store, "source")
	id := uuid.NewString()
	job := seedJournaledHandoff(t, source, id)
	var offer handoffControl
	if err := json.Unmarshal(job.Offer, &offer); err != nil {
		t.Fatal(err)
	}
	offer.Descriptor.Record.Name = "different checkpoint"
	data, _ := json.Marshal(offer)
	job.Offer = data
	if err := source.publishHandoffOffer(ctx, job); err == nil {
		t.Fatal("published mismatched descriptor")
	}
	current, _, err := source.readHandoff(ctx, id)
	if err != nil || current.Phase != handoffLocal {
		t.Fatalf("invalid offer changed authority: %+v %v", current, err)
	}
}
