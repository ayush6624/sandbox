package apiv1

import (
	"context"
	"fmt"
	"net/http"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

type measuredCreateExecutor struct {
	t                              *testing.T
	store                          createops.Store
	mu                             sync.Mutex
	active, peak, placed, released int
}

func (e *measuredCreateExecutor) Place(_ context.Context, _ createops.Spec, _ int) (createops.Placement, error) {
	e.mu.Lock()
	e.placed++
	e.mu.Unlock()
	return createops.Placement{Worker: createops.Worker{HostID: "worker", RegistryID: "database"}, Release: func([]createops.Outcome) {
		e.mu.Lock()
		e.released++
		e.mu.Unlock()
	}}, nil
}

func (e *measuredCreateExecutor) Execute(ctx context.Context, worker createops.Worker, command createops.Command) ([]createops.Outcome, error) {
	op, err := e.store.Get(ctx, "operation")
	if err != nil {
		return nil, err
	}
	for _, member := range command.Members {
		stored := op.Members[member.Index]
		if stored.Worker == nil || *stored.Worker != worker {
			e.t.Errorf("member %s dispatched before its assignment was persisted", member.ID)
		}
	}
	e.mu.Lock()
	e.active += len(command.Members)
	e.peak = max(e.peak, e.active)
	e.mu.Unlock()
	defer func() {
		e.mu.Lock()
		e.active -= len(command.Members)
		e.mu.Unlock()
	}()
	select {
	case <-time.After(20 * time.Millisecond):
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	outcomes := make([]createops.Outcome, len(command.Members))
	for i, member := range command.Members {
		outcomes[len(outcomes)-i-1] = createops.Outcome{ID: member.ID, Sandbox: &registry.Sandbox{ID: "sandbox-" + member.ID}}
	}
	return outcomes, nil
}

func TestCreateDispatcherBoundsMembersAcrossFreshAndRecoveredChunks(t *testing.T) {
	for _, snapshot := range []bool{false, true} {
		for _, recovered := range []int{0, 5, 10} {
			t.Run(fmt.Sprintf("snapshot=%t/recovered=%d", snapshot, recovered), func(t *testing.T) {
				ctx := context.Background()
				store, err := createops.Open(filepath.Join(t.TempDir(), "operations.db"))
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				members := make([]createops.Member, 10)
				for i := range members {
					members[i] = createops.Member{ID: fmt.Sprint(i), Index: i}
					if snapshot {
						members[i].Spec.Source = createops.Source{Type: "snapshot", ID: "snapshot"}
					}
				}
				accepted, err := store.Accept(ctx, createops.AcceptRequest{ID: "operation", Scope: "scope", BodyHash: []byte{1}, Type: "sandbox_batch_create", MaxParallelism: 4, Members: members, ResponseStatus: 202, ResponseBody: []byte("{}")})
				if err != nil {
					t.Fatal(err)
				}
				if recovered > 0 {
					ids := make([]string, recovered)
					for i := range ids {
						ids[i] = members[i].ID
					}
					if err := store.Assign(ctx, "operation", ids, createops.Worker{HostID: "worker", RegistryID: "database"}); err != nil {
						t.Fatal(err)
					}
					accepted.Operation, err = store.Get(ctx, "operation")
					if err != nil {
						t.Fatal(err)
					}
				}
				executor := &measuredCreateExecutor{t: t, store: store}
				handler := NewWithCreateOperations(http.NotFoundHandler(), store, executor)
				handler.executeOperation(ctx, accepted.Operation)
				if executor.peak > 4 || executor.peak < 2 {
					t.Fatalf("peak members in flight = %d, expected bounded concurrency 2..4", executor.peak)
				}
				if executor.placed != executor.released || (recovered == 10 && executor.placed != 0) {
					t.Fatalf("placement ownership: placed=%d released=%d", executor.placed, executor.released)
				}
				completed, err := store.Get(ctx, "operation")
				if err != nil || completed.Status != "succeeded" {
					t.Fatalf("completion=%+v error=%v", completed, err)
				}
				for i, member := range completed.Members {
					if member.Index != i || member.Outcome == nil || member.Outcome.Sandbox.ID != "sandbox-"+member.ID {
						t.Fatalf("out-of-order response corrupted member index %d: %+v", i, member)
					}
				}
			})
		}
	}
}

type progressCreateExecutor struct {
	place   func(context.Context) (createops.Placement, error)
	execute func(context.Context, createops.Command) ([]createops.Outcome, error)
}

func (e progressCreateExecutor) Place(ctx context.Context, _ createops.Spec, _ int) (createops.Placement, error) {
	return e.place(ctx)
}
func (e progressCreateExecutor) Execute(ctx context.Context, _ createops.Worker, cmd createops.Command) ([]createops.Outcome, error) {
	return e.execute(ctx, cmd)
}

func TestCreateDispatcherPlacementQueueAndRetryProgress(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	store, err := createops.Open(filepath.Join(t.TempDir(), "ops.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	accepted, err := store.Accept(ctx, createops.AcceptRequest{ID: "operation", Scope: "scope", BodyHash: []byte{1}, Type: "sandbox_batch_create", MaxParallelism: 1, Members: []createops.Member{{ID: "first", Index: 0}, {ID: "second", Index: 1}}, ResponseStatus: 202, ResponseBody: []byte("{}")})
	if err != nil {
		t.Fatal(err)
	}
	placing := make(chan struct{}, 2)
	allowPlacement := make(chan struct{})
	executor := progressCreateExecutor{
		place: func(ctx context.Context) (createops.Placement, error) {
			placing <- struct{}{}
			select {
			case <-allowPlacement:
			case <-ctx.Done():
				return createops.Placement{}, ctx.Err()
			}
			return createops.Placement{Worker: createops.Worker{HostID: "h", RegistryID: "r"}, Release: func([]createops.Outcome) {}}, nil
		},
		execute: func(context.Context, createops.Command) ([]createops.Outcome, error) {
			return nil, fmt.Errorf("ambiguous transport error")
		},
	}
	handler := NewWithCreateOperations(http.NotFoundHandler(), store, executor)
	done := make(chan struct{})
	go func() { defer close(done); handler.executeOperation(ctx, accepted.Operation) }()
	defer func() { cancel(); <-done }()
	select {
	case <-placing:
	case <-time.After(5 * time.Second):
		t.Fatal("placement never started")
	}
	op, err := store.Get(ctx, "operation")
	if err != nil {
		t.Fatal(err)
	}
	if op.Members[0].Coordination.Phase != "placing" || op.Members[1].Coordination.Phase != "queued" {
		t.Fatalf("blocked placement progress: %+v %+v", op.Members[0].Coordination, op.Members[1].Coordination)
	}
	close(allowPlacement)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("dispatcher never returned")
	}
	op, err = store.Get(ctx, "operation")
	if err != nil {
		t.Fatal(err)
	}
	retry := *op.Members[0].Coordination
	if retry.Phase != "retrying" || op.CompletedAt != nil {
		t.Fatalf("ambiguous execute lost retry: %+v", op)
	}
	executor.execute = func(ctx context.Context, cmd createops.Command) ([]createops.Outcome, error) {
		live, err := store.Get(ctx, "operation")
		if err != nil {
			return nil, err
		}
		if cmd.Members[0].ID == "first" && *live.Members[0].Coordination != retry {
			t.Error("starting Execute cleared retry timestamp")
		}
		outcomes := make([]createops.Outcome, len(cmd.Members))
		for i, m := range cmd.Members {
			outcomes[i] = createops.Outcome{ID: m.ID, Failure: &createops.Failure{Code: "denied"}}
		}
		return outcomes, nil
	}
	handler.executor = executor
	handler.executeOperation(ctx, op)
	op, err = store.Get(ctx, "operation")
	if err != nil {
		t.Fatal(err)
	}
	if op.CompletedAt == nil || op.Members[0].Coordination.Phase != "completed" {
		t.Fatalf("fresh result failed to complete retry: %+v", op)
	}
}
