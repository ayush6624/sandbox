package server

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

func createRequestServer(t *testing.T) *Server {
	t.Helper()
	dir := t.TempDir()
	reg, err := registry.Open(filepath.Join(dir, "registry.db"), registry.Pools{TapPrefix: "fc", TapMax: 3, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.12", PortMin: 5200, PortMax: 5202})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reg.Close() })
	s := New(Config{HostID: "test-host", VMTemplate: vm.RunOptions{Vcpus: 1, MemMIB: 128}, Provisioner: &provisioner.Provisioner{RootfsDir: filepath.Join(dir, "rootfs"), SnapshotDir: filepath.Join(dir, "snapshots")}}, reg)
	t.Cleanup(s.pf.CloseAll)
	return s
}

func memberIntent(t *testing.T, member createops.Member) registry.CreateIntent {
	t.Helper()
	data, err := json.Marshal(member.Spec)
	if err != nil {
		t.Fatal(err)
	}
	return registry.CreateIntent{ID: member.ID, Hash: sha256.Sum256(data), SourceType: "default", Metadata: member.Spec.Metadata}
}

func TestCreateCommandSourceErrors(t *testing.T) {
	for _, tc := range []struct {
		name, sourceType, meta, peer string
		bucket                       bool
		status                       int
	}{
		{name: "missing local snapshot", sourceType: "snapshot", status: 404},
		{name: "missing durable snapshot", sourceType: "snapshot", bucket: true, status: 404},
		{name: "missing template", sourceType: "template", bucket: true, status: 404},
		{name: "deleted snapshot", sourceType: "snapshot", bucket: true, meta: `{"id":"missing-source","deleted_at":"2026-09-13T00:00:00Z"}`, status: 404},
		{name: "corrupt metadata", sourceType: "snapshot", bucket: true, meta: `{`, status: 500},
		{name: "peer unavailable before durability", sourceType: "snapshot", bucket: true, peer: "http://127.0.0.1:1", status: 500},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := createRequestServer(t)
			if tc.bucket {
				store := newUploadTestStore(t)
				s.blob = store.client
				if tc.meta != "" {
					store.objects[snapObj("missing-source", "meta.json")] = uploadTestObject{data: []byte(tc.meta), generation: 1}
				}
			}
			command := createops.Command{RegistryID: s.reg.RegistryID(), SnapshotPeer: tc.peer, Members: []createops.Member{
				{ID: "source-error", Spec: createops.Spec{Source: createops.Source{Type: tc.sourceType, ID: "missing-source"}}},
			}}
			for replay := 0; replay < 2; replay++ {
				outcomes, err := s.Execute(context.Background(), createops.Worker{RegistryID: s.reg.RegistryID()}, command)
				if err != nil || len(outcomes) != 1 {
					t.Fatalf("execute: %+v %v", outcomes, err)
				}
				failure := outcomes[0].Failure
				if failure == nil || failure.Status != tc.status {
					t.Fatalf("replay %d: want HTTP %d, got %+v", replay, tc.status, failure)
				}
				if tc.status == 404 && failure.Code != "source_not_found" {
					t.Fatalf("missing source code: %+v", failure)
				}
			}
			rows, err := s.reg.All(context.Background())
			if err != nil || len(rows) != 0 {
				t.Fatalf("failed source allocated resources: %+v %v", rows, err)
			}
		})
	}
}

func TestCreateCommandWarmIndexedReplayAfterDelete(t *testing.T) {
	s := createRequestServer(t)
	ctx := context.Background()
	for _, id := range []string{"warm-one", "warm-two"} {
		if _, err := s.reg.CreateWarmForTemplate(ctx, id, filepath.Join(t.TempDir(), "disk"), "golden", "golden", 1, 128); err != nil {
			t.Fatal(err)
		}
		if err := s.reg.MarkWarmReady(ctx, id); err != nil {
			t.Fatal(err)
		}
	}
	s.golden.Store(&registry.Snapshot{ID: "golden", Golden: true})
	command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{
		{ID: "first", Index: 0, Spec: createops.Spec{Name: "first", Metadata: map[string]string{"index": "zero"}}},
		{ID: "bad-middle", Index: 1, Spec: createops.Spec{Lifecycle: createops.Lifecycle{TTLSeconds: -1}}},
		{ID: "last", Index: 2, Spec: createops.Spec{Name: "last"}},
	}}
	invoke := func(command createops.Command) []createops.Outcome {
		t.Helper()
		data, _ := json.Marshal(command)
		request := httptest.NewRequest(http.MethodPost, "/internal/v1/create-commands", bytes.NewReader(data))
		response := httptest.NewRecorder()
		s.handleCreateCommand(response, request)
		if response.Code != 200 {
			t.Fatalf("command HTTP %d: %s", response.Code, response.Body.String())
		}
		var outcomes []createops.Outcome
		if err := json.Unmarshal(response.Body.Bytes(), &outcomes); err != nil {
			t.Fatal(err)
		}
		return outcomes
	}
	outcomes := invoke(command)
	if len(outcomes) != 3 || outcomes[0].ID != "first" || outcomes[1].Failure == nil || outcomes[2].ID != "last" || outcomes[0].Sandbox.ID == outcomes[2].Sandbox.ID {
		t.Fatalf("indexed outcomes: %+v", outcomes)
	}
	if outcomes[0].Sandbox.Metadata["index"] != "zero" {
		t.Fatal("metadata not captured with warm promotion")
	}
	if err := s.reg.Destroy(ctx, outcomes[0].Sandbox.ID); err != nil {
		t.Fatal(err)
	}
	replay := invoke(command)
	if replay[0].Sandbox.ID != outcomes[0].Sandbox.ID || replay[2].Sandbox.ID != outcomes[2].Sandbox.ID || replay[1].Failure.Code != outcomes[1].Failure.Code {
		t.Fatal("replay changed member outcomes")
	}
	command.Members = command.Members[:1]
	command.Members[0].Spec.Name = "different"
	conflict := invoke(command)
	if conflict[0].Failure == nil || conflict[0].Failure.Status != 409 {
		t.Fatalf("conflicting replay: %+v", conflict)
	}
}

func TestCreateCommandReconcileRetainsInterruptedOutcome(t *testing.T) {
	s := createRequestServer(t)
	ctx := context.Background()
	member := createops.Member{ID: "interrupted", Spec: createops.Spec{Name: "interrupted"}}
	intent := memberIntent(t, member)
	if _, err := s.reg.CreateStarting(ctx, "allocated", "", filepath.Join(t.TempDir(), "disk"), nil, "", 0, 1, 128, intent); err != nil {
		t.Fatal(err)
	}
	s.reconcile(ctx)
	command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
	worker := createops.Worker{HostID: s.hostID(), RegistryID: s.reg.RegistryID()}
	outcomes, err := s.Execute(ctx, worker, command)
	if err != nil || len(outcomes) != 1 || outcomes[0].Failure == nil || outcomes[0].Failure.Code != "create_interrupted" {
		t.Fatalf("reconciled outcome: %+v %v", outcomes, err)
	}
	rows, err := s.reg.All(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("replay allocated again: %+v %v", rows, err)
	}
	command.RegistryID = "old-registry"
	if _, err := s.Execute(ctx, worker, command); err == nil {
		t.Fatal("old registry assignment accepted")
	}
}

func TestCreateCommandCancellationBeforeAllocationRemainsRetryable(t *testing.T) {
	s := createRequestServer(t)
	member := createops.Member{ID: "cancel-before-allocation", Spec: createops.Spec{Source: createops.Source{Type: "snapshot", ID: "snapshot-source"}}}
	command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
	worker := createops.Worker{RegistryID: s.reg.RegistryID()}
	snapshotLock := s.snapshotLock("snapshot-source")
	snapshotLock.Lock()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	result := make(chan error, 1)
	go func() { _, err := s.Execute(ctx, worker, command); result <- err }()
	deadline := time.Now().Add(time.Second)
	for len(s.createSem) == 0 && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if len(s.createSem) == 0 {
		snapshotLock.Unlock()
		t.Fatal("create did not acquire its admission permit")
	}
	cancel()
	snapshotLock.Unlock()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation result: %v", err)
	}
	if _, err := s.reg.CreateRequest(context.Background(), memberIntent(t, member)); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("preallocation cancellation persisted a terminal outcome: %v", err)
	}
	// The same request can still reach execution. A missing source
	// is terminal, so this second outcome also proves the first wasn't cached.
	_, err := s.Execute(context.Background(), worker, command)
	if err != nil {
		t.Fatal(err)
	}
	stored, err := s.reg.CreateRequest(context.Background(), memberIntent(t, member))
	if err != nil || stored.Phase != "failed" {
		t.Fatalf("retry was not executed: %+v %v", stored, err)
	}
}
