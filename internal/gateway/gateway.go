// Package gateway is the Phase-1 multi-host control plane. It fronts the same
// HTTP API as a single `sandbox serve`, but fans requests out across many
// hosts: it places new sandboxes on the least-loaded host, routes every
// id-scoped request (exec, files, shell, …) to the host that owns the sandbox,
// and aggregates lists.
//
// The gateway holds no durable state. Hosts push heartbeats (see
// internal/cluster) carrying their address, capacity, and owned sandbox IDs;
// the gateway rebuilds its routing table from those, so it self-heals after a
// restart once every host has reported once.
package gateway

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/cluster"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// host is the gateway's view of one registered `sandbox serve` node.
type host struct {
	id         string
	addr       string // TCP API address the gateway dials
	token      string // bearer presented when dialing addr
	slotsTotal int
	slotsUsed  int // running sandboxes, from the last heartbeat
	// slotsFree is the host's self-reported allocatable capacity — the truth
	// to place against. It differs from slotsTotal-slotsUsed when hibernated
	// sandboxes hold ports (no slot, but a create still needs a port) or the
	// host is still warming up (advertises 0). For old host binaries whose
	// heartbeats lack the field, handleRegister falls back to total-used.
	slotsFree  int
	hibernated int // idle sandboxes frozen to disk on the host (hold no slot)
	// reserved counts creates dispatched to this host but not yet completed.
	// Without it, a burst of concurrent creates all read the same stale
	// slotsFree (heartbeats lag by seconds) and pile onto one bin-pack target
	// until its pool exhausts. Reserving at pick time makes concurrent picks
	// see each other, so they spread and cleanly 503 at capacity instead.
	reserved int
	// penaltyUntil makes the host unplaceable until this instant. Set when a
	// create on it fails with a capacity-class error (its advertised free was
	// stale — trust nothing until heartbeats correct it) or a connection
	// failure. Zero = no penalty.
	penaltyUntil time.Time
	lastSeen     time.Time
}

func (h *host) free() int {
	f := h.slotsFree - h.reserved
	if f < 0 {
		return 0
	}
	return f
}

// Gateway routes the sandbox API across a fleet of hosts.
type Gateway struct {
	token string        // bearer required on all inbound requests
	ttl   time.Duration // a host not seen within ttl is considered dead

	// queueWait/queueMax bound the create wait queue: a create that finds no
	// free slot waits up to queueWait for capacity (a destroy, a failed create,
	// or — the burst case — the autoscaler bringing a new host up) instead of
	// failing immediately. queueMax caps how many creates may wait at once;
	// beyond it, or with queueWait<=0, creates 503 right away as before.
	queueWait time.Duration
	queueMax  int
	queued    atomic.Int64 // creates currently waiting; exported as a metric
	// rejected counts creates 503'd for capacity (queue full, or no host freed
	// within queue-wait). The queue-depth gauge saturates at queueMax, so once
	// a burst overflows the queue this counter is the ONLY signal of the
	// excess demand — the autoscaler rule folds its rate back into
	// workers_desired (rejected clients retry every Retry-After, so the rate
	// approximates outstanding unqueued demand).
	rejected atomic.Int64
	// slotFreed nudges one queued create to retry placement immediately when
	// capacity may have appeared (failed create, fresh heartbeat). Buffered,
	// best-effort; the queue's poll ticker is the backstop.
	slotFreed chan struct{}

	mu        sync.RWMutex
	hosts     map[string]*host  // host id → host
	route     map[string]string // sandbox id → host id (derived from heartbeats)
	snapRoute map[string]string // snapshot id → host id (derived from heartbeats)

	// Cross-host wake (roadmap B4). When an id-scoped request finds no live
	// route (the owning host is gone), the gateway dispatches an /adopt to a
	// live host, which reconstructs the sandbox from GCS. adopts single-flights
	// concurrent misses for the same id onto one adopt; notFound briefly caches
	// a definitive 404 (no durable record) so a storm of requests for a dead id
	// doesn't fan /adopt out to every host.
	adoptMu  sync.Mutex
	adopts   map[string]*adoptInflight
	notFound sync.Map // sandbox id → time.Time (negative-cache expiry)

	// proxies caches one ReverseProxy per host id (self-invalidating on
	// addr/token change; pruned with the host). Rebuilding a proxy + three
	// closures per proxied request is pure allocation churn at high fan-out.
	proxies sync.Map // host id → *hostProxyEntry
}

// New returns a Gateway. token gates all inbound requests (clients and host
// registration alike); ttl is the stale-host cutoff. queueWait/queueMax
// configure the create wait queue (queueWait 0 disables queueing).
func New(token string, ttl time.Duration, queueWait time.Duration, queueMax int) *Gateway {
	return &Gateway{
		token:     token,
		ttl:       ttl,
		queueWait: queueWait,
		queueMax:  queueMax,
		slotFreed: make(chan struct{}, 1),
		hosts:     map[string]*host{},
		route:     map[string]string{},
		snapRoute: map[string]string{},
		adopts:    map[string]*adoptInflight{},
	}
}

// Serve listens on addr until ctx is cancelled.
func (g *Gateway) Serve(ctx context.Context, addr string) error {
	go g.pruneLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", g.handleRegister)
	mux.HandleFunc("GET /info", g.handleInfo)
	mux.HandleFunc("GET /hosts", g.handleHosts)
	mux.HandleFunc("GET /metrics", g.handleMetrics)
	mux.HandleFunc("POST /sandboxes", g.handleCreate)
	mux.HandleFunc("GET /sandboxes", g.handleList)
	// Drain moves a host's sandboxes elsewhere (release on the source, adopt on
	// a target) — maintenance, or rebalancing (roadmap B4).
	mux.HandleFunc("POST /hosts/{host}/drain", g.handleDrain)
	// Every id-scoped request (GET/DELETE /sandboxes/{id} and all
	// /sandboxes/{id}/... subpaths, including the /shell WebSocket and the
	// /exec/stream NDJSON stream) is reverse-proxied to the owning host.
	mux.HandleFunc("/sandboxes/{id}", g.handleProxyByID)
	mux.HandleFunc("/sandboxes/{id}/{rest...}", g.handleProxyByID)
	// Snapshot operations route to the host holding the snapshot; when that
	// host is gone, any live host can serve them by pulling from GCS.
	mux.HandleFunc("GET /snapshots", g.handleListSnapshots)
	mux.HandleFunc("POST /snapshots/{id}/restore", g.handleSnapshotOp)
	mux.HandleFunc("POST /snapshots/{id}/fanout", g.handleSnapshotOp)
	mux.HandleFunc("POST /snapshots/{id}/rename", g.handleSnapshotOp)
	mux.HandleFunc("DELETE /snapshots/{id}", g.handleSnapshotOp)

	srv := &http.Server{Addr: addr, Handler: bearerAuth(g.token, mux)}
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	fmt.Fprintf(os.Stderr, "gateway listening on %s (bearer auth)\n", addr)

	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shCtx)
	case err := <-errc:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// --- host registration ---

func (g *Gateway) handleRegister(w http.ResponseWriter, r *http.Request) {
	var hb cluster.Heartbeat
	if err := json.NewDecoder(r.Body).Decode(&hb); err != nil {
		httpError(w, 400, fmt.Errorf("decode heartbeat: %w", err))
		return
	}
	if hb.HostID == "" || hb.Addr == "" {
		httpError(w, 400, errors.New("heartbeat missing host_id or addr"))
		return
	}

	g.mu.Lock()
	h := g.hosts[hb.HostID]
	if h == nil {
		h = &host{id: hb.HostID}
		g.hosts[hb.HostID] = h
		fmt.Fprintf(os.Stderr, "gateway: host %s registered (%s)\n", hb.HostID, hb.Addr)
	}
	h.addr = hb.Addr
	h.token = hb.Token
	h.slotsTotal = hb.SlotsTotal
	h.slotsUsed = hb.SlotsUsed
	h.slotsFree = hb.SlotsTotal - hb.SlotsUsed // old host binary: best guess
	if hb.SlotsFree != nil {
		h.slotsFree = *hb.SlotsFree
	}
	h.hibernated = hb.Hibernated
	h.lastSeen = time.Now()
	// Rebuild this host's routes: drop stale entries, add current ones.
	for sid, hid := range g.route {
		if hid == hb.HostID {
			delete(g.route, sid)
		}
	}
	for _, sid := range hb.SandboxIDs {
		g.route[sid] = hb.HostID
	}
	for sid, hid := range g.snapRoute {
		if hid == hb.HostID {
			delete(g.snapRoute, sid)
		}
	}
	for _, sid := range hb.SnapshotIDs {
		g.snapRoute[sid] = hb.HostID
	}
	g.mu.Unlock()
	// A heartbeat can bring capacity (new host, corrected free count) — let a
	// queued create retry now rather than on its next poll tick.
	g.notifySlotFreed()

	w.WriteHeader(http.StatusNoContent)
}

// --- placement & create ---

func (g *Gateway) handleCreate(w http.ResponseWriter, r *http.Request) {
	// Optional {timeout_sec} body, forwarded to the chosen host.
	var body client.CreateOpts
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}

	// One shared queue deadline across all attempts: a create that fails over
	// twice must not wait 3× queueWait.
	deadline := time.Now().Add(g.queueWait)
	tried := map[string]bool{}
	var lastErr error
	for attempt := 0; attempt < maxCreateAttempts; attempt++ {
		h := g.reserveHost(tried)
		if h == nil {
			// No free slot right now — wait for one instead of failing. During a
			// burst the queue depth itself feeds the autoscaler's scaling signal
			// (sandbox_create_queue_depth), so waiting here is what gives the new
			// host time to boot and absorb the queue.
			h = g.awaitHost(r.Context(), deadline, tried)
		}
		if h == nil {
			break
		}

		sb, err := client.NewHTTP(h.addr, h.token).Create(r.Context(), body)
		if err == nil {
			// Landed: convert the reservation into a used slot and record the
			// route. The next heartbeat overwrites the host's counts (which now
			// include this sandbox), so the adjustment just bridges the gap.
			g.mu.Lock()
			g.route[sb.ID] = h.id
			g.mu.Unlock()
			g.release(h.id, true)
			sb.HostAddr = hostOnly(h.addr)
			writeJSON(w, http.StatusCreated, sb)
			return
		}

		g.release(h.id, false) // create failed: free the reservation
		if r.Context().Err() != nil {
			// The CLIENT went away mid-create; the error is our own context
			// cancellation, not the host's fault. Penalizing here would let a
			// wave of client timeouts blackout placement on healthy hosts.
			httpError(w, 499, fmt.Errorf("client disconnected during create on host %s: %w", h.id, err))
			return
		}
		lastErr = fmt.Errorf("create on host %s: %w", h.id, err)
		var ae *client.APIError
		switch {
		case errors.As(err, &ae) && (ae.StatusCode == http.StatusServiceUnavailable || ae.StatusCode == http.StatusTooManyRequests):
			// Capacity pushback: the host's advertised free was stale (e.g. a
			// wake or expose consumed the last port since its heartbeat). Stop
			// feeding it until heartbeats restore the truth; try elsewhere.
			g.penalize(h.id, capacityPenalty, true)
		case !errors.As(err, &ae):
			// Transport-level failure — host possibly down or unreachable.
			g.penalize(h.id, connPenalty, false)
		default:
			// A real host-side failure: not a capacity signal — don't burn
			// boots on other hosts, surface it. A client error (4xx) keeps its
			// status so e.g. an unfittable mem_mib override reaches the client
			// as the 400 the host intended, not a retryable-looking 502.
			code := http.StatusBadGateway
			if ae.StatusCode >= 400 && ae.StatusCode < 500 {
				code = ae.StatusCode
			}
			httpError(w, code, lastErr)
			return
		}
		tried[h.id] = true
	}

	w.Header().Set("Retry-After", "5")
	g.rejected.Add(1)
	if lastErr != nil {
		httpError(w, http.StatusServiceUnavailable, fmt.Errorf("no host with free capacity (last error: %w)", lastErr))
		return
	}
	httpError(w, http.StatusServiceUnavailable, errors.New("no host with free capacity"))
}

// maxCreateAttempts bounds how many hosts one create may be tried on before
// giving up with 503. Failover only happens on capacity-class (503/429) or
// connection errors — a genuine host-side failure returns 502 immediately.
const maxCreateAttempts = 3

// Penalty windows applied to a host after a failed create. Capacity penalties
// last ~2 heartbeats — long enough for the host's own accounting to correct
// the stale free-slot count that misled placement. Connection penalties are a
// bit longer; the host may be mid-crash and its row only clears at TTL.
const (
	capacityPenalty = 10 * time.Second
	connPenalty     = 15 * time.Second
)

// penalize makes a host unplaceable for d. zeroFree also clears its advertised
// free capacity (used after capacity pushback — the count was provably stale);
// the next heartbeat restores the host's own truth.
func (g *Gateway) penalize(hostID string, d time.Duration, zeroFree bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.hosts[hostID]
	if h == nil {
		return
	}
	h.penaltyUntil = time.Now().Add(d)
	if zeroFree {
		h.slotsFree = 0
	}
}

// reserveHost bin-packs (fullest host with free capacity, id tie-break) AND
// reserves a slot on the chosen host atomically under the write lock, so
// concurrent creates during a burst see the reservation. Hosts in exclude
// (already tried by this create) or under a penalty window are skipped.
// Returns a snapshot copy, or nil if no host has capacity. The caller MUST
// release() exactly once.
func (g *Gateway) reserveHost(exclude map[string]bool) *host {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	var best *host
	for _, h := range g.hosts {
		if exclude[h.id] || now.Before(h.penaltyUntil) {
			continue
		}
		if now.Sub(h.lastSeen) > g.ttl || h.free() <= 0 {
			continue
		}
		if best == nil || h.free() < best.free() || (h.free() == best.free() && h.id < best.id) {
			best = h
		}
	}
	if best == nil {
		return nil
	}
	best.reserved++
	snap := *best
	return &snap
}

// release ends a create's reservation. landed=true means the sandbox came up,
// so the slot moves from reserved to used (and debits the advertised free
// count until the host's next heartbeat reports its own numbers);
// landed=false (create failed) just frees the reservation.
func (g *Gateway) release(hostID string, landed bool) {
	g.mu.Lock()
	h := g.hosts[hostID]
	if h == nil {
		g.mu.Unlock()
		return
	}
	if h.reserved > 0 {
		h.reserved--
	}
	if landed {
		h.slotsUsed++
		if h.slotsFree > 0 {
			h.slotsFree--
		}
	}
	g.mu.Unlock()
	if !landed {
		// A freed reservation is capacity: nudge one queued create to retry
		// now instead of waiting out its poll tick.
		g.notifySlotFreed()
	}
}

// notifySlotFreed wakes at most one awaitHost waiter without blocking. The
// 250ms poll remains the backstop; this just shaves latency when capacity
// appears (failed create, fresh heartbeat).
func (g *Gateway) notifySlotFreed() {
	select {
	case g.slotFreed <- struct{}{}:
	default:
	}
}

// queuePollInterval is how often a queued create re-tries placement. Capacity
// appears via heartbeats (5 s cadence) or releases, so sub-second polling is
// plenty; the cost is one map scan under the lock per waiter per tick.
const queuePollInterval = 250 * time.Millisecond

// awaitHost holds a create in the bounded wait queue, re-trying reserveHost
// until a slot frees up or the deadline (shared across a create's failover
// attempts) passes. Returns a reserved host snapshot (caller MUST release()
// exactly once), or nil when queueing is disabled, the queue is full, the
// wait times out, or the client goes away.
func (g *Gateway) awaitHost(ctx context.Context, deadline time.Time, exclude map[string]bool) *host {
	if g.queueWait <= 0 || g.queueMax <= 0 {
		return nil
	}
	wait := time.Until(deadline)
	if wait <= 0 {
		return nil
	}
	if g.queued.Add(1) > int64(g.queueMax) {
		g.queued.Add(-1)
		return nil
	}
	defer g.queued.Add(-1)

	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	tick := time.NewTicker(queuePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-timeout.C:
			return nil
		case <-g.slotFreed:
			if h := g.reserveHost(exclude); h != nil {
				return h
			}
		case <-tick.C:
			if h := g.reserveHost(exclude); h != nil {
				return h
			}
		}
	}
}

// hostOnly strips the port from an addr, so clients can pair it with a
// sandbox's forwarded ports (which live on the host, not the gateway).
func hostOnly(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// pickHost returns a snapshot of the live host to place a new sandbox on, or
// nil if none has free capacity. It BIN-PACKS: among hosts with free slots it
// picks the fullest (fewest free), tie-broken by smaller host id for
// determinism. Packing onto the fullest host lets other hosts drain to empty,
// which is what makes autoscaler scale-in safe — an empty host can be removed
// without evicting running sandboxes. (This is the deliberate inverse of a
// spread/least-loaded policy, which would keep every host partially full and
// never releasable.)
func (g *Gateway) pickHost() *host {
	g.mu.RLock()
	defer g.mu.RUnlock()
	now := time.Now()
	var best *host
	for _, h := range g.hosts {
		if now.Before(h.penaltyUntil) {
			continue
		}
		if now.Sub(h.lastSeen) > g.ttl || h.free() <= 0 {
			continue
		}
		if best == nil || h.free() < best.free() || (h.free() == best.free() && h.id < best.id) {
			best = h
		}
	}
	if best == nil {
		return nil
	}
	snap := *best
	return &snap
}

// --- list (scatter-gather) ---

func (g *Gateway) handleList(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	var live []host
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) <= g.ttl {
			live = append(live, *h)
		}
	}
	g.mu.RUnlock()

	// Fan out concurrently: a sequential sweep makes list latency grow linearly
	// with fleet size. A per-host timeout keeps one wedged host from stalling
	// the whole response.
	var (
		mu  sync.Mutex
		wg  sync.WaitGroup
		out = []registry.Sandbox{}
	)
	for _, h := range live {
		wg.Add(1)
		go func(h host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			sandboxes, err := client.NewHTTP(h.addr, h.token).List(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gateway: list from host %s: %v\n", h.id, err)
				return
			}
			for i := range sandboxes {
				sandboxes[i].HostAddr = hostOnly(h.addr)
			}
			mu.Lock()
			out = append(out, sandboxes...)
			mu.Unlock()
		}(h)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, 200, out)
}

// --- id-scoped reverse proxy ---

// hostProxyEntry caches one ReverseProxy per host so the hot proxy path stops
// allocating a proxy + closures per request. Entries self-invalidate: a lookup
// whose addr/token no longer match rebuilds the proxy.
type hostProxyEntry struct {
	addr, token string
	proxy       *httputil.ReverseProxy
}

// hostProxy returns the cached reverse proxy for a host, (re)building it if
// the host's addr or token changed since it was cached.
func (g *Gateway) hostProxy(hostID, addr, token string) *httputil.ReverseProxy {
	if v, ok := g.proxies.Load(hostID); ok {
		if e := v.(*hostProxyEntry); e.addr == addr && e.token == token {
			return e.proxy
		}
	}
	e := &hostProxyEntry{addr: addr, token: token, proxy: g.buildHostProxy(hostID, addr, token)}
	g.proxies.Store(hostID, e)
	return e.proxy
}

func (g *Gateway) buildHostProxy(hostID, addr, token string) *httputil.ReverseProxy {
	target := &url.URL{Scheme: "http", Host: addr}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = client.SharedTransport()
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req) // sets scheme+host; preserves the /sandboxes/{id}/... path
		req.Host = target.Host
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		} else {
			req.Header.Del("Authorization") // don't leak the gateway token
		}
		// The gateway already re-authenticated with the host's token; drop a
		// browser client's access_token so it doesn't ride further.
		if q := req.URL.Query(); q.Has("access_token") {
			q.Del("access_token")
			req.URL.RawQuery = q.Encode()
		}
	}
	proxy.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		err = fmt.Errorf("host %s unreachable: %w", hostID, err)
		if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseBadGateway, err.Error()) == nil {
			return
		}
		httpError(w, http.StatusBadGateway, err)
	}
	// One ModifyResponse dispatching on the outbound request (the proxy is
	// shared across requests, so per-request assignment is no longer possible):
	//  - POST .../snapshot: record a freshly created snapshot's location
	//    immediately — its id only reaches heartbeats after up to one interval,
	//    and a restore issued in that window would otherwise fall back to the
	//    wrong host.
	//  - plain GET /sandboxes/{id} (the SDK connect path): annotate the
	//    response with the owning host's address, like create/list do.
	// Everything else — exec streams, file bytes, WebSockets — passes through
	// untouched.
	proxy.ModifyResponse = func(resp *http.Response) error {
		req := resp.Request
		if req == nil {
			return nil
		}
		switch {
		case req.Method == http.MethodPost && strings.HasSuffix(req.URL.Path, "/snapshot"):
			if resp.StatusCode != http.StatusCreated {
				return nil
			}
			var sn registry.Snapshot
			if err := json.NewDecoder(resp.Body).Decode(&sn); err != nil {
				return err
			}
			resp.Body.Close()
			if sn.ID != "" {
				g.mu.Lock()
				g.snapRoute[sn.ID] = hostID
				g.mu.Unlock()
			}
			return replaceJSONBody(resp, sn)
		case req.Method == http.MethodGet && isPlainSandboxGet(req.URL.Path):
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			var sb registry.Sandbox
			if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
				return err
			}
			resp.Body.Close()
			sb.HostAddr = hostOnly(addr)
			return replaceJSONBody(resp, sb)
		}
		return nil
	}
	return proxy
}

// isPlainSandboxGet reports whether path is exactly /sandboxes/{id} — no
// trailing sub-resource segment.
func isPlainSandboxGet(path string) bool {
	rest, ok := strings.CutPrefix(path, "/sandboxes/")
	return ok && rest != "" && !strings.Contains(rest, "/")
}

// replaceJSONBody swaps resp's (already-consumed) body for the JSON encoding
// of v, fixing up Content-Length.
func replaceJSONBody(resp *http.Response, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(b))
	resp.ContentLength = int64(len(b))
	resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
	return nil
}

func (g *Gateway) handleProxyByID(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	g.mu.RLock()
	hid := g.route[id]
	h := g.hosts[hid]
	var snap host
	if h != nil {
		snap = *h
	}
	g.mu.RUnlock()

	if h == nil {
		// No live route: the owning host may be gone. Try to adopt the sandbox
		// onto a live host from its durable GCS record (roadmap B4). Adopt can
		// take seconds (reconstruct + wake), so this blocks the request like a
		// wake-on-connect; concurrent misses for the same id single-flight.
		if hid, ok := g.resolveViaAdopt(id, nil); ok {
			g.mu.RLock()
			if ah := g.hosts[hid]; ah != nil {
				snap = *ah
				h = ah
			}
			g.mu.RUnlock()
		}
	}
	if h == nil {
		err := fmt.Errorf("sandbox %s not found on any host", id)
		if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseNotFound, err.Error()) == nil {
			return
		}
		httpError(w, 404, err)
		return
	}

	g.hostProxy(snap.id, snap.addr, snap.token).ServeHTTP(w, r)
}

// --- host info ---

// handleInfo forwards GET /info to a live host. A fleet's hosts share one
// template config, so any host's defaults and limits speak for the fleet;
// the lowest-id live host is picked for determinism.
func (g *Gateway) handleInfo(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	var pick *host
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) > g.ttl {
			continue
		}
		if pick == nil || h.id < pick.id {
			pick = h
		}
	}
	var snap host
	if pick != nil {
		snap = *pick
	}
	g.mu.RUnlock()
	if pick == nil {
		httpError(w, http.StatusServiceUnavailable, errors.New("no live host to serve /info"))
		return
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, "http://"+snap.addr+"/info", nil)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	if snap.token != "" {
		req.Header.Set("Authorization", "Bearer "+snap.token)
	}
	resp, err := (&http.Client{Transport: client.SharedTransport()}).Do(req)
	if err != nil {
		httpError(w, http.StatusBadGateway, fmt.Errorf("host %s unreachable: %w", snap.id, err))
		return
	}
	defer resp.Body.Close()
	w.Header().Set("Content-Type", resp.Header.Get("Content-Type"))
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// --- debug ---

func (g *Gateway) handleHosts(w http.ResponseWriter, r *http.Request) {
	type hostView struct {
		ID         string `json:"id"`
		Addr       string `json:"addr"`
		SlotsTotal int    `json:"slots_total"`
		SlotsUsed  int    `json:"slots_used"`
		Hibernated int    `json:"hibernated"`
		Free       int    `json:"free"`
		Alive      bool   `json:"alive"`
		LastSeenMS int64  `json:"last_seen_ms_ago"`
	}
	g.mu.RLock()
	views := []hostView{}
	for _, h := range g.hosts {
		views = append(views, hostView{
			ID: h.id, Addr: h.addr, SlotsTotal: h.slotsTotal, SlotsUsed: h.slotsUsed,
			Hibernated: h.hibernated, Free: h.free(), Alive: time.Since(h.lastSeen) <= g.ttl,
			LastSeenMS: time.Since(h.lastSeen).Milliseconds(),
		})
	}
	g.mu.RUnlock()
	sort.Slice(views, func(i, j int) bool { return views[i].ID < views[j].ID })
	writeJSON(w, 200, views)
}

// --- stale-host pruning ---

func (g *Gateway) pruneLoop(ctx context.Context) {
	ticker := time.NewTicker(g.ttl / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			g.mu.Lock()
			for id, h := range g.hosts {
				if time.Since(h.lastSeen) > g.ttl {
					fmt.Fprintf(os.Stderr, "gateway: host %s timed out, dropping\n", id)
					delete(g.hosts, id)
					g.proxies.Delete(id)
					for sid, hid := range g.route {
						if hid == id {
							delete(g.route, sid)
						}
					}
					for sid, hid := range g.snapRoute {
						if hid == id {
							delete(g.snapRoute, sid)
						}
					}
				}
			}
			g.mu.Unlock()
		}
	}
}

// --- helpers (mirrors internal/server) ---

// bearerAuth mirrors internal/server: WebSocket upgrades may carry the token
// as ?access_token= (browsers can't set headers on a WebSocket), and their
// rejections are delivered as post-handshake close frames (4401) so the page
// sees the reason instead of an opaque 1006.
func bearerAuth(token string, next http.Handler) http.Handler {
	want := sha256.Sum256([]byte("Bearer " + token))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if auth == "" && wsutil.IsUpgrade(r) {
			if t := r.URL.Query().Get("access_token"); t != "" {
				auth = "Bearer " + t
			}
		}
		got := sha256.Sum256([]byte(auth))
		if subtle.ConstantTimeCompare(want[:], got[:]) != 1 {
			err := errors.New("missing or invalid bearer token")
			if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseUnauthorized, err.Error()) == nil {
				return
			}
			httpError(w, http.StatusUnauthorized, err)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}
