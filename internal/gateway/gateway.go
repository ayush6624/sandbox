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
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ayush6624/sandbox/internal/apiv1"
	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/cluster"
	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/management"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// host is the gateway's view of one registered `sandbox serve` node.
type host struct {
	id         string
	addr       string // TCP API address the gateway dials
	token      string // bearer presented when dialing addr
	release    string // worker artifact generation reported by the host
	slotsTotal int
	// slotsUsed starts with the worker heartbeat count and is optimistically
	// advanced as create responses land. It is capped at slotsTotal because a
	// heartbeat can already include a registry-committed create whose response
	// is still in flight.
	slotsUsed int
	// slotsFree is the host's self-reported allocatable capacity — the truth
	// to place against. It differs from slotsTotal-slotsUsed when memory
	// admission is binding or the host is still warming up (advertises 0).
	// For old host binaries whose
	// heartbeats lack the field, handleRegister falls back to total-used.
	slotsFree  int
	warmReady  int // worker-reported fully initialized VMs ready for atomic claim
	hibernated int // idle sandboxes frozen to disk on the host (hold no slot)
	// reserved counts creates dispatched to this host but not yet completed.
	// Without it, a burst of concurrent creates all read the same stale
	// slotsFree (heartbeats lag by seconds) and pile onto one bin-pack target
	// until its pool exhausts. Reserving at pick time makes concurrent picks
	// see each other, so they spread and cleanly 503 at capacity instead.
	reserved int
	// warmReserved is the subset of reservations selected against warmReady.
	// Keeping it separate lets concurrent placement spread across ready pools
	// before falling back to ordinary clone capacity.
	warmReserved int
	// reservationWarm exists only on the snapshot returned by reserveHost and
	// tells completion which counter to release.
	reservationWarm bool
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

func (h *host) warmFree() int {
	f := h.warmReady - h.warmReserved
	if f < 0 {
		return 0
	}
	return f
}

// committed is running occupancy plus create reservations assigned to this
// host but not yet completed. A heartbeat can observe a create before its HTTP
// response arrives, so clamp that brief double-count to physical capacity.
func (h *host) committed() int {
	n := h.slotsUsed + h.reserved
	if n < 0 {
		return 0
	}
	if n > h.slotsTotal {
		return h.slotsTotal
	}
	return n
}

// Gateway routes the sandbox API across a fleet of hosts.
type Gateway struct {
	token             string // retained for compatibility with in-package tests
	clientCredentials *management.Credentials
	workerCredentials *management.Credentials
	transport         management.Transport
	ttl               time.Duration // a host not seen within ttl is considered dead

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
	// createsOK counts sandboxes the gateway successfully brought up (each
	// release(landed=true)). Exported as sandbox_creates_total — a gateway-side
	// aggregate on the gateway's own /metrics (scraped every 10s), so the
	// autoscaler's lead term reads create RATE at 10s resolution instead of the
	// 30s-federated per-host sandbox_creates_ok_total.
	createsOK atomic.Int64
	// slotFreed is replaced and the old channel closed whenever capacity may
	// have appeared. Closing broadcasts to every queued create: a fresh worker
	// can expose dozens of slots in one heartbeat, so waking only one waiter
	// needlessly leaves the rest on the polling backstop.
	slotFreedMu sync.Mutex
	slotFreed   chan struct{}

	// directScaler is the scale-out path for queue pressure, and where it is
	// configured it is the SOLE writer that grows the group: the Nomad
	// autoscaler is left to scale IN only, because two independent writers can
	// ratchet the target far above demand.
	//
	// It is LEVEL-triggered, not edge-triggered. Every event that can change
	// demand (a create entering the queue, a host heartbeat) requests a
	// re-evaluation; evaluations coalesce through directDirty/directScalePending
	// so a 160-create burst still costs one debounce and one resize. An earlier
	// version fired only on the queue's 0 -> 1 edge, so demand that grew while
	// the queue stayed non-empty had to wait for the Prometheus/Nomad loop —
	// worth ~10 s of p95 on the canonical held burst, and up to ~189 s when it
	// landed inside the autoscaler's scale-out blackout.
	//
	// directRequested is a grow-only watermark of the largest target already
	// requested, so repeated evaluations during one burst do not re-issue
	// shrinking or duplicate resizes. It re-baselines to the live host count
	// once the queue empties; otherwise autoscaler scale-in would leave it
	// pinned high and permanently suppress the next burst's scale-out.
	directScaler       DirectScaler
	directSlotsPerHost int
	directHeadroom     int
	directScalePending atomic.Bool
	directDirty        atomic.Bool
	directRequested    atomic.Int64
	directScaleStarted atomic.Int64
	directScaleFailed  atomic.Int64

	// migTarget is the provider's own target worker count, polled when the
	// scaler implements TargetSizer. Exported as sandbox_mig_target_size so the
	// autoscaler's scale-in ceiling can be exact; migTargetKnown stays false
	// until a poll succeeds, so a provider error publishes no series rather than
	// a misleading zero.
	migTarget      atomic.Int64
	migTargetKnown atomic.Bool

	// expectedRelease gates placement during worker rollouts. A suspended VM
	// can resume an old serve process and heartbeat before Nomad replaces its
	// stale allocation. Keep its routes, but force free capacity to zero until
	// the expected release reports. The value is persisted across gateway
	// restarts and updated by deploy-job.sh before it submits the new job.
	expectedRelease string
	releaseFile     string
	releaseUpdateMu sync.Mutex

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

// DirectScaler is implemented by the queue-triggered infrastructure fast path.
// Implementations must treat desired as a grow-only request.
type DirectScaler interface {
	ScaleOut(context.Context, int) error
}

// TargetSizer is an optional DirectScaler capability: the provider's current
// target worker count. When available the gateway exports it as
// sandbox_mig_target_size, which is what the autoscaler's scale-in ceiling is
// built from. A heartbeat-derived count is NOT a substitute — it also counts
// resumed standby workers that sit outside the target, which is what let the
// autoscaler scale out (from=5 to=6) past its cap on 2026-07-28.
type TargetSizer interface {
	TargetSize(context.Context) (int, error)
}

// New returns a Gateway. token gates all inbound requests (clients and host
// registration alike); ttl is the stale-host cutoff. queueWait/queueMax
// configure the create wait queue (queueWait 0 disables queueing).
func New(token string, ttl time.Duration, queueWait time.Duration, queueMax int) *Gateway {
	creds, _ := management.NewCredentials([]string{token}, "")
	return &Gateway{
		token:             token,
		clientCredentials: creds,
		workerCredentials: creds,
		transport:         management.Transport{Mode: management.TransportDevelopment},
		ttl:               ttl,
		queueWait:         queueWait,
		queueMax:          queueMax,
		slotFreed:         make(chan struct{}),
		hosts:             map[string]*host{},
		route:             map[string]string{},
		snapRoute:         map[string]string{},
		adopts:            map[string]*adoptInflight{},
	}
}

// ConfigureSecurity separates public-client and worker-control credentials and
// makes the listener transport explicit. Credential files support overlap
// rotation without restarting the gateway.
func (g *Gateway) ConfigureSecurity(clientTokens []string, clientFile string, workerTokens []string, workerFile string, transport management.Transport) error {
	clientCreds, err := management.NewCredentials(clientTokens, clientFile)
	if err != nil {
		return fmt.Errorf("gateway client credentials: %w", err)
	}
	workerCreds, err := management.NewCredentials(workerTokens, workerFile)
	if err != nil {
		return fmt.Errorf("gateway worker-control credentials: %w", err)
	}
	if transport.Mode != management.TransportDevelopment &&
		clientCreds.Overlaps(workerCreds) {
		return errors.New("gateway client and worker-control credentials must differ outside development mode")
	}
	g.clientCredentials = clientCreds
	g.workerCredentials = workerCreds
	g.transport = transport
	return nil
}

// ConfigureDirectScaleOut enables a queue 0 -> 1 scale-out trigger. The direct
// path is deliberately optional so non-GCE and local gateways are unchanged.
func (g *Gateway) ConfigureDirectScaleOut(s DirectScaler, slotsPerHost, headroom int) error {
	if s == nil {
		return errors.New("direct scale-out scaler is nil")
	}
	if slotsPerHost <= 0 {
		return errors.New("direct scale-out slots per host must be positive")
	}
	if headroom < 0 {
		return errors.New("direct scale-out headroom cannot be negative")
	}
	g.directScaler = s
	g.directSlotsPerHost = slotsPerHost
	g.directHeadroom = headroom
	return nil
}

// ConfigureWorkerReleaseFile enables the persisted worker-release placement
// gate. A missing file is valid on first deploy; PUT /worker-release creates it.
func (g *Gateway) ConfigureWorkerReleaseFile(path string) error {
	if path == "" {
		return errors.New("worker release file path is empty")
	}
	b, err := os.ReadFile(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read worker release file: %w", err)
	}
	release := strings.TrimSpace(string(b))
	if err == nil {
		if err := validateWorkerRelease(release); err != nil {
			return fmt.Errorf("worker release file: %w", err)
		}
	}
	g.releaseFile = path
	g.expectedRelease = release
	return nil
}

// Serve listens on addr until ctx is cancelled.
func (g *Gateway) Serve(ctx context.Context, addr string) error {
	if err := g.transport.ValidateListener(addr); err != nil {
		return err
	}
	go g.pruneLoop(ctx)
	go g.migTargetLoop(ctx)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /register", g.handleRegister)
	mux.HandleFunc("POST /internal/v1/hosts:register", g.handleRegister)
	mux.HandleFunc("GET /info", g.handleInfo)
	mux.HandleFunc("GET /hosts", g.handleHosts)
	mux.HandleFunc("GET /internal/v1/hosts", g.handleHosts)
	mux.HandleFunc("GET /worker-release", g.handleWorkerRelease)
	mux.HandleFunc("PUT /worker-release", g.handleWorkerRelease)
	mux.HandleFunc("GET /internal/v1/worker-release", g.handleWorkerRelease)
	mux.HandleFunc("PUT /internal/v1/worker-release", g.handleWorkerRelease)
	mux.HandleFunc("GET /metrics", g.handleMetrics)
	// Per-host detail, federated: the gateway scrapes each live host's /metrics
	// (it already holds their addr+token) and re-exports every series with a
	// host="<id>" label. Prometheus keeps scraping only the gateway, and the
	// dynamic worker fleet needs no service discovery.
	mux.HandleFunc("GET /metrics/hosts", g.handleHostMetrics)
	mux.HandleFunc("POST /sandboxes", g.handleCreate)
	mux.HandleFunc("GET /sandboxes", g.handleList)
	// Drain moves a host's sandboxes elsewhere (release on the source, adopt on
	// a target) — maintenance, or rebalancing (roadmap B4).
	mux.HandleFunc("POST /hosts/{host}/drain", g.handleDrain)
	mux.HandleFunc("POST /internal/v1/hosts/{action}", g.handleInternalHostAction)
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
	mux.HandleFunc("PATCH /snapshots/{id}/public-fields", g.handleSnapshotOp)
	mux.HandleFunc("DELETE /snapshots/{id}", g.handleSnapshotOp)
	apiv1.New(mux).Register(mux)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	tlsConfig, err := g.transport.TLSConfig()
	if err != nil {
		_ = ln.Close()
		return err
	}
	if tlsConfig != nil {
		ln = tls.NewListener(ln, tlsConfig)
	}
	srv := &http.Server{
		Addr:      addr,
		Handler:   httpapi.Middleware(g.bearerAuth(mux)),
		TLSConfig: tlsConfig,
	}
	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	fmt.Fprintf(os.Stderr, "gateway listening on %s (transport=%s, separated bearer auth)\n", addr, g.transport.Mode)

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

func (g *Gateway) handleInternalHostAction(w http.ResponseWriter, r *http.Request) {
	hostID, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || hostID == "" || action != "drain" {
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("host", hostID)
	g.handleDrain(w, r)
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
	if g.transport.Mode != management.TransportDevelopment &&
		!management.IsEncryptedOrPrivateEndpoint(hb.Addr) {
		httpError(w, http.StatusBadRequest, errors.New("worker address must use HTTPS or a verifiably private IP"))
		return
	}
	callbackToken := hb.ControlToken
	if callbackToken == "" && g.transport.Mode == management.TransportDevelopment {
		callbackToken = hb.Token
		if callbackToken == "" {
			callbackToken = g.token
		}
	}
	if callbackToken == "" {
		httpError(w, http.StatusBadRequest, errors.New("heartbeat missing worker control credential"))
		return
	}

	g.mu.Lock()
	// A host's advertised API address is unique within the fleet. Treat it as
	// the physical-host identity backstop when HostID changes across a process
	// restart (for example, a resumed GCE worker first reporting its short
	// hostname and the replacement Nomad allocation reporting its FQDN).
	//
	// Keeping both entries would double-count the same capacity. Worse, creates
	// routed through the old ID would become unreachable when that entry timed
	// out, even though the sandboxes still exist at this address. The incoming
	// heartbeat is authoritative for the address, so discard the superseded
	// identity before rebuilding its routes below.
	for id, existing := range g.hosts {
		if id != hb.HostID && existing.addr == hb.Addr {
			fmt.Fprintf(os.Stderr, "gateway: host %s replaced by %s at %s\n", id, hb.HostID, hb.Addr)
			g.dropHostLocked(id)
		}
	}
	h := g.hosts[hb.HostID]
	if h == nil {
		h = &host{id: hb.HostID}
		g.hosts[hb.HostID] = h
		fmt.Fprintf(os.Stderr, "gateway: host %s registered (%s)\n", hb.HostID, hb.Addr)
	}
	h.addr = hb.Addr
	h.token = callbackToken
	h.release = hb.Release
	h.slotsTotal = hb.SlotsTotal
	h.slotsUsed = hb.SlotsUsed
	h.warmReady = hb.WarmReady
	h.slotsFree = hb.SlotsTotal - hb.SlotsUsed // old host binary: best guess
	if hb.SlotsFree != nil {
		h.slotsFree = *hb.SlotsFree
	}
	// SlotsUsed and SlotsFree are sampled by the worker. Older releases
	// obtained them with separate registry reads, so concurrent deletes could
	// pair an older used count with newer free capacity (observed as used=7,
	// free=46 on a 48-slot host). Never advertise more than total-used. A
	// temporarily conservative count self-corrects on the next heartbeat;
	// accepting an impossible optimistic count can over-place.
	if h.slotsTotal < 0 {
		h.slotsTotal = 0
	}
	if h.slotsUsed < 0 {
		h.slotsUsed = 0
	}
	if h.slotsUsed > h.slotsTotal {
		h.slotsUsed = h.slotsTotal
	}
	if h.warmReady < 0 {
		h.warmReady = 0
	}
	if h.warmReady > h.slotsTotal {
		h.warmReady = h.slotsTotal
	}
	maxFree := h.slotsTotal - h.slotsUsed
	if h.slotsFree < 0 {
		h.slotsFree = 0
	}
	if h.slotsFree > maxFree {
		h.slotsFree = maxFree
	}
	if g.expectedRelease != "" && h.release != g.expectedRelease {
		h.slotsFree = 0
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
	// It also changes the inputs to desired-capacity sizing (a worker becoming
	// eligible, occupancy moving), so re-evaluate scale-out at the same time.
	g.notifyDirectScale()

	w.WriteHeader(http.StatusNoContent)
}

// --- placement & create ---

func (g *Gateway) handleCreate(w http.ResponseWriter, r *http.Request) {
	// Forward the create body to the chosen host VERBATIM. Decoding into a typed
	// struct here would silently drop any field the gateway's client build
	// doesn't yet model (e.g. ssh_pubkey), so the gateway must not need
	// rebuilding in lockstep with every new POST /sandboxes field. We still
	// parse it once — purely to reject malformed JSON with a fast 400 before
	// reserving a host — but placement and forwarding use the raw bytes.
	raw, err := io.ReadAll(r.Body)
	if err != nil {
		httpError(w, 400, fmt.Errorf("read body: %w", err))
		return
	}
	if len(raw) > 0 {
		var probe client.CreateOpts
		if err := json.Unmarshal(raw, &probe); err != nil {
			httpError(w, 400, fmt.Errorf("decode body: %w", err))
			return
		}
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

		sb, err := client.NewHTTP(h.addr, h.token).CreateRaw(r.Context(), raw)
		if err == nil {
			// Landed: convert the reservation into a used slot and record the
			// route. The next heartbeat overwrites the host's counts (which now
			// include this sandbox), so the adjustment just bridges the gap.
			// landReservation also handles the host changing identity while this
			// request was in flight.
			g.landReservation(h, sb.ID)
			sb.HostAddr = hostOnly(h.addr)
			writeJSON(w, http.StatusCreated, sb)
			return
		}

		g.releaseReservation(h, false) // create failed: free the reservation
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
	return g.reserveHostMode(exclude, true)
}

// reserveHostOrdinary is used by snapshot adoption, which cannot consume a
// default-create ready VM.
func (g *Gateway) reserveHostOrdinary(exclude map[string]bool) *host {
	return g.reserveHostMode(exclude, false)
}

func (g *Gateway) reserveHostMode(exclude map[string]bool, useWarm bool) *host {
	g.mu.Lock()
	defer g.mu.Unlock()
	now := time.Now()
	var best *host
	preferWarm := false
	if useWarm {
		for _, h := range g.hosts {
			if exclude[h.id] || now.Before(h.penaltyUntil) ||
				now.Sub(h.lastSeen) > g.ttl || h.free() <= 0 {
				continue
			}
			if h.warmFree() > 0 {
				preferWarm = true
				break
			}
		}
	}
	for _, h := range g.hosts {
		if exclude[h.id] || now.Before(h.penaltyUntil) {
			continue
		}
		if now.Sub(h.lastSeen) > g.ttl || h.free() <= 0 {
			continue
		}
		if preferWarm && h.warmFree() <= 0 {
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
	if preferWarm {
		best.warmReserved++
		snap.reservationWarm = true
	}
	return &snap
}

// release ends a create's reservation. landed=true means the sandbox came up,
// so it debits the advertised free count until the host's next heartbeat;
// landed=false (create failed) just frees the reservation.
//
// A heartbeat can include a registry-committed sandbox while its create
// response is still in flight. Optimistically increment used for fresh
// landings, but cap it at physical capacity so that race cannot report an
// impossible slots_used > slots_total. The next heartbeat supplies the exact
// count; slotsFree + reserved remain the placement accounting bridge.
func (g *Gateway) releaseReservation(reserved *host, landed bool) {
	g.mu.Lock()
	h := g.hosts[reserved.id]
	if h == nil {
		g.mu.Unlock()
		return
	}
	if h.reserved > 0 {
		h.reserved--
	}
	if reserved.reservationWarm && h.warmReserved > 0 {
		h.warmReserved--
	}
	// Once a request was dispatched against a ready VM, stop advertising that
	// VM even if the request failed: the worker may have claimed it before
	// returning the error. A later heartbeat restores the count when the
	// request never reached the worker or replenishment completed.
	if reserved.reservationWarm && h.warmReady > 0 {
		h.warmReady--
	}
	if landed {
		if h.slotsUsed < h.slotsTotal {
			h.slotsUsed++
		}
		if h.slotsFree > 0 {
			h.slotsFree--
		}
	}
	g.mu.Unlock()
	if landed {
		g.createsOK.Add(1)
	}
	if !landed {
		// A freed reservation is capacity: nudge one queued create to retry
		// now instead of waiting out its poll tick.
		g.notifySlotFreed()
	}
}

// landReservation records a successful create/adopt and converts its
// reservation into a used slot. A host can replace its HostID while the
// request is in flight; in that case handleRegister has removed the reserved
// ID, so resolve the current identity by the same unique advertised address
// before writing the route. It returns the identity the route was recorded
// against.
func (g *Gateway) landReservation(reserved *host, sandboxID string) string {
	g.mu.Lock()
	h := g.hosts[reserved.id]
	if h == nil || h.addr != reserved.addr {
		h = nil
		for _, candidate := range g.hosts {
			if candidate.addr == reserved.addr {
				h = candidate
				break
			}
		}
	}
	routeID := reserved.id
	if h != nil {
		routeID = h.id
		if h.reserved > 0 {
			h.reserved--
		}
		if reserved.reservationWarm && h.warmReserved > 0 {
			h.warmReserved--
		}
		if h.slotsUsed < h.slotsTotal {
			h.slotsUsed++
		}
		if h.slotsFree > 0 {
			h.slotsFree--
		}
		if reserved.reservationWarm && h.warmReady > 0 {
			h.warmReady--
		}
	}
	g.route[sandboxID] = routeID
	g.mu.Unlock()
	g.createsOK.Add(1)
	return routeID
}

// notifySlotFreed broadcasts to every awaitHost waiter without blocking. Each
// waiter still reserves atomically, so only the newly available slots win; the
// rest return to waiting. The 250ms poll remains the missed-signal backstop.
func (g *Gateway) notifySlotFreed() {
	g.slotFreedMu.Lock()
	close(g.slotFreed)
	g.slotFreed = make(chan struct{})
	g.slotFreedMu.Unlock()
}

func (g *Gateway) slotFreedSignal() <-chan struct{} {
	g.slotFreedMu.Lock()
	ch := g.slotFreed
	g.slotFreedMu.Unlock()
	return ch
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
	depth := g.queued.Add(1)
	if depth > int64(g.queueMax) {
		g.queued.Add(-1)
		return nil
	}
	// Level-triggered: notify on EVERY enqueue, not just the 0 -> 1 edge, so a
	// burst that keeps the queue non-empty still re-sizes as it grows.
	// Evaluations coalesce, so the extra notifies are nearly free.
	g.notifyDirectScale()
	defer func() {
		g.queued.Add(-1)
		// Re-baseline the watermark once the queue drains.
		g.notifyDirectScale()
	}()

	timeout := time.NewTimer(wait)
	defer timeout.Stop()
	tick := time.NewTicker(queuePollInterval)
	defer tick.Stop()
	for {
		slotFreed := g.slotFreedSignal()
		select {
		case <-ctx.Done():
			return nil
		case <-timeout.C:
			return nil
		case <-slotFreed:
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

const directScaleDebounce = 50 * time.Millisecond

// notifyDirectScale requests a coalesced re-evaluation of desired capacity.
// Safe and cheap to call from any demand-changing path: evaluations collapse
// into at most one in-flight worker, and an evaluation that does not raise the
// watermark issues no provider call at all.
func (g *Gateway) notifyDirectScale() {
	if g.directScaler == nil {
		return
	}
	g.directDirty.Store(true)
	if !g.directScalePending.CompareAndSwap(false, true) {
		// An evaluator already owns the work and will observe directDirty.
		return
	}
	go g.directScaleWorker()
}

// directScaleWorker drains re-evaluation requests until none remain. The short
// debounce before each pass lets concurrent requests enter the queue and lets
// in-flight reservations become visible, without paying a scrape interval.
func (g *Gateway) directScaleWorker() {
	for {
		for g.directDirty.Swap(false) {
			timer := time.NewTimer(directScaleDebounce)
			<-timer.C
			timer.Stop()
			g.evaluateDirectScaleOut()
		}
		g.directScalePending.Store(false)
		// A notify landing between the Swap above and this Store would have
		// seen pending==true and declined to spawn, so re-check before exiting.
		if !g.directDirty.Load() {
			return
		}
		if !g.directScalePending.CompareAndSwap(false, true) {
			return // another goroutine took ownership
		}
	}
}

// evaluateDirectScaleOut sizes the fleet to current demand and requests a
// resize only when that exceeds the grow-only watermark.
func (g *Gateway) evaluateDirectScaleOut() {
	queued := int(g.queued.Load())

	now := time.Now()
	live, occupied := 0, 0
	g.mu.RLock()
	for _, h := range g.hosts {
		if now.Sub(h.lastSeen) > g.ttl {
			continue
		}
		live++
		occupied += h.committed() + h.hibernated
	}
	g.mu.RUnlock()

	if queued == 0 {
		// Idle. Re-baseline the watermark down to the fleet's true size so the
		// next burst can grow from there; never below, or a scale-in would
		// suppress scale-out forever.
		if int64(live) < g.directRequested.Load() {
			g.directRequested.Store(int64(live))
		}
		return
	}

	demand := ceilDiv(occupied+queued+g.directHeadroom, g.directSlotsPerHost)
	desired := demand
	if desired <= live {
		// A non-empty queue proves the currently registered fleet cannot
		// place the request, even if stale accounting says it should fit.
		desired = live + 1
	}
	// Bound the stale-accounting nudge by what demand actually justifies. Without
	// this, every new worker that joins while the queue is still draining would
	// push desired to live+1 again and ratchet the group above demand.
	if ceiling := demand + 1; desired > ceiling {
		desired = ceiling
	}
	if int64(desired) <= g.directRequested.Load() {
		return // already asked for at least this much
	}
	g.directRequested.Store(int64(desired))

	g.directScaleStarted.Add(1)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err := g.directScaler.ScaleOut(ctx, desired); err != nil {
		g.directScaleFailed.Add(1)
		fmt.Fprintf(os.Stderr, "gateway: direct scale-out to %d workers failed: %v\n", desired, err)
		return
	}
	fmt.Fprintf(os.Stderr, "gateway: direct scale-out requested %d workers (live=%d occupied=%d queued=%d)\n",
		desired, live, occupied, queued)
	// Re-read immediately so the exported target reflects this resize instead of
	// staying a poll interval stale — the scale-in ceiling is derived from it.
	if sizer, ok := g.directScaler.(TargetSizer); ok {
		g.refreshMIGTarget(context.Background(), sizer)
	}
}

// migTargetPollInterval matches the Prometheus scrape cadence: one cheap
// provider GET per interval keeps sandbox_mig_target_size fresh enough for the
// scale-in ceiling without adding meaningful API load.
const migTargetPollInterval = 10 * time.Second

// migTargetLoop keeps the exported provider target size current. It is a no-op
// unless the configured scaler can report one.
func (g *Gateway) migTargetLoop(ctx context.Context) {
	sizer, ok := g.directScaler.(TargetSizer)
	if !ok {
		return
	}
	ticker := time.NewTicker(migTargetPollInterval)
	defer ticker.Stop()
	for {
		g.refreshMIGTarget(ctx, sizer)
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
	}
}

func (g *Gateway) refreshMIGTarget(ctx context.Context, sizer TargetSizer) {
	pollCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	size, err := sizer.TargetSize(pollCtx)
	if err != nil {
		// Keep the last known value: a transient provider error must not drop
		// the ceiling and hand the autoscaler a scale-in it shouldn't make.
		return
	}
	g.migTarget.Store(int64(size))
	g.migTargetKnown.Store(true)
}

func ceilDiv(n, d int) int {
	if d <= 0 {
		return 0
	}
	if n <= 0 {
		return 0
	}
	return (n + d - 1) / d
}

// hostOnly strips the port from an addr, so clients can pair it with a
// sandbox's forwarded ports (which live on the host, not the gateway).
func hostOnly(addr string) string {
	if parsed, err := url.Parse(addr); err == nil && parsed.Host != "" {
		if host := parsed.Hostname(); host != "" {
			return portHost(host)
		}
	}
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return portHost(h)
	}
	return addr
}

func portHost(host string) string {
	if strings.Contains(host, ":") {
		return "[" + strings.Trim(host, "[]") + "]"
	}
	return host
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
	routeOwners := make(map[string]bool)
	for _, hostID := range g.route {
		routeOwners[hostID] = true
	}
	var candidates []host
	for _, h := range g.hosts {
		// Listing an empty placement-quarantined refill worker adds no data and
		// can turn the whole fleet list into a 502 while the MIG suspends it.
		// Skip only when every ownership signal agrees it is empty. Routes
		// cover hibernated and heartbeat-known identities; occupancy is a
		// conservative backstop for route/accounting skew; reservations cover
		// creates that can commit after this snapshot.
		if !routeOwners[h.id] && h.slotsUsed == 0 && h.hibernated == 0 && h.reserved == 0 {
			continue
		}
		candidates = append(candidates, *h)
	}
	g.mu.RUnlock()

	// Fan out concurrently: a sequential sweep makes list latency grow linearly
	// with fleet size. A per-host timeout keeps one wedged host from stalling
	// the whole response.
	var (
		mu       sync.Mutex
		wg       sync.WaitGroup
		out      = []registry.Sandbox{}
		failures = map[string]error{}
	)
	for _, h := range candidates {
		wg.Add(1)
		go func(h host) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			sandboxes, err := client.NewHTTP(h.addr, h.token).List(ctx)
			if err != nil {
				fmt.Fprintf(os.Stderr, "gateway: list from host %s: %v\n", h.id, err)
				mu.Lock()
				failures[h.id] = err
				mu.Unlock()
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
	if len(failures) > 0 {
		// A partial list is indistinguishable from sandbox loss to callers and
		// has caused real clients to conclude that held sandboxes disappeared
		// while an owning worker was merely unreachable. Fail closed instead of
		// returning a plausible-looking 200 response assembled from the other
		// hosts. The caller can retry; the error also preserves the underlying
		// fleet availability signal.
		hostIDs := make([]string, 0, len(failures))
		for hostID := range failures {
			hostIDs = append(hostIDs, hostID)
		}
		sort.Strings(hostIDs)
		details := make([]string, 0, len(hostIDs))
		for _, hostID := range hostIDs {
			details = append(details, fmt.Sprintf("%s: %v", hostID, failures[hostID]))
		}
		httpError(w, http.StatusBadGateway,
			fmt.Errorf("sandbox list incomplete; %d/%d candidate hosts failed: %s",
				len(hostIDs), len(candidates), strings.Join(details, "; ")))
		return
	}
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
	target, err := url.Parse(client.EndpointURL(addr))
	if err != nil {
		target = &url.URL{Scheme: "http", Host: addr}
	}
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
		// Query-string credentials are never accepted. Strip legacy attempts so
		// a secret cannot ride into worker traces during migration.
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

	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, client.EndpointURL(snap.addr)+"/info", nil)
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

func (g *Gateway) handleWorkerRelease(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		g.mu.RLock()
		release := g.expectedRelease
		g.mu.RUnlock()
		writeJSON(w, http.StatusOK, map[string]string{"release": release})
		return
	}

	var body struct {
		Release string `json:"release"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 4<<10)).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("decode worker release: %w", err))
		return
	}
	body.Release = strings.TrimSpace(body.Release)
	if err := validateWorkerRelease(body.Release); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	if g.releaseFile == "" {
		httpError(w, http.StatusServiceUnavailable, errors.New("worker release persistence is not configured"))
		return
	}

	g.releaseUpdateMu.Lock()
	defer g.releaseUpdateMu.Unlock()
	if err := os.MkdirAll(filepath.Dir(g.releaseFile), 0o755); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("create worker release directory: %w", err))
		return
	}
	tmp := g.releaseFile + ".tmp"
	if err := os.WriteFile(tmp, []byte(body.Release+"\n"), 0o644); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("write worker release: %w", err))
		return
	}
	if err := os.Rename(tmp, g.releaseFile); err != nil {
		_ = os.Remove(tmp)
		httpError(w, http.StatusInternalServerError, fmt.Errorf("persist worker release: %w", err))
		return
	}

	g.mu.Lock()
	g.expectedRelease = body.Release
	for _, h := range g.hosts {
		if h.release != body.Release {
			h.slotsFree = 0
		}
	}
	g.mu.Unlock()
	fmt.Fprintf(os.Stderr, "gateway: worker release set to %s; stale workers gated from placement\n", body.Release)
	writeJSON(w, http.StatusOK, map[string]string{"release": body.Release})
}

func validateWorkerRelease(release string) error {
	if release == "" {
		return errors.New("worker release cannot be empty")
	}
	if len(release) > 128 {
		return errors.New("worker release is longer than 128 bytes")
	}
	for _, r := range release {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') ||
			(r >= '0' && r <= '9') || r == '.' || r == '_' || r == '-' {
			continue
		}
		return fmt.Errorf("worker release contains invalid character %q", r)
	}
	return nil
}

func (g *Gateway) handleHosts(w http.ResponseWriter, r *http.Request) {
	type hostView struct {
		ID         string `json:"id"`
		Addr       string `json:"addr"`
		Release    string `json:"release,omitempty"`
		Compatible bool   `json:"release_compatible"`
		SlotsTotal int    `json:"slots_total"`
		SlotsUsed  int    `json:"slots_used"`
		Hibernated int    `json:"hibernated"`
		Free       int    `json:"free"`
		WarmReady  int    `json:"warm_ready"`
		Alive      bool   `json:"alive"`
		LastSeenMS int64  `json:"last_seen_ms_ago"`
	}
	g.mu.RLock()
	views := []hostView{}
	for _, h := range g.hosts {
		compatible := g.expectedRelease == "" || h.release == g.expectedRelease
		views = append(views, hostView{
			ID: h.id, Addr: h.addr, Release: h.release, Compatible: compatible,
			SlotsTotal: h.slotsTotal, SlotsUsed: h.slotsUsed,
			Hibernated: h.hibernated, Free: h.free(), Alive: time.Since(h.lastSeen) <= g.ttl,
			WarmReady:  h.warmFree(),
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
					g.dropHostLocked(id)
				}
			}
			g.mu.Unlock()
		}
	}
}

// dropHostLocked removes a host and every derived cache entry that names it.
// g.mu must be held for writing.
func (g *Gateway) dropHostLocked(id string) {
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

// --- helpers (mirrors internal/server) ---

func (g *Gateway) bearerAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		workerOnly := r.URL.Path == "/register" || strings.HasPrefix(r.URL.Path, "/internal/v1/")
		workerMatch := g.workerCredentials != nil && g.workerCredentials.MatchAuthorization(auth)
		clientMatch := g.clientCredentials != nil && g.clientCredentials.MatchAuthorization(auth)
		// Fail closed if independently rotated files ever acquire an
		// overlapping token after startup.
		ok := workerOnly && workerMatch && !clientMatch
		if !workerOnly && g.clientCredentials != nil {
			ok = clientMatch && !workerMatch
		}
		if !ok {
			err := errors.New("missing or invalid bearer token")
			if wsutil.IsUpgrade(r) && wsutil.Reject(w, r, wsutil.CloseUnauthorized, err.Error()) == nil {
				return
			}
			if strings.HasPrefix(r.URL.Path, "/v1/") {
				httpapi.WriteProblem(w, r, http.StatusUnauthorized, "unauthorized", err.Error())
			} else {
				httpError(w, http.StatusUnauthorized, err)
			}
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
