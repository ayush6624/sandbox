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
	hibernated int // idle sandboxes frozen to disk on the host (hold no slot)
	// reserved counts creates dispatched to this host but not yet completed.
	// Without it, a burst of concurrent creates all read the same stale
	// slotsUsed (heartbeats lag by seconds) and pile onto one bin-pack target
	// until its pool exhausts. Reserving at pick time makes concurrent picks
	// see each other, so they spread and cleanly 503 at capacity instead.
	reserved int
	lastSeen time.Time
}

func (h *host) free() int {
	used := h.slotsUsed + h.reserved
	if h.slotsTotal <= used {
		return 0
	}
	return h.slotsTotal - used
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

	mu        sync.RWMutex
	hosts     map[string]*host  // host id → host
	route     map[string]string // sandbox id → host id (derived from heartbeats)
	snapRoute map[string]string // snapshot id → host id (derived from heartbeats)
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
		hosts:     map[string]*host{},
		route:     map[string]string{},
		snapRoute: map[string]string{},
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

	h := g.reserveHost()
	if h == nil {
		// No free slot right now — wait for one instead of failing. During a
		// burst the queue depth itself feeds the autoscaler's scaling signal
		// (sandbox_create_queue_depth), so waiting here is what gives the new
		// host time to boot and absorb the queue.
		h = g.awaitHost(r.Context())
	}
	if h == nil {
		w.Header().Set("Retry-After", "5")
		httpError(w, http.StatusServiceUnavailable, errors.New("no host with free capacity"))
		return
	}

	c := client.NewHTTP(h.addr, h.token)
	sb, err := c.Create(r.Context(), body)
	if err != nil {
		g.release(h.id, false) // create failed: free the reservation, host's pool untouched
		httpError(w, http.StatusBadGateway, fmt.Errorf("create on host %s: %w", h.id, err))
		return
	}

	// Landed: convert the reservation into a used slot and record the route.
	// The next heartbeat overwrites slotsUsed with the host's own count (which
	// now includes this sandbox), so the +1 just bridges the heartbeat gap.
	g.mu.Lock()
	g.route[sb.ID] = h.id
	g.mu.Unlock()
	g.release(h.id, true)

	sb.HostAddr = hostOnly(h.addr)
	writeJSON(w, http.StatusCreated, sb)
}

// reserveHost bin-packs (fullest host with free capacity, id tie-break) AND
// reserves a slot on the chosen host atomically under the write lock, so
// concurrent creates during a burst see the reservation. Returns a snapshot
// copy, or nil if no host has capacity. The caller MUST release() exactly once.
func (g *Gateway) reserveHost() *host {
	g.mu.Lock()
	defer g.mu.Unlock()
	var best *host
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) > g.ttl || h.free() <= 0 {
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
// so the slot moves from reserved to slotsUsed; landed=false (create failed)
// just frees the reservation.
func (g *Gateway) release(hostID string, landed bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	h := g.hosts[hostID]
	if h == nil {
		return
	}
	if h.reserved > 0 {
		h.reserved--
	}
	if landed {
		h.slotsUsed++
	}
}

// queuePollInterval is how often a queued create re-tries placement. Capacity
// appears via heartbeats (5 s cadence) or releases, so sub-second polling is
// plenty; the cost is one map scan under the lock per waiter per tick.
const queuePollInterval = 250 * time.Millisecond

// awaitHost holds a create in the bounded wait queue, re-trying reserveHost
// until a slot frees up or the deadline passes. Returns a reserved host
// snapshot (caller MUST release() exactly once), or nil when queueing is
// disabled, the queue is full, the wait times out, or the client goes away.
func (g *Gateway) awaitHost(ctx context.Context) *host {
	if g.queueWait <= 0 || g.queueMax <= 0 {
		return nil
	}
	if g.queued.Add(1) > int64(g.queueMax) {
		g.queued.Add(-1)
		return nil
	}
	defer g.queued.Add(-1)

	deadline := time.NewTimer(g.queueWait)
	defer deadline.Stop()
	tick := time.NewTicker(queuePollInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-deadline.C:
			return nil
		case <-tick.C:
			if h := g.reserveHost(); h != nil {
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
	var best *host
	for _, h := range g.hosts {
		if time.Since(h.lastSeen) > g.ttl || h.free() <= 0 {
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

	out := []registry.Sandbox{}
	for _, h := range live {
		sandboxes, err := client.NewHTTP(h.addr, h.token).List(r.Context())
		if err != nil {
			fmt.Fprintf(os.Stderr, "gateway: list from host %s: %v\n", h.id, err)
			continue
		}
		for i := range sandboxes {
			sandboxes[i].HostAddr = hostOnly(h.addr)
		}
		out = append(out, sandboxes...)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	writeJSON(w, 200, out)
}

// --- id-scoped reverse proxy ---

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
		err := fmt.Errorf("sandbox %s not found on any host", id)
		if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseNotFound, err.Error()) == nil {
			return
		}
		httpError(w, 404, err)
		return
	}

	target := &url.URL{Scheme: "http", Host: snap.addr}
	proxy := httputil.NewSingleHostReverseProxy(target)
	director := proxy.Director
	proxy.Director = func(req *http.Request) {
		director(req) // sets scheme+host; preserves the /sandboxes/{id}/... path
		req.Host = target.Host
		if snap.token != "" {
			req.Header.Set("Authorization", "Bearer "+snap.token)
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
		err = fmt.Errorf("host %s unreachable: %w", snap.id, err)
		if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseBadGateway, err.Error()) == nil {
			return
		}
		httpError(w, http.StatusBadGateway, err)
	}
	// Record a freshly created snapshot's location immediately — its id only
	// reaches heartbeats after up to one interval, and a restore issued in
	// that window would otherwise fall back to the wrong host.
	if r.Method == http.MethodPost && r.PathValue("rest") == "snapshot" {
		proxy.ModifyResponse = func(resp *http.Response) error {
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
				g.snapRoute[sn.ID] = snap.id
				g.mu.Unlock()
			}
			b, err := json.Marshal(sn)
			if err != nil {
				return err
			}
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		}
	}
	// Annotate plain GET /sandboxes/{id} responses (the SDK connect path) with
	// the owning host's address, like create/list do. Everything else —
	// exec streams, file bytes, WebSockets — passes through untouched.
	if r.Method == http.MethodGet && r.PathValue("rest") == "" {
		proxy.ModifyResponse = func(resp *http.Response) error {
			if resp.StatusCode != http.StatusOK {
				return nil
			}
			var sb registry.Sandbox
			if err := json.NewDecoder(resp.Body).Decode(&sb); err != nil {
				return err
			}
			resp.Body.Close()
			sb.HostAddr = hostOnly(snap.addr)
			b, err := json.Marshal(sb)
			if err != nil {
				return err
			}
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Length", strconv.Itoa(len(b)))
			return nil
		}
	}
	proxy.ServeHTTP(w, r)
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
	resp, err := http.DefaultClient.Do(req)
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
