package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/cluster"
	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestCreateProgressIngestion(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "operations.db")
	store, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	member := createops.Member{ID: "member", Spec: createops.Spec{Name: "test"}}
	_, err = store.Accept(ctx, createops.AcceptRequest{ID: "operation", Scope: "key", BodyHash: []byte("hash"), Type: "sandbox_batch_create", MaxParallelism: 1, Members: []createops.Member{member}, ResponseStatus: 202, ResponseBody: []byte(`{}`)})
	if err != nil {
		t.Fatal(err)
	}
	worker := createops.Worker{HostID: "worker", RegistryID: "registry", CreateProgress: true}
	if err = store.Assign(ctx, "operation", []string{member.ID}, worker); err != nil {
		t.Fatal(err)
	}
	g := secureTestGateway(t)
	registerCreateWorker(t, g, cluster.Heartbeat{HostID: worker.HostID, RegistryID: worker.RegistryID, Addr: "http://127.0.0.1:18080", CreateProgress: true})
	registerCreateWorker(t, g, cluster.Heartbeat{HostID: "other", RegistryID: "other-registry", Addr: "http://127.0.0.1:18081"})
	spec, _ := json.Marshal(member.Spec)
	now := time.Now().UTC()
	progress := registry.CreateProgress{ID: member.ID, RegistryID: worker.RegistryID, RequestHash: sha256.Sum256(spec), Attempt: 1, Sequence: 1, Current: registry.CreateStageMark{Stage: registry.CreateStageVM, Attempt: 1, StartedAt: now}, Condition: "active", ObservedAt: now, Owner: registry.CreateProgressOwner{CoordinatorID: store.CoordinatorID(), OperationID: "operation"}, Target: registry.CreateProgressGateway}
	handler := g.bearerAuth(g.createProgressHandler(store))
	post := func(body []byte, token string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/internal/v1/create-progress", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, req)
		return response
	}
	body, _ := json.Marshal(createops.ProgressUpdate{Worker: worker, Progress: progress})
	if response := post(body, "client-token"); response.Code != http.StatusUnauthorized {
		t.Fatalf("client credential: %d", response.Code)
	}
	for _, invalid := range [][]byte{[]byte(`{`), []byte(`{"unknown":true}`), append(append([]byte{}, body...), []byte(` {}`)...), []byte(`{"unknown":"` + strings.Repeat("x", 2<<20) + `"}`)} {
		if response := post(invalid, "worker-token"); response.Code != http.StatusBadRequest {
			t.Fatalf("malformed: %d", response.Code)
		}
	}
	for _, tc := range []struct {
		name   string
		mutate func(*createops.ProgressUpdate)
		status int
	}{
		{"owner", func(u *createops.ProgressUpdate) { u.Progress.Owner.CoordinatorID = "unknown" }, http.StatusConflict},
		{"operation", func(u *createops.ProgressUpdate) { u.Progress.Owner.OperationID = "unknown" }, http.StatusNotFound},
		{"member", func(u *createops.ProgressUpdate) { u.Progress.ID = "unknown" }, http.StatusNotFound},
		{"unregistered", func(u *createops.ProgressUpdate) { u.Worker.HostID = "missing" }, http.StatusConflict},
		{"replacement registry", func(u *createops.ProgressUpdate) { u.Worker.RegistryID = "replacement" }, http.StatusConflict},
		{"assignment", func(u *createops.ProgressUpdate) {
			u.Worker = createops.Worker{HostID: "other", RegistryID: "other-registry"}
			u.Progress.RegistryID = "other-registry"
		}, http.StatusConflict},
		{"hash", func(u *createops.ProgressUpdate) { u.Progress.RequestHash = [32]byte{} }, http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := createops.ProgressUpdate{Worker: worker, Progress: progress}
			tc.mutate(&u)
			data, _ := json.Marshal(u)
			r := post(data, "worker-token")
			if r.Code != tc.status {
				t.Fatalf("HTTP %d: %s", r.Code, r.Body.String())
			}
		})
	}
	response := post(body, "worker-token")
	var ack createops.ProgressAcknowledgement
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &ack) != nil || ack.Sequence != 1 {
		t.Fatalf("ack: %d %s", response.Code, response.Body.String())
	}
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	op, err := store.Get(ctx, "operation")
	if err != nil || op.Members[0].Progress == nil || op.Members[0].Progress.Sequence != ack.Sequence {
		t.Fatalf("ack was not durable: %+v %v", op, err)
	}
	handler = g.bearerAuth(g.createProgressHandler(store))
	progress.Sequence = 2
	progress.Condition = "succeeded"
	progress.Current = registry.CreateStageMark{Stage: registry.CreateStageReady, Attempt: 1, StartedAt: now, CompletedAt: &now}
	progress.LastCompleted = &progress.Current
	progress.Outcome = &registry.CreateRequestResult{Phase: "succeeded", Sandbox: &registry.Sandbox{ID: "sandbox", Status: registry.StatusRunning}}
	body, _ = json.Marshal(createops.ProgressUpdate{Worker: worker, Progress: progress})
	if response = post(body, "worker-token"); response.Code != http.StatusOK {
		t.Fatalf("terminal: %d %s", response.Code, response.Body.String())
	}
	pending, err := store.Pending(ctx)
	if err != nil || len(pending) != 1 || pending[0].Members[0].Outcome != nil || g.route["sandbox"] != "" {
		t.Fatalf("progress published completion or route: %+v %v", pending, err)
	}
	endpoint := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode([]createops.Outcome{{ID: member.ID, Sandbox: progress.Outcome.Sandbox, Routable: true}})
	}))
	defer endpoint.Close()
	registerCreateWorker(t, g, cluster.Heartbeat{HostID: worker.HostID, RegistryID: worker.RegistryID, Addr: endpoint.URL, CreateProgress: true})
	outcomes, err := g.Execute(ctx, worker, createops.Command{Members: []createops.Member{member}, Owner: &progress.Owner})
	if err != nil {
		t.Fatal(err)
	}
	if g.route["sandbox"] != worker.HostID {
		t.Fatal("fresh Execute did not publish route")
	}
	op, err = store.Record(ctx, "operation", outcomes)
	if err != nil || op.CompletedAt == nil {
		t.Fatalf("record: %+v %v", op, err)
	}
}

func TestCreateCommandProgressCapability(t *testing.T) {
	for _, tc := range []struct {
		name              string
		assigned, current bool
	}{
		{"legacy", false, false},
		{"capable", true, true},
		{"upgraded legacy assignment", false, true},
		{"downgraded owned assignment", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls++
				if tc.assigned {
					var command createops.Command
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					if err := decoder.Decode(&command); err != nil || command.Owner == nil || command.Owner.OperationID != "operation" {
						t.Errorf("owner: %+v %v", command, err)
					}
				} else {
					var command struct {
						RegistryID   string
						Members      []createops.Member
						SnapshotPeer string
					}
					decoder := json.NewDecoder(r.Body)
					decoder.DisallowUnknownFields()
					if err := decoder.Decode(&command); err != nil {
						t.Errorf("legacy strict decoder: %v", err)
						w.WriteHeader(400)
						return
					}
				}
				_ = json.NewEncoder(w).Encode([]createops.Outcome{{ID: "member", Failure: &createops.Failure{Status: 409}}})
			}))
			defer worker.Close()
			g := liveGateway()
			register := func(capable bool) {
				registerCreateWorker(t, g, cluster.Heartbeat{HostID: "worker", RegistryID: "registry", Addr: worker.URL, SlotsTotal: 4, CreateProgress: capable})
			}
			register(tc.assigned)
			placement, err := g.Place(context.Background(), createops.Spec{}, 1)
			if err != nil {
				t.Fatal(err)
			}
			defer placement.Release(nil)
			if placement.Worker.CreateProgress != tc.assigned {
				t.Fatal("placement lost capability")
			}
			register(tc.current)
			_, err = g.Execute(context.Background(), placement.Worker, createops.Command{Owner: &registry.CreateProgressOwner{CoordinatorID: "coordinator", OperationID: "operation"}, Members: []createops.Member{{ID: "member"}}})
			if tc.assigned && !tc.current {
				if err == nil || calls != 0 {
					t.Fatalf("downgrade sent anonymous command: calls=%d err=%v", calls, err)
				}
			} else if err != nil || calls != 1 {
				t.Fatalf("calls=%d err=%v", calls, err)
			}
		})
	}
}
