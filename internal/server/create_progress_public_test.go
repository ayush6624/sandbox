package server

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/apiv1"
	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestProgressDeliveryPublishesStartupFailureAfterCoordinatorReopen(t *testing.T) {
	s := createRequestServer(t)
	t.Setenv("PATH", t.TempDir())
	s.cfg.Provisioner.RootfsBase = filepath.Join(t.TempDir(), "private-missing-rootfs")
	s.cfg.Provisioner.RootfsDir = filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(s.cfg.Provisioner.RootfsDir, nil, 0600); err != nil {
		t.Fatal(err)
	}
	store, path := deliveryStore(t)
	member, intent := deliveryMember(t, s, store, "public-failure", registry.CreateProgressLocal)
	worker := createops.Worker{HostID: s.hostID(), RegistryID: s.reg.RegistryID(), CreateProgress: true}
	outcomes, err := s.Execute(context.Background(), worker, createops.Command{
		RegistryID: worker.RegistryID, Members: []createops.Member{member}, Owner: &intent.ProgressOwner,
	})
	if err != nil || len(outcomes) != 1 || outcomes[0].Failure == nil || outcomes[0].Failure.Code != "rootfs_prepare_failed" {
		t.Fatalf("worker failure: %+v %v", outcomes, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { s.runCreateProgressDelivery(ctx, store); close(done) }()
	func() {
		defer func() { cancel(); <-done }()
		deadline := time.NewTimer(2 * time.Second)
		defer deadline.Stop()
		tick := time.NewTicker(10 * time.Millisecond)
		defer tick.Stop()
		for {
			op, err := store.Get(context.Background(), intent.ProgressOwner.OperationID)
			if err != nil {
				t.Fatal(err)
			}
			if op.CompletedAt != nil {
				return
			}
			select {
			case <-deadline.C:
				t.Fatal("background delivery did not complete the failed operation")
			case <-tick.C:
			}
		}
	}()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := createops.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	h := apiv1.NewWithCreateOperations(http.NotFoundHandler(), reopened, nil)
	mux := http.NewServeMux()
	h.Register(mux)
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/operations/"+intent.ProgressOwner.OperationID, nil))
	var public apiv1.Operation
	if response.Code != http.StatusOK || json.Unmarshal(response.Body.Bytes(), &public) != nil {
		t.Fatalf("public GET: %d %s", response.Code, response.Body.String())
	}
	if public.Status != "failed" || public.Failed != 1 || public.CompletedAt == nil || len(public.Results) != 1 {
		t.Fatalf("public completion: %+v", public)
	}
	item := public.Results[0]
	if item.Error == nil || item.Error.Code != "rootfs_prepare_failed" || item.Sandbox != nil || item.Progress == nil || item.Progress.Worker == nil {
		t.Fatalf("public failure or progress missing: %+v", item)
	}
	progress := item.Progress
	if progress.Coordination.Phase != "completed" || progress.Worker.Condition != "failed" || progress.Worker.Current.Stage != "sandbox_preparation" || progress.Worker.LastCompleted == nil || progress.Worker.LastCompleted.Stage != "allocated" {
		t.Fatalf("public stage history: %+v %+v", progress, progress.Worker)
	}
	for _, private := range []string{s.cfg.Provisioner.RootfsBase, s.cfg.Provisioner.RootfsDir, store.CoordinatorID(), s.reg.RegistryID(), "RequestHash", "Routable", "ProgressOwner"} {
		if strings.Contains(response.Body.String(), private) {
			t.Fatalf("public response exposed %q: %s", private, response.Body.String())
		}
	}
	rows, err := s.reg.All(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("failed allocation survived cleanup: %+v %v", rows, err)
	}
}
