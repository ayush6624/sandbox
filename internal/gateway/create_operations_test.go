package gateway

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestCreateCommandLostResponseStaysOnAssignedRegistry(t *testing.T) {
	var calls, otherCalls atomic.Int64
	worker := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/internal/v1/create-commands" || r.Header.Get("Authorization") != "Bearer worker-token" {
			t.Errorf("unexpected command request %s", r.URL.Path)
			w.WriteHeader(401)
			return
		}
		var command createops.Command
		if err := json.NewDecoder(r.Body).Decode(&command); err != nil {
			t.Error(err)
			return
		}
		if command.RegistryID != "registry-a" || len(command.Members) != 1 || command.Members[0].ID != "stable-member" {
			t.Errorf("command identity changed: %+v", command)
		}
		if calls.Add(1) == 1 {
			conn, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Error(err)
				return
			}
			_ = conn.Close()
			return
		}
		_ = json.NewEncoder(w).Encode([]createops.Outcome{{ID: "stable-member", Sandbox: &registry.Sandbox{ID: "same-sandbox"}}})
	}))
	defer worker.Close()
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		otherCalls.Add(1)
		w.WriteHeader(500)
	}))
	defer other.Close()
	g := liveGateway(
		&host{id: "a", registryID: "registry-a", addr: worker.URL, token: "worker-token", slotsTotal: 4},
		&host{id: "b", registryID: "registry-b", addr: other.URL, slotsTotal: 4},
	)
	placement, err := g.Place(context.Background(), createops.Spec{}, 1)
	if err != nil {
		t.Fatal(err)
	}
	command := createops.Command{Members: []createops.Member{{ID: "stable-member"}}}
	_, err = g.Execute(context.Background(), placement.Worker, command)
	placement.Release(nil)
	if err == nil {
		t.Fatal("lost response was treated as completed")
	}
	if g.hosts["a"].reserved != 0 || g.hosts["a"].reservedUnits != 0 {
		t.Fatal("ambiguous request leaked a placement reservation")
	}
	outcomes, err := g.Execute(context.Background(), placement.Worker, command)
	if err != nil || len(outcomes) != 1 || outcomes[0].Sandbox.ID != "same-sandbox" {
		t.Fatalf("replay result=%+v err=%v", outcomes, err)
	}
	if calls.Load() != 2 || otherCalls.Load() != 0 {
		t.Fatalf("request changed workers: chosen=%d other=%d", calls.Load(), otherCalls.Load())
	}
	// The host/address may be reused, but a fresh database cannot receive the
	// old request. A missing host likewise never triggers fresh placement.
	g.hosts["a"].registryID = "replacement-registry"
	if _, err := g.Execute(context.Background(), placement.Worker, command); err == nil {
		t.Fatal("replayed a command into a replacement registry")
	}
	delete(g.hosts, "a")
	if _, err := g.Execute(context.Background(), placement.Worker, command); err == nil {
		t.Fatal("replayed a command without its assigned worker")
	}
	if calls.Load() != 2 || otherCalls.Load() != 0 {
		t.Fatal("an unavailable registry caused new dispatch")
	}
}

func TestCreateCommandValidatesIndexedOutcomes(t *testing.T) {
	members := []createops.Member{{ID: "first"}, {ID: "second"}}
	first := createops.Outcome{ID: "first", Sandbox: &registry.Sandbox{ID: "sandbox-a"}}
	second := createops.Outcome{ID: "second", Failure: &createops.Failure{Status: 503, Code: "capacity_unavailable"}}
	if err := validateCreateOutcomes(members, []createops.Outcome{second, first}); err != nil {
		t.Fatalf("out-of-order indexed results rejected: %v", err)
	}
	for _, invalid := range [][]createops.Outcome{
		{first},
		{first, first},
		{first, {ID: "second"}},
		{first, {ID: "second", Sandbox: first.Sandbox}},
		{first, {ID: "second", Failure: &createops.Failure{Status: 201}}},
	} {
		if err := validateCreateOutcomes(members, invalid); err == nil {
			t.Fatalf("accepted malformed worker result: %+v", invalid)
		}
	}
}
