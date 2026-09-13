package apiv1

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/registry"
)

type progressReadExecutor struct{ t *testing.T }

func (e progressReadExecutor) Place(context.Context, createops.Spec, int) (createops.Placement, error) {
	e.t.Fatal("HTTP request called Place")
	return createops.Placement{}, nil
}
func (e progressReadExecutor) Execute(context.Context, createops.Worker, createops.Command) ([]createops.Outcome, error) {
	e.t.Fatal("HTTP request called Execute")
	return nil, nil
}

func progressHTTPHandler(t *testing.T, store createops.Store) http.Handler {
	h := NewWithCreateOperations(http.NotFoundHandler(), store, progressReadExecutor{t})
	mux := http.NewServeMux()
	h.Register(mux)
	return httpapi.Middleware(mux)
}

func readProgressOperation(t *testing.T, h http.Handler, id string) Operation {
	t.Helper()
	get := httptest.NewRecorder()
	h.ServeHTTP(get, httptest.NewRequest("GET", "/v1/operations/"+id, nil))
	if get.Code != 200 {
		t.Fatalf("GET: %d %s", get.Code, get.Body.String())
	}
	var op Operation
	if err := json.Unmarshal(get.Body.Bytes(), &op); err != nil {
		t.Fatal(err)
	}
	list := httptest.NewRecorder()
	h.ServeHTTP(list, httptest.NewRequest("GET", "/v1/operations", nil))
	var page struct {
		Operations []Operation `json:"operations"`
	}
	if list.Code != 200 {
		t.Fatalf("list: %d %s", list.Code, list.Body.String())
	}
	if err := json.Unmarshal(list.Body.Bytes(), &page); err != nil {
		t.Fatal(err)
	}
	if len(page.Operations) != 1 || !reflect.DeepEqual(page.Operations[0], op) {
		t.Fatalf("GET/list differ: %s / %s", get.Body.String(), list.Body.String())
	}
	for _, secret := range []string{"private-host", "private-registry", "private-sandbox", "RequestHash", "request_hash", "CoordinatorID", "coordinator_id", "owner", "raw-internal-error"} {
		if strings.Contains(get.Body.String(), secret) {
			t.Fatalf("internal field leaked: %s", get.Body.String())
		}
	}
	return op
}

func TestOperationProgressHTTPAndReopen(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "operations.db")
	store, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { store.Close() }()
	h := progressHTTPHandler(t, store)
	post := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/v1/sandbox-batches", strings.NewReader(`{"count":2,"sandbox":{}}`))
		req.Header.Set("Idempotency-Key", "progress-key")
		req.Header.Set("X-Request-ID", "public-request")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, req)
		return w
	}
	accepted := post()
	if accepted.Code != 202 {
		t.Fatalf("accept: %d %s", accepted.Code, accepted.Body.String())
	}
	var initial Operation
	if err := json.Unmarshal(accepted.Body.Bytes(), &initial); err != nil {
		t.Fatal(err)
	}
	op := readProgressOperation(t, h, initial.ID)
	if op.RequestID != "public-request" || len(op.Results) != 2 {
		t.Fatalf("operation: %+v", op)
	}
	for _, item := range op.Results {
		if item.Progress == nil || item.Progress.Coordination.Phase != "queued" || item.Progress.Coordination.UpdatedAt == nil || item.Progress.Worker != nil {
			t.Fatalf("queued: %+v", item)
		}
	}
	stored, err := store.Get(ctx, op.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker := createops.Worker{HostID: "private-host", RegistryID: "private-registry", CreateProgress: true}
	ids := []string{stored.Members[0].ID, stored.Members[1].ID}
	if err := store.Assign(ctx, op.ID, ids, worker); err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 9, 13, 10, 0, 0, 0, time.UTC)
	observation := func(i int) registry.CreateProgress {
		spec, _ := json.Marshal(stored.Members[i].Spec)
		return registry.CreateProgress{ID: ids[i], RegistryID: worker.RegistryID, RequestHash: sha256.Sum256(spec), Owner: registry.CreateProgressOwner{CoordinatorID: store.CoordinatorID(), OperationID: op.ID}, Target: registry.CreateProgressLocal, Attempt: 2, Sequence: 3, Condition: "active", Current: registry.CreateStageMark{Stage: registry.CreateStageAgent, Attempt: 2, StartedAt: now}, LastCompleted: &registry.CreateStageMark{Stage: registry.CreateStageNetwork, Attempt: 1, StartedAt: now.Add(-time.Minute), CompletedAt: &now}, ObservedAt: now}
	}
	p := observation(0)
	if _, err := store.IngestProgress(ctx, worker, p); err != nil {
		t.Fatal(err)
	}
	if err := store.SetCoordination(ctx, op.ID, ids, "retrying"); err != nil {
		t.Fatal(err)
	}
	older := p
	older.Sequence = 2
	older.Current.Stage = registry.CreateStageSource
	if _, err := store.IngestProgress(ctx, worker, older); err != nil {
		t.Fatal(err)
	}
	op = readProgressOperation(t, h, op.ID)
	got := op.Results[0].Progress
	if got.Coordination.Phase != "retrying" || got.Worker.Sequence != 3 || got.Worker.Current.Stage != "guest_agent" || got.Worker.LastCompleted.Attempt != 1 || op.CompletedAt != nil || op.Succeeded != 0 || op.Failed != 0 {
		t.Fatalf("retry: %+v %+v", op, got)
	}
	p.Sequence++
	p.Condition = "failed"
	p.Outcome = &registry.CreateRequestResult{Phase: "failed", Status: 500, Code: "create_interrupted", Detail: "Interrupted."}
	if _, err := store.IngestProgress(ctx, worker, p); err != nil {
		t.Fatal(err)
	}
	success := observation(1)
	success.Condition = "succeeded"
	success.Current = registry.CreateStageMark{Stage: registry.CreateStageReady, Attempt: 2, StartedAt: now, CompletedAt: &now}
	success.LastCompleted = &success.Current
	success.Outcome = &registry.CreateRequestResult{Phase: "succeeded", Sandbox: &registry.Sandbox{ID: "private-sandbox"}}
	if _, err := store.IngestProgress(ctx, worker, success); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	h = progressHTTPHandler(t, store)
	op = readProgressOperation(t, h, op.ID)
	failed, unresolved := op.Results[0], op.Results[1]
	if failed.Error == nil || failed.Error.Code != "create_interrupted" || failed.Progress.Coordination.Phase != "completed" || failed.Progress.Worker.Condition != "failed" || failed.Progress.Worker.LastCompleted.Stage != "guest_network" {
		t.Fatalf("failure: %+v", failed)
	}
	if unresolved.Progress.Coordination.Phase != "retrying" || unresolved.Progress.Worker.Condition != "succeeded" || unresolved.Sandbox != nil || op.CompletedAt != nil || op.Succeeded != 0 || op.Failed != 1 {
		t.Fatalf("success observation completed operation: %+v", op)
	}
	replay := post()
	if replay.Code != accepted.Code || replay.Body.String() != accepted.Body.String() {
		t.Fatalf("replay changed: %s / %s", accepted.Body.String(), replay.Body.String())
	}
}

func TestOperationProgressLegacyAndFutureStageHTTP(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "operations.db")
	store, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	req := createops.AcceptRequest{ID: "legacy", Type: "sandbox_batch_create", Scope: "legacy", BodyHash: []byte("hash"), Members: []createops.Member{{ID: "queued", Index: 0}, {ID: "assigned", Index: 1}, {ID: "done", Index: 2}}, MaxParallelism: 1, ResponseStatus: 202, ResponseBody: []byte(`{}`)}
	if _, err := store.Accept(ctx, req); err != nil {
		t.Fatal(err)
	}
	if err := store.Assign(ctx, req.ID, []string{"assigned"}, createops.Worker{HostID: "private-host", RegistryID: "private-registry"}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Record(ctx, req.ID, []createops.Outcome{{ID: "done", Failure: &createops.Failure{Status: 503, Code: "unavailable"}}}); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`UPDATE create_members SET coordination_phase=NULL, coordination_updated_at=NULL`); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	p := registry.CreateProgress{Attempt: 1, Sequence: 1, Condition: "active", Current: registry.CreateStageMark{Stage: "future_stage", Attempt: 1, StartedAt: now}, ObservedAt: now}
	body, _ := json.Marshal(p)
	if _, err := db.Exec(`UPDATE create_members SET progress=? WHERE member_id='assigned'`, body); err != nil {
		t.Fatal(err)
	}
	h := progressHTTPHandler(t, store)
	op := readProgressOperation(t, h, req.ID)
	for i, phase := range []string{"queued", "assigned", "completed"} {
		got := op.Results[i].Progress
		if got.Coordination.Phase != phase || got.Coordination.UpdatedAt != nil {
			t.Fatalf("legacy %s: %+v", phase, got)
		}
	}
	if op.Results[1].Progress.Worker.Current.Stage != "future_stage" || op.Results[0].Progress.Worker != nil || op.Results[2].Progress.Worker != nil {
		t.Fatalf("legacy worker: %+v", op.Results)
	}
}
