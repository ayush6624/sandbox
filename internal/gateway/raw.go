package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/cluster"
	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
)

const rawIndexObject = "ingress/raw/index.json"

// A single allocation performs two generation-CAS writes (pending, then active),
// so concurrent gateway replicas contend on every allocation. Retrying that
// contention with no delay just re-collides: measured against real GCS, three
// replicas allocating 8 ports concurrently exhausted a 5-attempt budget and
// failed a legitimate request. Retry more times, and space the attempts with
// jittered backoff so colliding writers separate instead of thrashing.
const rawCASAttempts = 10

func rawCASBackoff(ctx context.Context, attempt int) {
	delay := time.Duration(20<<attempt) * time.Millisecond
	if delay > 400*time.Millisecond {
		delay = 400 * time.Millisecond
	}
	// Jitter is what actually breaks up a collision between equal writers.
	delay = delay/2 + time.Duration(rand.Int63n(int64(delay/2)+1))
	select {
	case <-ctx.Done():
	case <-time.After(delay):
	}
}

var errRawReleasing = errors.New("raw port exposure is being released")

type RawConfig struct {
	Bucket     string
	PublicHost string
	PortMin    int
	PortMax    int
}

type rawStore interface {
	GetBytesGen(context.Context, string) ([]byte, int64, error)
	PutBytesIfGenerationMatch(context.Context, string, []byte, int64) (int64, error)
}

type rawLease struct {
	PublicPort int       `json:"public_port"`
	SandboxID  string    `json:"sandbox_id"`
	GuestPort  int       `json:"guest_port"`
	LeaseID    string    `json:"lease_id"`
	State      string    `json:"state"`
	CreatedAt  time.Time `json:"created_at"`
	UpdatedAt  time.Time `json:"updated_at"`
}

type rawIndex struct {
	Version int                 `json:"version"`
	Leases  map[string]rawLease `json:"leases"`
}

// rawSnapshot is an immutable view of the lease index for the READ path.
// route() runs for every inbound public raw TCP connection, and stats() runs on
// every metrics scrape; neither may block behind an allocation, which holds
// rawAllocator.mu across GCS commits, the worker assign() round trip, and
// jittered CAS backoff (measured: a single exposure stalled route() by ~485 ms).
// Mutators publish a fresh snapshot before releasing mu; readers never take it.
type rawSnapshot struct {
	leases map[string]rawLease
	gen    int64
}

type rawAllocator struct {
	cfg   RawConfig
	store rawStore

	// mu serializes index mutation and the durable CAS behind it. It is held
	// across network I/O by design, so nothing on a latency-sensitive path may
	// acquire it — use snap instead.
	mu     sync.Mutex
	index  rawIndex
	gen    int64
	loaded bool
	// snap is the lock-free read view of index/gen, republished by
	// publishLocked at the end of every mutating critical section.
	snap atomic.Pointer[rawSnapshot]

	allocOK        atomic.Int64
	allocError     atomic.Int64
	reconcileOK    atomic.Int64
	reconcileError atomic.Int64
	conflicts      atomic.Int64
}

func (g *Gateway) ConfigureRaw(cfg RawConfig) error {
	if cfg.Bucket == "" || cfg.PublicHost == "" {
		return errors.New("raw ingress requires --ingress-bucket and --raw-public-host")
	}
	if cfg.PortMin < 1 || cfg.PortMax > 65535 || cfg.PortMin > cfg.PortMax {
		return fmt.Errorf("invalid raw port range %d-%d", cfg.PortMin, cfg.PortMax)
	}
	g.raw = newRawAllocator(cfg, gcsblob.New(cfg.Bucket))
	return nil
}

// newRawAllocator builds an allocator with its lock-free read view already
// published, so route()/stats() never observe a nil snapshot.
func newRawAllocator(cfg RawConfig, store rawStore) *rawAllocator {
	a := &rawAllocator{cfg: cfg, store: store, index: rawIndex{Version: 1, Leases: map[string]rawLease{}}}
	a.publishLocked() // no contention yet; nothing else holds a reference
	return a
}

func (a *rawAllocator) load(ctx context.Context) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publishLocked()
	return a.loadLocked(ctx)
}

func (a *rawAllocator) loadLocked(ctx context.Context) error {
	b, gen, err := a.store.GetBytesGen(ctx, rawIndexObject)
	if errors.Is(err, fs.ErrNotExist) || errors.Is(err, gcsblob.ErrNotExist) {
		a.index = rawIndex{Version: 1, Leases: map[string]rawLease{}}
		a.gen, a.loaded = 0, true
		return nil
	}
	if err != nil {
		return err
	}
	var idx rawIndex
	if err := json.Unmarshal(b, &idx); err != nil {
		return fmt.Errorf("decode raw index: %w", err)
	}
	if idx.Leases == nil {
		idx.Leases = map[string]rawLease{}
	}
	a.index, a.gen, a.loaded = idx, gen, true
	return nil
}

func (a *rawAllocator) commitLocked(ctx context.Context) error {
	b, err := json.Marshal(a.index)
	if err != nil {
		return err
	}
	gen, err := a.store.PutBytesIfGenerationMatch(ctx, rawIndexObject, b, a.gen)
	if err != nil {
		return err
	}
	a.gen = gen
	return nil
}

func (a *rawAllocator) findBySandboxLocked(id string, guestPort int) (string, rawLease, bool) {
	for key, lease := range a.index.Leases {
		if lease.SandboxID == id && lease.GuestPort == guestPort {
			return key, lease, true
		}
	}
	return "", rawLease{}, false
}

func (a *rawAllocator) allocate(ctx context.Context, id string, guestPort int,
	assign func(int) error) (rawLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publishLocked()
	if !a.loaded {
		a.allocError.Add(1)
		return rawLease{}, errors.New("raw allocator is not ready")
	}
	for attempt := 0; attempt < rawCASAttempts; attempt++ {
		if key, lease, ok := a.findBySandboxLocked(id, guestPort); ok {
			if lease.State == "active" {
				a.allocOK.Add(1)
				return lease, nil
			}
			if lease.State == "releasing" {
				a.allocError.Add(1)
				return rawLease{}, errRawReleasing
			}
			if err := assign(lease.PublicPort); err != nil {
				a.allocError.Add(1)
				return rawLease{}, err
			}
			lease.State, lease.UpdatedAt = "active", time.Now().UTC()
			a.index.Leases[key] = lease
			if err := a.commitLocked(ctx); err != nil {
				if errors.Is(err, gcsblob.ErrPreconditionFailed) {
					rawCASBackoff(ctx, attempt)
					_ = a.loadLocked(ctx)
					continue
				}
				a.allocError.Add(1)
				return rawLease{}, err
			}
			a.allocOK.Add(1)
			return lease, nil
		}
		port := 0
		for p := a.cfg.PortMin; p <= a.cfg.PortMax; p++ {
			if _, used := a.index.Leases[strconv.Itoa(p)]; !used {
				port = p
				break
			}
		}
		if port == 0 {
			a.allocError.Add(1)
			return rawLease{}, registry.ErrPoolExhausted
		}
		now := time.Now().UTC()
		lease := rawLease{
			PublicPort: port, SandboxID: id, GuestPort: guestPort,
			LeaseID: uuid.NewString(), State: "pending", CreatedAt: now, UpdatedAt: now,
		}
		key := strconv.Itoa(port)
		a.index.Leases[key] = lease
		if err := a.commitLocked(ctx); err != nil {
			if errors.Is(err, gcsblob.ErrPreconditionFailed) {
				rawCASBackoff(ctx, attempt)
				_ = a.loadLocked(ctx)
				continue
			}
			a.allocError.Add(1)
			return rawLease{}, err
		}
		if err := assign(port); err != nil {
			a.allocError.Add(1)
			if current, ok := a.index.Leases[key]; ok && current.LeaseID == lease.LeaseID {
				delete(a.index.Leases, key)
				if err := a.commitLocked(ctx); errors.Is(err, gcsblob.ErrPreconditionFailed) {
					_ = a.loadLocked(ctx)
				}
			}
			return rawLease{}, err
		}
		lease.State, lease.UpdatedAt = "active", time.Now().UTC()
		a.index.Leases[key] = lease
		if err := a.commitLocked(ctx); err != nil {
			if errors.Is(err, gcsblob.ErrPreconditionFailed) {
				rawCASBackoff(ctx, attempt)
				_ = a.loadLocked(ctx)
				continue
			}
			a.allocError.Add(1)
			return rawLease{}, err
		}
		a.allocOK.Add(1)
		return lease, nil
	}
	a.allocError.Add(1)
	return rawLease{}, errors.New("raw index changed too frequently; retry")
}

func (a *rawAllocator) checkHeartbeat(hostID string, routes []cluster.RawPortRoute) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, route := range routes {
		lease, ok := a.index.Leases[strconv.Itoa(route.PublicPort)]
		if !ok || lease.SandboxID != route.SandboxID || lease.GuestPort != route.GuestPort {
			a.conflicts.Add(1)
			fmt.Printf("gateway: raw route conflict from host %s: public=%d sandbox=%s guest=%d\n",
				hostID, route.PublicPort, route.SandboxID, route.GuestPort)
		}
	}
}

func (a *rawAllocator) beginRemove(ctx context.Context, id string, guestPort int) (rawLease, bool, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publishLocked()
	key, lease, ok := a.findBySandboxLocked(id, guestPort)
	if !ok {
		return rawLease{}, false, nil
	}
	if lease.State == "releasing" {
		return lease, true, nil
	}
	updated := lease
	updated.State, updated.UpdatedAt = "releasing", time.Now().UTC()
	a.index.Leases[key] = updated
	if err := a.commitLocked(ctx); err != nil {
		if errors.Is(err, gcsblob.ErrPreconditionFailed) {
			_ = a.loadLocked(ctx)
		} else {
			a.index.Leases[key] = lease
		}
		return rawLease{}, false, err
	}
	return lease, true, nil
}

func (a *rawAllocator) beginRemoveSandbox(ctx context.Context, id string) ([]rawLease, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publishLocked()
	var releasing, changed []rawLease
	now := time.Now().UTC()
	for key, lease := range a.index.Leases {
		if lease.SandboxID != id {
			continue
		}
		releasing = append(releasing, lease)
		if lease.State != "releasing" {
			changed = append(changed, lease)
			updated := lease
			updated.State, updated.UpdatedAt = "releasing", now
			a.index.Leases[key] = updated
		}
	}
	if len(changed) == 0 {
		return releasing, nil
	}
	if err := a.commitLocked(ctx); err != nil {
		if errors.Is(err, gcsblob.ErrPreconditionFailed) {
			_ = a.loadLocked(ctx)
		} else {
			for _, lease := range changed {
				a.index.Leases[strconv.Itoa(lease.PublicPort)] = lease
			}
		}
		return nil, err
	}
	return releasing, nil
}

func (a *rawAllocator) finishRemove(ctx context.Context, leases []rawLease) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publishLocked()
	removed := make([]rawLease, 0, len(leases))
	for _, lease := range leases {
		key := strconv.Itoa(lease.PublicPort)
		current, ok := a.index.Leases[key]
		if ok && current.LeaseID == lease.LeaseID && current.State == "releasing" {
			removed = append(removed, current)
			delete(a.index.Leases, key)
		}
	}
	if len(removed) == 0 {
		return nil
	}
	if err := a.commitLocked(ctx); err != nil {
		if errors.Is(err, gcsblob.ErrPreconditionFailed) {
			_ = a.loadLocked(ctx)
		} else {
			for _, lease := range removed {
				a.index.Leases[strconv.Itoa(lease.PublicPort)] = lease
			}
		}
		return err
	}
	return nil
}

func (a *rawAllocator) restore(ctx context.Context, leases []rawLease) {
	a.mu.Lock()
	defer a.mu.Unlock()
	defer a.publishLocked()
	for _, lease := range leases {
		key := strconv.Itoa(lease.PublicPort)
		current, ok := a.index.Leases[key]
		if ok && current.LeaseID == lease.LeaseID && current.State == "releasing" {
			lease.UpdatedAt = time.Now().UTC()
			a.index.Leases[key] = lease
		}
	}
	if err := a.commitLocked(ctx); errors.Is(err, gcsblob.ErrPreconditionFailed) {
		_ = a.loadLocked(ctx)
	}
}

// publishLocked republishes the lock-free read view. a.mu must be held; call it
// before releasing the lock in every mutating critical section (a deferred
// publish registered AFTER `defer a.mu.Unlock()` runs first, which is correct).
// Copy-on-write is the right trade here: mutations are rare (a port exposure),
// reads are per-connection.
func (a *rawAllocator) publishLocked() {
	leases := make(map[string]rawLease, len(a.index.Leases))
	for k, v := range a.index.Leases {
		leases[k] = v
	}
	a.snap.Store(&rawSnapshot{leases: leases, gen: a.gen})
}

// emptyRawSnapshot answers reads on an allocator that has not published yet
// (i.e. before the startup load completed). It must NOT fall back to locking
// a.mu: that would reintroduce exactly the data-plane stall this snapshot
// exists to remove. "Not loaded" and "no such lease" are the same answer to a
// caller — route() reports unroutable either way.
var emptyRawSnapshot = &rawSnapshot{leases: map[string]rawLease{}}

// readSnapshot returns the current lock-free read view.
func (a *rawAllocator) readSnapshot() *rawSnapshot {
	if s := a.snap.Load(); s != nil {
		return s
	}
	return emptyRawSnapshot
}

func (a *rawAllocator) route(publicPort int) (rawLease, bool) {
	lease, ok := a.readSnapshot().leases[strconv.Itoa(publicPort)]
	return lease, ok && lease.State == "active"
}

func (a *rawAllocator) stats() (pending, active, releasing int, generation int64) {
	s := a.readSnapshot()
	generation = s.gen
	for _, lease := range s.leases {
		if lease.State == "active" {
			active++
		} else if lease.State == "releasing" {
			releasing++
		} else {
			pending++
		}
	}
	return pending, active, releasing, generation
}

// sandboxHost resolves a sandbox to its live host, adopting it on a route miss
// under pol. Callers pass the policy matching their trust level: edgeResolve for
// public ingress resolution, requestResolve for authenticated id-scoped calls.
// sandboxHost resolves a sandbox to its owning live host, adopting it when no
// live route exists. It returns the resolve outcome rather than a bool so
// callers answer 404 only for a definitive absence — see resolveOutcome and
// writeResolveFailure.
func (g *Gateway) sandboxHost(id string, pol resolvePolicy) (host, resolveOutcome) {
	g.mu.RLock()
	hid := g.route[id]
	h := g.hosts[hid]
	var snap host
	if h != nil && time.Since(h.lastSeen) <= g.ttl {
		snap = *h
	}
	g.mu.RUnlock()
	if h != nil && snap.id != "" {
		return snap, resolveAdopted
	}
	hid, outcome := g.resolveViaAdopt(id, nil, pol)
	if outcome != resolveAdopted {
		return host{}, outcome
	}
	g.mu.RLock()
	h = g.hosts[hid]
	if h != nil {
		snap = *h
	}
	g.mu.RUnlock()
	if h == nil {
		// Adopted, but the winning host aged out of the registry between the
		// adopt landing and this read. The sandbox exists — don't 404 it.
		return host{}, resolveUnknown
	}
	return snap, resolveAdopted
}

func (g *Gateway) handleAllocateRawPort(w http.ResponseWriter, r *http.Request) {
	if g.raw == nil {
		httpError(w, http.StatusNotFound, errors.New("raw ingress is disabled"))
		return
	}
	id := r.PathValue("id")
	var body struct {
		GuestPort int `json:"guest_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.GuestPort < 1 || body.GuestPort > 65535 {
		httpError(w, http.StatusBadRequest, errors.New("invalid guest_port"))
		return
	}
	h, outcome := g.sandboxHost(id, requestResolve)
	if outcome != resolveAdopted {
		writeResolveFailure(w, r, id, outcome)
		return
	}
	lease, err := g.raw.allocate(r.Context(), id, body.GuestPort, func(publicPort int) error {
		hc := client.NewHTTP(h.addr, h.token)
		_, err := hc.SetPublicPort(r.Context(), id, body.GuestPort, publicPort)
		return err
	})
	if err != nil {
		if errors.Is(err, registry.ErrPoolExhausted) {
			w.Header().Set("Retry-After", "5")
			httpError(w, http.StatusServiceUnavailable, err)
		} else if errors.Is(err, errRawReleasing) {
			httpError(w, http.StatusConflict, err)
		} else {
			httpError(w, http.StatusInternalServerError, err)
		}
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"guest_port": body.GuestPort, "mode": "raw",
		"public_host": g.raw.cfg.PublicHost, "public_port": lease.PublicPort,
	})
}

func (g *Gateway) handleRawRoute(w http.ResponseWriter, r *http.Request) {
	if g.raw == nil {
		http.NotFound(w, r)
		return
	}
	port, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	lease, ok := g.raw.route(port)
	if !ok {
		httpError(w, http.StatusNotFound, fmt.Errorf("raw port %d is not allocated", port))
		return
	}
	h, outcome := g.sandboxHost(lease.SandboxID, edgeResolve)
	if outcome != resolveAdopted {
		writeResolveFailure(w, r, lease.SandboxID, outcome)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, map[string]any{
		"sandbox_id": lease.SandboxID, "guest_port": lease.GuestPort,
		"host_addr": dialAddr(h.addr), "token": h.token, "ttl": 5,
	})
}

func (g *Gateway) handleGatewayDeletePort(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	guestPort, err := strconv.Atoi(r.PathValue("port"))
	if err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	h, outcome := g.sandboxHost(id, requestResolve)
	if outcome != resolveAdopted {
		writeResolveFailure(w, r, id, outcome)
		return
	}
	var releasing []rawLease
	if g.raw != nil {
		lease, exists, err := g.raw.beginRemove(r.Context(), id, guestPort)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
		if exists {
			releasing = append(releasing, lease)
		}
	}
	if err := client.NewHTTP(h.addr, h.token).DeletePort(r.Context(), id, guestPort); err != nil {
		if g.raw != nil {
			g.raw.restore(r.Context(), releasing)
		}
		httpError(w, http.StatusBadGateway, err)
		return
	}
	if g.raw != nil {
		if err := g.raw.finishRemove(r.Context(), releasing); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) handleGatewayDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	h, outcome := g.sandboxHost(id, requestResolve)
	switch outcome {
	case resolveAbsent:
		// Provably gone: release its public-port leases, which would otherwise
		// hold entries in the fleet-wide index forever...
		if g.raw != nil {
			leases, err := g.raw.beginRemoveSandbox(r.Context(), id)
			if err != nil {
				httpError(w, http.StatusInternalServerError, err)
				return
			}
			if err := g.raw.finishRemove(r.Context(), leases); err != nil {
				httpError(w, http.StatusInternalServerError, err)
				return
			}
		}
		// ...then report it the way a single host does. handleDestroy answers
		// 404 for a row that is not there, and the gateway fronts the same API,
		// so answering 204 made the fleet contradict a worker for the same id —
		// and contradict ITSELF, since every other id-scoped route (connect,
		// exec, files) already 404s a deleted sandbox through this same
		// resolveAbsent path. A caller cannot distinguish "I deleted it" from
		// "it was never there" under 204.
		httpError(w, http.StatusNotFound, fmt.Errorf("sandbox %s not found", id))
		return
	case resolveUnknown:
		// We could not determine whether it exists, so we must not claim to
		// have deleted it — and must not drop its leases, which would strand a
		// live sandbox's public port.
		writeResolveFailure(w, r, id, outcome)
		return
	}
	var releasing []rawLease
	if g.raw != nil {
		var err error
		releasing, err = g.raw.beginRemoveSandbox(r.Context(), id)
		if err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
	}
	if err := client.NewHTTP(h.addr, h.token).Destroy(r.Context(), id); err != nil {
		if g.raw != nil {
			g.raw.restore(r.Context(), releasing)
		}
		httpError(w, http.StatusBadGateway, err)
		return
	}
	if g.raw != nil {
		if err := g.raw.finishRemove(r.Context(), releasing); err != nil {
			httpError(w, http.StatusInternalServerError, err)
			return
		}
	}
	g.mu.Lock()
	g.unpinRouteLocked(id)
	g.mu.Unlock()
	// We just destroyed it, so its absence is proven by the strongest evidence
	// the gateway can have — its own completed delete. Record that, because the
	// only OTHER way to reach a definitive absence is an adopt probe answering
	// 404, and that probe is rate-limited: during a bulk teardown a follow-up
	// GET degrades to "not resolvable yet, retry" (503) for a sandbox this
	// gateway knows is gone. Measured on a 64-way fleet teardown, that turned
	// 10/64 delete-then-verify checks into retryable failures.
	//
	// Nothing can wrongly resurrect this: the negative is dropped only when a
	// create/restore/adopt LANDS for the id, and a destroyed id is never handed
	// out again.
	g.notFound.add(id)
	w.WriteHeader(http.StatusNoContent)
}

func (g *Gateway) rawReconcileLoop(ctx context.Context) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.reconcilePendingRaw(ctx)
		}
	}
}

func (g *Gateway) reconcilePendingRaw(ctx context.Context) {
	if g.raw == nil {
		return
	}
	g.raw.mu.Lock()
	var candidates []rawLease
	for _, lease := range g.raw.index.Leases {
		if (lease.State == "pending" || lease.State == "releasing") &&
			time.Since(lease.UpdatedAt) > 2*time.Minute {
			candidates = append(candidates, lease)
		}
	}
	g.raw.mu.Unlock()
	for _, lease := range candidates {
		h, outcome := g.sandboxHost(lease.SandboxID, requestResolve)
		if outcome != resolveAdopted {
			// Only a PROVEN absence retires a lease. An indeterminate answer
			// (throttled, no capacity, adopt still running) leaves it for the
			// next pass: retiring it here would hand the public port to another
			// sandbox while this one is merely unreachable.
			if outcome == resolveAbsent && lease.State == "releasing" {
				if err := g.raw.finishRemove(ctx, []rawLease{lease}); err != nil {
					g.raw.reconcileError.Add(1)
				} else {
					g.raw.reconcileOK.Add(1)
				}
			}
			continue
		}
		ports, err := client.NewHTTP(h.addr, h.token).ListPorts(ctx, lease.SandboxID)
		matched := false
		if err == nil {
			for _, pm := range ports {
				matched = pm.GuestPort == lease.GuestPort && pm.PublicPort == lease.PublicPort
				if matched {
					break
				}
			}
		}
		g.raw.mu.Lock()
		key := strconv.Itoa(lease.PublicPort)
		current, exists := g.raw.index.Leases[key]
		if exists && current.LeaseID == lease.LeaseID {
			if matched && lease.State == "pending" {
				current.State, current.UpdatedAt = "active", time.Now().UTC()
				g.raw.index.Leases[key] = current
			} else if matched && lease.State == "releasing" {
				current.State, current.UpdatedAt = "active", time.Now().UTC()
				g.raw.index.Leases[key] = current
			} else if err == nil {
				delete(g.raw.index.Leases, key)
			}
			if commitErr := g.raw.commitLocked(ctx); commitErr != nil {
				if errors.Is(commitErr, gcsblob.ErrPreconditionFailed) {
					_ = g.raw.loadLocked(ctx)
				}
				g.raw.reconcileError.Add(1)
			} else {
				g.raw.reconcileOK.Add(1)
			}
			g.raw.publishLocked()
		}
		g.raw.mu.Unlock()
	}
}
