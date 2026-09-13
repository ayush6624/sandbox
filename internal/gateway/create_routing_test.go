package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/cluster"
	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func registerCreateWorker(t *testing.T, g *Gateway, heartbeat cluster.Heartbeat) {
	t.Helper()
	heartbeat.ControlToken = "worker-token"
	encoded, err := json.Marshal(heartbeat)
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	g.handleRegister(response, httptest.NewRequest(http.MethodPost, "/register", bytes.NewReader(encoded)))
	if response.Code != http.StatusNoContent {
		t.Fatalf("register HTTP %d: %s", response.Code, response.Body.String())
	}
}

func TestDelayedCreateResponsePreservesCurrentRegistryAndOwner(t *testing.T) {
	for _, replacement := range []bool{true, false} {
		name := "adopted-owner"
		if replacement {
			name = "replacement-registry"
		}
		t.Run(name, func(t *testing.T) {
			arrived := make(chan struct{})
			release := make(chan struct{}, 1)
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(arrived)
				select {
				case <-release:
				case <-r.Context().Done():
					return
				}
				_ = json.NewEncoder(w).Encode([]createops.Outcome{{ID: "member", Sandbox: &registry.Sandbox{ID: "sandbox"}, Routable: true}})
			}))
			defer worker.Close()
			defer close(release)
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			g := liveGateway(&host{id: "creator", registryID: "original-db", addr: worker.URL, token: "worker-token", slotsTotal: 4})
			type execution struct {
				outcomes []createops.Outcome
				err      error
			}
			done := make(chan execution, 1)
			go func() {
				outcomes, err := g.Execute(ctx, createops.Worker{HostID: "creator", RegistryID: "original-db"}, createops.Command{Members: []createops.Member{{ID: "member"}}})
				done <- execution{outcomes, err}
			}()
			select {
			case <-arrived:
			case <-ctx.Done():
				t.Fatal("worker request did not arrive")
			}
			if replacement {
				registerCreateWorker(t, g, cluster.Heartbeat{HostID: "creator", RegistryID: "new-db", Addr: worker.URL, SlotsTotal: 4})
			} else {
				registerCreateWorker(t, g, cluster.Heartbeat{HostID: "adopter", RegistryID: "adopter-db", Addr: "http://127.0.0.1:18081", SlotsTotal: 4, SlotsUsed: 1, SandboxIDs: []string{"sandbox"}})
			}
			release <- struct{}{}
			result := <-done
			if result.err != nil || len(result.outcomes) != 1 || result.outcomes[0].Sandbox.ID != "sandbox" {
				t.Fatalf("historical result changed: %+v, %v", result.outcomes, result.err)
			}
			g.mu.RLock()
			owner := g.route["sandbox"]
			_, pinned := g.routePin["sandbox"]
			g.mu.RUnlock()
			expected := "adopter"
			if replacement {
				expected = ""
			}
			if owner != expected || pinned {
				t.Fatalf("delayed response republished stale route: owner=%q want=%q pinned=%t", owner, expected, pinned)
			}
		})
	}
}

func TestCreateRegistryReplacementRetiresOldReservations(t *testing.T) {
	g := liveGateway(&host{id: "worker", registryID: "old-db", addr: "http://127.0.0.1:18080", slotsTotal: 8, warmReady: 2, warmReadyByTemplate: map[string]int{"template": 2}})
	spec := createops.Spec{Source: createops.Source{Type: "snapshot", ID: "template"}}
	oldPlacement, err := g.Place(context.Background(), spec, 3)
	if err != nil {
		t.Fatal(err)
	}
	if g.hosts["worker"].reserved != 1 || g.hosts["worker"].warmReserved != 2 {
		t.Fatal("test did not reserve both ordinary and warm capacity")
	}
	registerCreateWorker(t, g, cluster.Heartbeat{HostID: "worker", RegistryID: "new-db", Addr: "http://127.0.0.1:18080", SlotsTotal: 8, WarmReady: 2, WarmReadyByTemplate: map[string]int{"template": 2}})
	current := g.hosts["worker"]
	if current.reserved != 0 || current.reservedUnits != 0 || current.warmReserved != 0 || len(current.warmReservedByTemplate) != 0 {
		t.Fatalf("replacement inherited reservations: %+v", current)
	}
	nextPlacement, err := g.Place(context.Background(), spec, 3)
	if err != nil {
		t.Fatal(err)
	}
	beforeReserved, beforeUnits, beforeWarm, beforeTemplate := current.reserved, current.reservedUnits, current.warmReserved, current.warmReservedByTemplate["template"]
	oldPlacement.Release([]createops.Outcome{{ID: "old", Sandbox: &registry.Sandbox{ID: "old-sandbox"}, Routable: true}})
	if current.reserved != beforeReserved || current.reservedUnits != beforeUnits || current.warmReserved != beforeWarm || current.warmReservedByTemplate["template"] != beforeTemplate || current.slotsUsed != 0 {
		t.Fatalf("old release modified new incarnation: %+v", current)
	}
	nextPlacement.Release(nil)
	if current.reserved != 0 || current.reservedUnits != 0 || current.warmReserved != 0 || current.warmReservedByTemplate["template"] != 0 {
		t.Fatalf("new reservation did not release: %+v", current)
	}
}

func TestCreateHistoricalOutcomeDoesNotPublishRoute(t *testing.T) {
	for _, routable := range []bool{false, true} {
		name := "historical"
		if routable {
			name = "live"
		}
		t.Run(name, func(t *testing.T) {
			worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_ = json.NewEncoder(w).Encode([]createops.Outcome{{ID: "member", Sandbox: &registry.Sandbox{ID: "sandbox", Status: registry.StatusRunning}, Routable: routable}})
			}))
			defer worker.Close()
			g := liveGateway(&host{id: "worker", registryID: "db", addr: worker.URL, slotsTotal: 4})
			outcomes, err := g.Execute(context.Background(), createops.Worker{HostID: "worker", RegistryID: "db"}, createops.Command{Members: []createops.Member{{ID: "member"}}})
			if err != nil || len(outcomes) != 1 || outcomes[0].Sandbox.ID != "sandbox" {
				t.Fatalf("result=%+v err=%v", outcomes, err)
			}
			owner := g.route["sandbox"]
			_, pinned := g.routePin["sandbox"]
			if routable {
				if owner != "worker" || !pinned {
					t.Fatalf("live allocation was not routed: owner=%q pinned=%t", owner, pinned)
				}
			} else if owner != "" || pinned {
				t.Fatalf("saved running state republished historical allocation: owner=%q pinned=%t", owner, pinned)
			}
		})
	}
}

func TestCreatePartialResourceReservation(t *testing.T) {
	for _, tc := range []struct {
		name        string
		resources   *createops.Resources
		units, warm int
	}{
		{"default", nil, 0, 1},
		{"cpu only", &createops.Resources{VCPU: 2}, 1, 0},
		{"memory only", &createops.Resources{MemoryMIB: 256}, 2, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := &host{id: "worker", registryID: "db", addr: "http://127.0.0.1:18080", slotsTotal: 8, warmReady: 1}
			g := liveGateway(h)
			g.directMemPerSlotMIB = 128
			placement, err := g.Place(context.Background(), createops.Spec{Resources: tc.resources}, 1)
			if err != nil {
				t.Fatal(err)
			}
			if h.reservedUnits != tc.units || h.warmReserved != tc.warm {
				t.Fatalf("reserved units=%d warm=%d, want %d/%d", h.reservedUnits, h.warmReserved, tc.units, tc.warm)
			}
			placement.Release(nil)
			if h.reservedUnits != 0 || h.warmReserved != 0 {
				t.Fatal("reservation leaked")
			}
		})
	}
}
