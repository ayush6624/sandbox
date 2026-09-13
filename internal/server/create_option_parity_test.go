package server

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestCreatePartialResourcesAllocationAndReplay(t *testing.T) {
	for _, tc := range []struct {
		name      string
		resources *createops.Resources
		cpu, mem  int64
	}{
		{"omitted", nil, 1, 128},
		{"zero", &createops.Resources{}, 1, 128},
		{"cpu", &createops.Resources{VCPU: 2}, 2, 128},
		{"memory", &createops.Resources{MemoryMIB: 256}, 1, 256},
		{"both", &createops.Resources{VCPU: 2, MemoryMIB: 256}, 2, 256},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := createRequestServer(t)
			t.Setenv("PATH", t.TempDir())
			if tc.resources != nil && (tc.resources.VCPU > 0 || tc.resources.MemoryMIB > 0) {
				if _, err := s.reg.CreateWarmForTemplate(context.Background(), "warm", "/tmp/warm", "golden", "golden", 1, 128); err != nil {
					t.Fatal(err)
				}
				if err := s.reg.MarkWarmReady(context.Background(), "warm"); err != nil {
					t.Fatal(err)
				}
				s.golden.Store(&registry.Snapshot{ID: "golden", Golden: true})
			}
			// Capture the allocated values before the intentional missing-rootfs failure removes the row.
			ageLedger(t, s.reg.Path(), `CREATE TABLE allocated_resources (vcpu INTEGER, memory INTEGER); CREATE TRIGGER capture_resources AFTER INSERT ON sandboxes BEGIN INSERT INTO allocated_resources VALUES (NEW.vcpus, NEW.mem_mib); END`)
			member := createops.Member{ID: "partial", Spec: createops.Spec{Resources: tc.resources, Lifecycle: createops.Lifecycle{IdleTimeoutSeconds: -1}}}
			command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
			for i := 0; i < 2; i++ {
				outcomes, err := s.Execute(context.Background(), createops.Worker{RegistryID: s.reg.RegistryID()}, command)
				if err != nil || len(outcomes) != 1 || outcomes[0].Failure == nil || outcomes[0].Failure.Status != 500 {
					t.Fatalf("failure/replay: %+v %v", outcomes, err)
				}
			}
			db, err := sql.Open("sqlite", s.reg.Path())
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			var cpu, mem int64
			var count int
			if err := db.QueryRow(`SELECT COUNT(*), vcpu, memory FROM allocated_resources`).Scan(&count, &cpu, &mem); err != nil {
				t.Fatal(err)
			}
			if count != 1 || cpu != tc.cpu || mem != tc.mem {
				t.Fatalf("allocations=%d resources=%d/%d, want 1 and %d/%d", count, cpu, mem, tc.cpu, tc.mem)
			}
		})
	}
}

func TestColdPartialResourcesAdmission(t *testing.T) {
	s := createRequestServer(t)
	s.reg.SetMemAccounting(registry.MemAccounting{BudgetMIB: 127, TemplateMemMIB: 1})
	_, err := s.createCold(context.Background(), "", nil, 0, 2, 0, "")
	if !errors.Is(err, registry.ErrMemExhausted) {
		t.Fatalf("worker default memory escaped admission: %v", err)
	}
	rows, err := s.reg.All(context.Background())
	if err != nil || len(rows) != 0 {
		t.Fatalf("admission rows=%+v err=%v", rows, err)
	}
}

func TestCreateZeroResourcesRemainWarm(t *testing.T) {
	for _, resources := range []*createops.Resources{nil, {}} {
		s := createRequestServer(t)
		ctx := context.Background()
		if _, err := s.reg.CreateWarmForTemplate(ctx, "warm", "/tmp/warm", "golden", "golden", 1, 128); err != nil {
			t.Fatal(err)
		}
		if err := s.reg.MarkWarmReady(ctx, "warm"); err != nil {
			t.Fatal(err)
		}
		s.golden.Store(&registry.Snapshot{ID: "golden", Golden: true})
		member := createops.Member{ID: "warm-request", Spec: createops.Spec{Resources: resources, Lifecycle: createops.Lifecycle{IdleTimeoutSeconds: -1}}}
		command := createops.Command{RegistryID: s.reg.RegistryID(), Members: []createops.Member{member}}
		outcomes, err := s.Execute(ctx, createops.Worker{RegistryID: s.reg.RegistryID()}, command)
		if err != nil || len(outcomes) != 1 || outcomes[0].Sandbox == nil {
			t.Fatalf("warm outcome: %+v %v", outcomes, err)
		}
		got := outcomes[0].Sandbox
		if got.ID != "warm" || got.Vcpus != 1 || got.MemMIB != 128 || got.HibernateAfterSec != -1 {
			t.Fatalf("warm values: %+v", got)
		}
	}
}

func TestPublicFieldsDisabledIdle(t *testing.T) {
	s := createRequestServer(t)
	ctx := context.Background()
	if _, err := s.reg.CreateStarting(ctx, "sandbox", "", "/tmp/disk", nil, "", 0, 1, 128); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"idle_timeout_seconds":-1}`, 200},
		{`{"ttl_seconds":60}`, 200},
		{`{"idle_timeout_seconds":-2}`, 400},
	} {
		req := httptest.NewRequest(http.MethodPatch, "/sandboxes/sandbox/public-fields", bytes.NewBufferString(tc.body))
		req.SetPathValue("id", "sandbox")
		response := httptest.NewRecorder()
		s.handlePublicFields(response, req)
		if response.Code != tc.status {
			t.Fatalf("%s: HTTP %d %s", tc.body, response.Code, response.Body.String())
		}
		got, err := s.reg.Get(ctx, "sandbox")
		if err != nil || got.HibernateAfterSec != -1 {
			t.Fatalf("idle changed: %+v %v", got, err)
		}
	}
	got, err := s.reg.Get(ctx, "sandbox")
	if err != nil || got.ExpiresAt == nil {
		t.Fatalf("TTL was not applied: %+v %v", got, err)
	}
}

func TestWorkerCreateOptionValidation(t *testing.T) {
	s := createRequestServer(t)
	for _, tc := range []struct {
		name    string
		spec    createops.Spec
		invalid bool
	}{
		{"disabled idle", createops.Spec{Lifecycle: createops.Lifecycle{IdleTimeoutSeconds: -1}}, false},
		{"invalid idle", createops.Spec{Lifecycle: createops.Lifecycle{IdleTimeoutSeconds: -2}}, true},
		{"invalid ttl", createops.Spec{Lifecycle: createops.Lifecycle{TTLSeconds: -1}}, true},
		{"negative cpu", createops.Spec{Resources: &createops.Resources{VCPU: -1}}, true},
		{"negative memory", createops.Spec{Resources: &createops.Resources{MemoryMIB: -1}}, true},
		{"snapshot zero", createops.Spec{Source: createops.Source{Type: "snapshot", ID: "saved"}, Resources: &createops.Resources{}}, false},
		{"snapshot cpu", createops.Spec{Source: createops.Source{Type: "snapshot", ID: "saved"}, Resources: &createops.Resources{VCPU: 1}}, true},
		{"snapshot memory", createops.Spec{Source: createops.Source{Type: "snapshot", ID: "saved"}, Resources: &createops.Resources{MemoryMIB: 128}}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.validateCreateSpec(tc.spec); (err != nil) != tc.invalid {
				t.Fatalf("validation: %v, invalid=%t", err, tc.invalid)
			}
		})
	}
}
