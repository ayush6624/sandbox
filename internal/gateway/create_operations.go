package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/createops"
)

var _ createops.Executor = (*Gateway)(nil)

func (g *Gateway) Place(ctx context.Context, spec createops.Spec, count int) (createops.Placement, error) {
	snapshot := spec.Source.Type == "snapshot" || (spec.Source.Type == "template" && spec.Source.ID != "default")
	units := 1
	if spec.Resources != nil {
		units = g.createDemandUnits(spec.Resources.MemoryMIB)
	}
	reserve := func() *host {
		excluded := map[string]bool{}
		g.mu.RLock()
		for id, h := range g.hosts {
			if h.registryID == "" {
				excluded[id] = true
			}
		}
		g.mu.RUnlock()
		if snapshot {
			return g.reserveHostForTemplate(excluded, count, g.snapshotOwner(spec.Source.ID), spec.Source.ID)
		}
		return g.reserveHostCreate(excluded, units, spec.Resources == nil)
	}
	h := reserve()
	if h == nil {
		demand := units
		if snapshot {
			demand = count
		}
		h = g.awaitHostWith(ctx, time.Now().Add(g.queueWait), demand, reserve)
	}
	if h == nil {
		return createops.Placement{}, errors.New("no worker with capacity for create operation")
	}
	return createops.Placement{
		Worker: createops.Worker{HostID: h.id, RegistryID: h.registryID, CreateProgress: h.createProgress},
		Release: func(outcomes []createops.Outcome) {
			// Release the whole reservation even after an ambiguous response.
			// The recorded assignment still pins retries to this registry.
			succeeded := 0
			for _, outcome := range outcomes {
				if outcome.Sandbox != nil {
					succeeded++
				}
			}
			g.releaseCreateReservation(h, succeeded)
			g.createsOK.Add(int64(succeeded))
		},
	}, nil
}

func (g *Gateway) Execute(ctx context.Context, worker createops.Worker, command createops.Command) ([]createops.Outcome, error) {
	h := g.hostByID(worker.HostID)
	if h == nil || h.registryID != worker.RegistryID {
		return nil, errors.New("assigned worker registry is unavailable")
	}
	command.RegistryID = worker.RegistryID
	if !worker.CreateProgress {
		command.Owner = nil
	} else if command.Owner != nil && !h.createProgress {
		return nil, errors.New("assigned worker progress capability is unavailable")
	}
	if len(command.Members) > 0 {
		source := command.Members[0].Spec.Source
		if source.Type == "snapshot" || (source.Type == "template" && source.ID != "default") {
			if owner := g.hostByID(g.snapshotOwner(source.ID)); owner != nil && owner.id != h.id {
				command.SnapshotPeer = client.EndpointURL(owner.addr)
			}
		}
	}
	body, err := json.Marshal(command)
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		client.EndpointURL(h.addr)+"/internal/v1/create-commands", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	response, err := snapClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("assigned worker command: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, 4096))
		return nil, fmt.Errorf("assigned worker command returned HTTP %d", response.StatusCode)
	}
	var outcomes []createops.Outcome
	if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&outcomes); err != nil {
		return nil, fmt.Errorf("decode worker command: %w", err)
	}
	if err := validateCreateOutcomes(command.Members, outcomes); err != nil {
		return nil, err
	}
	g.mu.Lock()
	current := g.hosts[h.id]
	for _, outcome := range outcomes {
		if outcome.Sandbox != nil {
			outcome.Sandbox.HostAddr = hostOnly(h.addr)
			owner := g.route[outcome.Sandbox.ID]
			if outcome.Routable && current != nil && current.registryID == worker.RegistryID && (owner == "" || owner == h.id) {
				g.route[outcome.Sandbox.ID] = h.id
				g.pinRouteLocked(outcome.Sandbox.ID)
			}
		}
	}
	g.mu.Unlock()
	return outcomes, nil
}

func validateCreateOutcomes(members []createops.Member, outcomes []createops.Outcome) error {
	if len(outcomes) != len(members) {
		return errors.New("worker command returned an incomplete result set")
	}
	pending := make(map[string]bool, len(members))
	sandboxes := make(map[string]bool, len(members))
	for _, member := range members {
		pending[member.ID] = true
	}
	for _, outcome := range outcomes {
		if !pending[outcome.ID] || (outcome.Sandbox == nil) == (outcome.Failure == nil) {
			return errors.New("worker command returned an invalid member outcome")
		}
		if outcome.Sandbox != nil {
			if outcome.Sandbox.ID == "" || sandboxes[outcome.Sandbox.ID] {
				return errors.New("worker command returned an invalid sandbox identity")
			}
			sandboxes[outcome.Sandbox.ID] = true
		}
		if outcome.Failure != nil && (outcome.Failure.Status < 400 || outcome.Failure.Status > 599) {
			return errors.New("worker command returned an invalid failure status")
		}
		delete(pending, outcome.ID)
	}
	return nil
}

func (g *Gateway) createProgressHandler(store createops.Store) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		decoder := json.NewDecoder(r.Body)
		decoder.DisallowUnknownFields()
		var update createops.ProgressUpdate
		if err := decoder.Decode(&update); err != nil {
			httpError(w, http.StatusBadRequest, fmt.Errorf("decode create progress: %w", err))
			return
		}
		if err := decoder.Decode(new(any)); err != io.EOF {
			httpError(w, http.StatusBadRequest, errors.New("create progress requires one JSON object"))
			return
		}
		g.mu.RLock()
		h := g.hosts[update.Worker.HostID]
		registered := h != nil && update.Worker.RegistryID != "" && h.registryID == update.Worker.RegistryID
		g.mu.RUnlock()
		if !registered {
			httpError(w, http.StatusConflict, errors.New("create progress worker registry is unavailable"))
			return
		}
		sequence, err := store.IngestProgress(r.Context(), update.Worker, update.Progress)
		if err != nil {
			status := http.StatusInternalServerError
			if errors.Is(err, createops.ErrNotFound) {
				status = http.StatusNotFound
			} else if errors.Is(err, createops.ErrProgressConflict) {
				status = http.StatusConflict
			}
			httpError(w, status, err)
			return
		}
		writeJSON(w, http.StatusOK, createops.ProgressAcknowledgement{Sequence: sequence})
	}
}
