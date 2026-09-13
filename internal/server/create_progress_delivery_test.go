package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/management"
	"github.com/ayush6624/sandbox/internal/registry"
)

func deliveryStore(t *testing.T) (*createops.SQLiteStore, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "operations.db")
	store, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store, path
}

func deliveryMember(t *testing.T, s *Server, store createops.Store, id string, target registry.CreateProgressTarget) (createops.Member, registry.CreateIntent) {
	t.Helper()
	ctx := context.Background()
	member := createops.Member{ID: id}
	opID := "operation-" + id
	if _, err := store.Accept(ctx, createops.AcceptRequest{ID: opID, Scope: opID, BodyHash: []byte(id), Type: "sandbox_batch_create", MaxParallelism: 1, Members: []createops.Member{member}, ResponseStatus: 202, ResponseBody: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}
	worker := createops.Worker{HostID: s.hostID(), RegistryID: s.reg.RegistryID(), CreateProgress: true}
	if err := store.Assign(ctx, opID, []string{id}, worker); err != nil {
		t.Fatal(err)
	}
	intent := memberIntent(t, member)
	intent.ProgressOwner = registry.CreateProgressOwner{CoordinatorID: store.CoordinatorID(), OperationID: opID}
	intent.ProgressTarget = target
	return member, intent
}

func waitDeliveredStage(t *testing.T, store createops.Store, operationID string, stage registry.CreateStage) createops.Operation {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	for {
		op, err := store.Get(ctx, operationID)
		if err != nil {
			t.Fatal(err)
		}
		if len(op.Members) == 1 && op.Members[0].Progress != nil && op.Members[0].Progress.Current.Stage == stage {
			return op
		}
		select {
		case <-ctx.Done():
			t.Fatalf("operation %s never received stage %s", operationID, stage)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func TestProgressDeliveryWhileAdmissionBlockedAndWarmCompletion(t *testing.T) {
	s := createRequestServer(t)
	s.cfg.GatewayURL = "http://127.0.0.1:1"
	store, _ := deliveryStore(t)
	member, intent := deliveryMember(t, s, store, "blocked", registry.CreateProgressLocal)
	deliveryCtx, stopDelivery := context.WithCancel(context.Background())
	deliveryDone := make(chan struct{})
	go func() { s.runCreateProgressDelivery(deliveryCtx, store); close(deliveryDone) }()
	defer func() { stopDelivery(); <-deliveryDone }()
	s.createSem = make(chan struct{}, 1)
	s.createSem <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	worker := createops.Worker{HostID: s.hostID(), RegistryID: s.reg.RegistryID(), CreateProgress: true}
	command := createops.Command{RegistryID: worker.RegistryID, Members: []createops.Member{member}, Owner: &intent.ProgressOwner}
	done := make(chan struct{})
	var executionErr error
	go func() { _, executionErr = s.Execute(ctx, worker, command); close(done) }()
	defer func() { cancel(); <-done }()
	waitCreateStage(t, s, member.ID, registry.CreateStageAdmission)
	op := waitDeliveredStage(t, store, intent.ProgressOwner.OperationID, registry.CreateStageAdmission)
	if op.CompletedAt != nil {
		t.Fatalf("blocked admission completed: %+v", op)
	}
	cancel()
	<-done
	if !errors.Is(executionErr, context.Canceled) {
		t.Fatalf("execution: %v", executionErr)
	}
	<-s.createSem
	if _, err := s.reg.CreateWarmForTemplate(context.Background(), "warm-delivery", "/tmp/unused-delivery-disk", "golden", "golden", 1, 128); err != nil {
		t.Fatal(err)
	}
	if err := s.reg.MarkWarmReady(context.Background(), "warm-delivery"); err != nil {
		t.Fatal(err)
	}
	s.golden.Store(&registry.Snapshot{ID: "golden", Golden: true})
	outcomes, err := s.Execute(context.Background(), worker, command)
	if err != nil {
		t.Fatal(err)
	}
	op = waitDeliveredStage(t, store, intent.ProgressOwner.OperationID, registry.CreateStageReady)
	if op.Members[0].Progress.Condition != "succeeded" || op.Members[0].Progress.Attempt != 2 || op.Members[0].Outcome != nil || op.CompletedAt != nil {
		t.Fatalf("progress bypassed fresh result confirmation: %+v", op)
	}
	if _, err := store.Record(context.Background(), op.ID, outcomes); err != nil {
		t.Fatal(err)
	}
	op, err = store.Get(context.Background(), op.ID)
	if err != nil || op.Status != "succeeded" || op.Members[0].Outcome == nil {
		t.Fatalf("fresh completion: %+v %v", op, err)
	}
}

func TestProgressDeliveryLostAcknowledgementReopenAndNewerObservation(t *testing.T) {
	s := createRequestServer(t)
	store, path := deliveryStore(t)
	_, intent := deliveryMember(t, s, store, "remote", registry.CreateProgressGateway)
	first, err := s.reg.BeginCreateProgress(context.Background(), intent)
	if err != nil {
		t.Fatal(err)
	}
	intent.ProgressAttempt = first.Attempt
	s.gatewayCredentials, err = management.NewCredentials([]string{"delivery-test-control"}, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := func(target createops.Store, loseAck bool) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/internal/v1/create-progress" || r.Header.Get("Authorization") != "Bearer delivery-test-control" {
				t.Error("wrong progress destination or credentials")
				w.WriteHeader(401)
				return
			}
			var update createops.ProgressUpdate
			if err := json.NewDecoder(r.Body).Decode(&update); err != nil {
				t.Error(err)
				w.WriteHeader(400)
				return
			}
			sequence, err := target.IngestProgress(r.Context(), update.Worker, update.Progress)
			if err != nil {
				t.Error(err)
				w.WriteHeader(409)
				return
			}
			if loseAck {
				conn, _, err := w.(http.Hijacker).Hijack()
				if err != nil {
					t.Error(err)
					return
				}
				_ = conn.Close()
				return
			}
			_ = json.NewEncoder(w).Encode(createops.ProgressAcknowledgement{Sequence: sequence})
		})
	}
	firstServer := httptest.NewServer(handler(store, true))
	s.cfg.GatewayURL = firstServer.URL
	_, err = s.deliverCreateProgressPage(context.Background(), store, firstServer.Client(), "")
	firstServer.Close()
	if err == nil {
		t.Fatal("lost acknowledgement accepted")
	}
	if pending, err := s.reg.PendingOwnedCreateProgress(context.Background(), "", 10); err != nil || len(pending) != 1 {
		t.Fatalf("lost ack erased pending update: %+v %v", pending, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if err := s.reg.AdvanceCreateProgress(context.Background(), intent, registry.CreateStageSource); err != nil {
		t.Fatal(err)
	}
	secondServer := httptest.NewServer(handler(reopened, false))
	defer secondServer.Close()
	s.cfg.GatewayURL = secondServer.URL
	if err := s.deliverCreateProgress(context.Background(), reopened, secondServer.Client(), first); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.reg.PendingOwnedCreateProgress(context.Background(), "", 10); err != nil || len(pending) != 1 || pending[0].Sequence <= first.Sequence {
		t.Fatalf("old acknowledgement cleared new observation: %+v %v", pending, err)
	}
	if _, err := s.deliverCreateProgressPage(context.Background(), reopened, secondServer.Client(), ""); err != nil {
		t.Fatal(err)
	}
	if pending, err := s.reg.PendingOwnedCreateProgress(context.Background(), "", 10); err != nil || len(pending) != 0 {
		t.Fatalf("acknowledged update still pending: %+v %v", pending, err)
	}
	op, err := reopened.Get(context.Background(), intent.ProgressOwner.OperationID)
	if err != nil || op.Members[0].Progress == nil || op.Members[0].Progress.Current.Stage != registry.CreateStageSource {
		t.Fatalf("reopened projection: %+v %v", op, err)
	}
}

func TestProgressDeliveryCursorPassesRejectedOwner(t *testing.T) {
	s := createRequestServer(t)
	store, _ := deliveryStore(t)
	for i := 0; i <= createProgressPageSize; i++ {
		_, intent := deliveryMember(t, s, store, fmt.Sprintf("%02d", i), registry.CreateProgressLocal)
		if i == 0 {
			intent.ProgressOwner.CoordinatorID = "unknown-coordinator"
		}
		if _, err := s.reg.BeginCreateProgress(context.Background(), intent); err != nil {
			t.Fatal(err)
		}
	}
	cursor, err := s.deliverCreateProgressPage(context.Background(), store, &http.Client{}, "")
	if err == nil || cursor == "" {
		t.Fatalf("missing rejected-owner result: %q %v", cursor, err)
	}
	if _, err := s.deliverCreateProgressPage(context.Background(), store, &http.Client{}, cursor); err != nil {
		t.Fatal(err)
	}
	pending, err := s.reg.PendingOwnedCreateProgress(context.Background(), "", 100)
	if err != nil || len(pending) != 1 || pending[0].ID != "00" {
		t.Fatalf("rejected row starved later observations: %+v %v", pending, err)
	}
}

func TestProgressDeliveryRejectsInvalidAcknowledgementsAndRedirects(t *testing.T) {
	for _, reply := range []string{"wrong_sequence", "trailing_json", "unavailable", "redirect"} {
		t.Run(reply, func(t *testing.T) {
			s := createRequestServer(t)
			store, _ := deliveryStore(t)
			_, intent := deliveryMember(t, s, store, "pending", registry.CreateProgressGateway)
			p, err := s.reg.BeginCreateProgress(context.Background(), intent)
			if err != nil {
				t.Fatal(err)
			}
			s.gatewayCredentials, err = management.NewCredentials([]string{"delivery-test-control"}, "")
			if err != nil {
				t.Fatal(err)
			}
			var redirected atomic.Int32
			other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				redirected.Add(1)
				_ = json.NewEncoder(w).Encode(createops.ProgressAcknowledgement{Sequence: p.Sequence})
			}))
			defer other.Close()
			endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch reply {
				case "wrong_sequence":
					_ = json.NewEncoder(w).Encode(createops.ProgressAcknowledgement{Sequence: p.Sequence + 1})
				case "trailing_json":
					_, _ = fmt.Fprintf(w, `{"Sequence":%d}{}`, p.Sequence)
				case "unavailable":
					w.WriteHeader(http.StatusServiceUnavailable)
				case "redirect":
					http.Redirect(w, r, other.URL, http.StatusTemporaryRedirect)
				}
			}))
			defer endpoint.Close()
			s.cfg.GatewayURL = endpoint.URL
			if err := s.deliverCreateProgress(context.Background(), store, endpoint.Client(), p); err == nil {
				t.Fatal("invalid delivery response acknowledged")
			}
			if pending, err := s.reg.PendingOwnedCreateProgress(context.Background(), "", 10); err != nil || len(pending) != 1 {
				t.Fatalf("failed delivery erased pending state: %+v %v", pending, err)
			}
			if redirected.Load() != 0 {
				t.Fatal("progress sent to redirect destination")
			}
		})
	}
}
