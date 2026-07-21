package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// Config bundles everything the server needs at startup.
type Config struct {
	SocketPath  string
	ListenAddr  string // optional TCP listener (e.g. tailnet IP:port); requires APIToken
	APIToken    string // bearer token enforced on the TCP listener only
	Provisioner *provisioner.Provisioner
	GatewayIP   string // bridge IP; used as the guest's default gateway
	// GuestSubnetBits is the prefix length shared by the gateway and every
	// guest NIC (cold-boot GuestCIDR and the clone-path MMDS reidentify
	// prefix). Must match the gateway CIDR. <=0 falls back to 24.
	GuestSubnetBits int
	VMTemplate      vm.RunOptions // base options (firecracker bin, kernel, args, vcpus, mem, dns)
	HotCreate       bool          // maintain a golden snapshot and serve POST /sandboxes by cloning it
	// CreateConcurrency bounds concurrent sandbox bring-ups (cold boots and
	// golden clones); excess creates queue. <=0 = default: min(2×NumCPU, 16).
	CreateConcurrency int
	// MemBudgetMIB caps committed guest memory (mem_mib + per-VM overhead)
	// across running sandboxes. 0 = derive from host total − 2 GiB;
	// <0 = disabled. See config.MemBudgetMIB.
	MemBudgetMIB int64
	// HibernateAfter freezes sandboxes idle this long to disk (snapshot +
	// kill), releasing their slot; any agent-bound request wakes them.
	// 0 disables idle hibernation. See hibernate.go.
	HibernateAfter time.Duration
	// UFFDRestore restores same-identity hibernation wakes with the userfaultfd
	// memory backend (lazy page-in) instead of the eager File backend. See
	// config.UFFDRestore and uffd_linux.go.
	UFFDRestore bool
	// UFFDChunkBytes selects the UFFD page source: 0 = whole-file mmap, >0 = lazy
	// per-chunk reads of that size through a chunk cache (roadmap Phase B1). See
	// config.UFFDChunkKiB and vm.RunOptions.UFFDChunkBytes.
	UFFDChunkBytes uint64
	// SnapshotBucket enables GCS snapshot durability: user snapshots upload
	// in the background and restore/fanout pull missing snapshots down from
	// the bucket, so any host can serve them. Empty = host-local only.
	SnapshotBucket string

	// --- Gateway registration (optional; Phase-1 multi-host) ---
	// When GatewayURL is set, the server periodically heartbeats to the gateway
	// so it can route requests to this host. Requires ListenAddr (the gateway
	// dials back over TCP using APIToken).
	GatewayURL    string // e.g. "http://100.x.y.z:9090"; empty disables registration
	GatewayToken  string // bearer the host presents to the gateway
	AdvertiseAddr string // addr the gateway should dial back; defaults to ListenAddr
	HostID        string // stable host identity; defaults to hostname
}

// Server holds runtime state for the sandbox API.
type Server struct {
	cfg      Config
	reg      *registry.Registry
	machines sync.Map        // map[string]*vm.Machine
	vmCtx    context.Context // long-lived; tied to Serve's ctx, NOT request ctx

	// golden is the snapshot POST /sandboxes clones from when hot create is on.
	// nil until ensureGolden adopts or builds one; cleared if it's deleted.
	golden  atomic.Pointer[registry.Snapshot]
	stageMu sync.Mutex // serializes re-staging the golden snapshot's baked rootfs

	// blob is the GCS client for snapshot durability; nil when disabled.
	blob *gcsblob.Client
	// baseUpMu/basesUploaded gate the once-per-base template upload.
	baseUpMu      sync.Mutex
	basesUploaded map[string]bool
	// pullMu/pulls serialize concurrent GCS pulls of the same snapshot id.
	pullMu sync.Mutex
	pulls  map[string]*sync.Mutex

	// act tracks per-sandbox API activity for idle hibernation; wakesMu/wakes
	// serialize hibernate/wake/destroy per sandbox id.
	act     *activityTracker
	wakesMu sync.Mutex
	wakes   map[string]*sync.Mutex

	// diffBase maps a live machine's sandbox id → the snapshot id its
	// dirty-page bitmap is tracking against. An entry exists ONLY while a diff
	// snapshot would be valid: set when a clone is loaded from a snapshot
	// (bringUpClone→finishClone), deleted the moment the bitmap stops matching
	// that base — any snapshot of the sandbox (Firecracker resets the bitmap
	// at snapshot creation) or a machine reload (hibernation wake loads from
	// the hib mem, not the original base). sb.BaseSnapshotID alone is NOT
	// sufficient: it's never cleared, so trusting it would write diffs against
	// the wrong base after a wake or a second snapshot — silent memory
	// corruption on restore.
	diffBase sync.Map // sandbox id → snapshot id

	// pf owns the userspace host-port → guest-port TCP proxies (see
	// portproxy.go). Its listeners persist through hibernation so a connection
	// to a frozen sandbox's port wakes it.
	pf *portForwarder

	// createSem bounds concurrent sandbox bring-ups (cold boots AND golden
	// clones) so a burst of creates queues instead of boot-storming the host —
	// the 60 s agent gate only starts ticking once a slot is acquired. Fanout
	// keeps its own separate budget.
	createSem chan struct{}
	// warmed is closed once ensureGolden has settled (adopted, built, or
	// failed). Until then the heartbeat advertises SlotsFree=0 so the gateway
	// doesn't route a burst of guaranteed-cold creates at a host that's still
	// building its golden snapshot. Pre-closed when hot create is disabled.
	warmed chan struct{}

	// memBudgetMIB is the resolved committed-guest-memory ceiling (0 =
	// disabled). Mirrors what reg.SetMemAccounting was given; kept here so
	// validateResources/handleInfo can clamp the per-sandbox override too.
	memBudgetMIB int64
}

// fcOverheadMIB is the per-VM memory charged on top of the guest's mem_mib:
// firecracker/VMM overhead. 1024 (template) + 156 = 1180, matching the
// MiB-per-slot arithmetic deploy-job.sh sizes the Nomad cgroup with.
const fcOverheadMIB = 156

func New(cfg Config, reg *registry.Registry) *Server {
	s := &Server{cfg: cfg, reg: reg, basesUploaded: map[string]bool{}, pulls: map[string]*sync.Mutex{},
		act: newActivityTracker(), wakes: map[string]*sync.Mutex{}}
	sem := cfg.CreateConcurrency
	if sem <= 0 {
		sem = 2 * runtime.NumCPU()
		if sem > 16 {
			sem = 16
		}
	}
	s.createSem = make(chan struct{}, sem)
	s.warmed = make(chan struct{})
	if !cfg.HotCreate {
		close(s.warmed) // nothing to warm up: cold creates are the steady state
	}

	// Memory-aware admission: explicit budget wins (fleet hosts MUST set it —
	// /proc/meminfo shows the machine total, not the Nomad cgroup limit);
	// 0 derives machine total minus a 2 GiB host reserve; negative (or a
	// failed derivation) disables admission entirely.
	budget := cfg.MemBudgetMIB
	if budget == 0 {
		if total := hostTotalMemMIB(); total > 2048 {
			budget = total - 2048
		}
	}
	if budget < 0 {
		budget = 0
	}
	s.memBudgetMIB = budget
	reg.SetMemAccounting(registry.MemAccounting{
		TemplateMemMIB: cfg.VMTemplate.MemMIB,
		BudgetMIB:      budget,
		OverheadMIB:    fcOverheadMIB,
	})
	if budget > 0 && budget < cfg.VMTemplate.MemMIB+fcOverheadMIB {
		fmt.Fprintf(os.Stderr, "WARNING: mem_budget_mib %d cannot fit even one template sandbox (%d+%d MiB) — every create (incl. the golden build) will be rejected\n",
			budget, cfg.VMTemplate.MemMIB, fcOverheadMIB)
	}

	s.pf = newPortForwarder(s.dialGuest, s.act.begin)
	if cfg.SnapshotBucket != "" {
		s.blob = gcsblob.New(cfg.SnapshotBucket)
		fmt.Fprintf(os.Stderr, "snapshot durability on: gs://%s\n", cfg.SnapshotBucket)
	}
	return s
}

// Serve listens on the configured Unix socket — and, if ListenAddr is set, on
// TCP with bearer-token auth — until ctx is cancelled. On shutdown, running
// sandboxes are hibernated (frozen to disk, wakeable on next start) rather
// than destroyed — see shutdownAll.
func (s *Server) Serve(ctx context.Context) error {
	// vmCtx must NOT be the serve ctx: the firecracker SDK (and the raw clone
	// path's CommandContext) kill their VMs the moment their context cancels,
	// and the serve ctx cancels on SIGTERM — before shutdownAll gets a chance
	// to freeze anything. Decouple it and cancel explicitly on the way out as
	// the backstop for VMs that outlive shutdown.
	vmCtx, vmCancel := context.WithCancel(context.Background())
	defer vmCancel()
	s.vmCtx = vmCtx

	s.reconcile(ctx)
	// Hibernated sandboxes survived reconcile; re-bind their port-forward
	// listeners or wake-on-connect breaks after a server restart.
	s.reopenPortListeners(ctx)
	go s.reapExpired(ctx)
	if s.cfg.HotCreate {
		go s.ensureGolden(ctx)
	}
	if s.cfg.GatewayURL != "" {
		go s.heartbeat(ctx)
	}
	// Always runs: even with no host-wide default, individual sandboxes can
	// opt in via hibernate_after_sec at create time.
	go s.hibernateLoop(ctx)
	if s.cfg.HibernateAfter > 0 {
		fmt.Fprintf(os.Stderr, "idle hibernation on: default freeze after %s idle (per-sandbox hibernate_after_sec overrides)\n", s.cfg.HibernateAfter)
	}

	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o755); err != nil {
		return err
	}
	_ = os.Remove(s.cfg.SocketPath) // clear stale socket

	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(s.cfg.SocketPath, 0o600)

	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", s.handleInfo)
	mux.HandleFunc("POST /sandboxes", s.handleCreate)
	mux.HandleFunc("GET /sandboxes", s.handleList)
	mux.HandleFunc("GET /sandboxes/{id}", s.handleGet)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.handleDestroy)
	mux.HandleFunc("POST /sandboxes/{id}/timeout", s.handleSetTimeout)
	mux.HandleFunc("POST /sandboxes/{id}/rename", s.handleRename)
	mux.HandleFunc("POST /sandboxes/{id}/ports", s.handleExposePort)
	mux.HandleFunc("GET /sandboxes/{id}/ports", s.handleListPorts)
	mux.HandleFunc("POST /sandboxes/{id}/exec", s.handleAgentProxy("exec"))
	mux.HandleFunc("POST /sandboxes/{id}/exec/stream", s.handleAgentProxy("exec/stream"))
	mux.HandleFunc("GET /sandboxes/{id}/files", s.handleAgentProxy("files"))
	mux.HandleFunc("PUT /sandboxes/{id}/files", s.handleAgentProxy("files"))
	mux.HandleFunc("GET /sandboxes/{id}/dir", s.handleAgentProxy("dir"))
	mux.HandleFunc("GET /sandboxes/{id}/shell", s.handleShellProxy())
	mux.HandleFunc("POST /sandboxes/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /sandboxes/{id}/hibernate", s.handleHibernate)
	mux.HandleFunc("GET /snapshots", s.handleListSnapshots)
	mux.HandleFunc("POST /snapshots/{id}/rename", s.handleRenameSnapshot)
	mux.HandleFunc("POST /snapshots/{id}/restore", s.handleRestore)
	mux.HandleFunc("POST /snapshots/{id}/fanout", s.handleFanout)
	mux.HandleFunc("DELETE /snapshots/{id}", s.handleDeleteSnapshot)

	servers := []*http.Server{{Handler: mux}}
	srvErr := make(chan error, 2)
	go func() { srvErr <- servers[0].Serve(ln) }()

	if s.cfg.ListenAddr != "" {
		if s.cfg.APIToken == "" {
			return errors.New("listen_addr is set but api_token is empty — refusing to serve TCP without auth")
		}
		tcpLn, err := net.Listen("tcp", s.cfg.ListenAddr)
		if err != nil {
			return fmt.Errorf("listen tcp %s: %w", s.cfg.ListenAddr, err)
		}
		tcpSrv := &http.Server{Handler: bearerAuth(s.cfg.APIToken, mux)}
		servers = append(servers, tcpSrv)
		go func() { srvErr <- tcpSrv.Serve(tcpLn) }()
		fmt.Fprintf(os.Stderr, "TCP API listening on %s (bearer auth)\n", s.cfg.ListenAddr)
	}

	select {
	case <-ctx.Done():
		// Short drain: freezing sandboxes (below) matters more than letting
		// slow API requests finish — the whole stop window is ~120 s (Nomad
		// kill_timeout / GCE stop) and hibernation needs most of it.
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, srv := range servers {
			_ = srv.Shutdown(shCtx)
		}
		cancel()
		s.shutdownAll()
		s.pf.CloseAll() // hibernated sandboxes' listeners; reopened next startup
		return nil
	case err := <-srvErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// bearerAuth rejects requests whose Authorization header doesn't carry token.
// Applied only to the TCP listener — the Unix socket is protected by file mode.
// WebSocket upgrades get two accommodations for browser clients, which cannot
// set request headers on a WebSocket: the token may ride in ?access_token=,
// and a rejection is delivered as a post-handshake close frame (4401) instead
// of a plain 401 the browser would collapse into an opaque 1006.
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

// shutdownAll freezes every tracked sandbox on server stop. Hibernate, not
// destroy: a server stop is routinely the HOST going away underneath live
// sandboxes — autoscaler scale-in, or the MIG stopping a standby-pool refill
// VM that had already taken placements — and hibernated rows survive it
// (artifacts + SQLite live on the persistent disk; reconcile skips them and
// re-binds their port listeners on the next start, so they come back
// wakeable). Diff hibernation keeps the write volume inside the stop window.
// Bounded parallelism: the mem writes all hit one disk. A sandbox that can't
// be frozen in the window is destroyed, as before.
func (s *Server) shutdownAll() {
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Second)
	defer cancel()
	sem := make(chan struct{}, 8)
	var wg sync.WaitGroup
	s.machines.Range(func(k, _ any) bool {
		id := k.(string)
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			// force=true: open connections are dying with the server anyway —
			// a busy pin must not condemn the sandbox to destruction.
			if err := s.hibernate(ctx, id, true); err != nil {
				fmt.Fprintf(os.Stderr, "[%s] shutdown hibernate failed (%v), destroying\n", id, err)
				_ = s.destroy(context.Background(), id)
			}
		}()
		return true
	})
	wg.Wait()
}

// --- HTTP handlers ---

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The body is optional (older clients send none); tolerate EOF.
	var body struct {
		Name              string `json:"name"`
		TimeoutSec        int    `json:"timeout_sec"`
		HibernateAfterSec int    `json:"hibernate_after_sec"`
		Vcpus             int64  `json:"vcpus"`
		MemMIB            int64  `json:"mem_mib"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if body.TimeoutSec < 0 {
		httpError(w, 400, errors.New("timeout_sec must be >= 0"))
		return
	}
	if body.HibernateAfterSec < -1 {
		httpError(w, 400, errors.New("hibernate_after_sec must be >= -1 (-1 = never, 0 = host default)"))
		return
	}
	if err := s.validateResources(body.Vcpus, body.MemMIB); err != nil {
		httpError(w, 400, err)
		return
	}
	if err := validateName(body.Name); err != nil {
		httpError(w, 400, err)
		return
	}
	var expiresAt *time.Time
	if body.TimeoutSec > 0 {
		t := time.Now().Add(time.Duration(body.TimeoutSec) * time.Second)
		expiresAt = &t
	}

	// Bound concurrent bring-ups: a burst queues here instead of boot-storming
	// the host. Queuing is correct (not 503) — the gateway only dispatches up
	// to the host's advertised free slots, and the 60 s agent gate below only
	// starts once a slot is acquired.
	if err := s.acquireCreate(ctx); err != nil {
		httpError(w, 499, fmt.Errorf("cancelled while queued for create slot: %w", err))
		return
	}
	defer s.releaseCreate()

	// Hot path: clone the golden snapshot when one is ready. Any failure falls
	// back to a cold boot, so a create is never worse off than before.
	// Resource overrides force the cold path: the golden snapshot bakes the
	// template's vcpus/mem at snapshot time, so a clone can't change them.
	if snap := s.golden.Load(); snap != nil && body.Vcpus == 0 && body.MemMIB == 0 {
		sb, err := s.createFromSnapshot(ctx, *snap, body.Name, expiresAt, body.HibernateAfterSec)
		if err == nil {
			writeJSON(w, 201, s.effectiveResources(sb))
			return
		}
		fmt.Fprintf(os.Stderr, "hot create from golden snapshot %s failed, cold-booting instead: %v\n", snap.ID, err)
	}

	sb, err := s.createCold(ctx, body.Name, expiresAt, body.HibernateAfterSec, body.Vcpus, body.MemMIB)
	if err != nil {
		capacityOrHTTPError(w, 500, err)
		return
	}
	writeJSON(w, 201, s.effectiveResources(sb))
}

// acquireCreate takes one bring-up slot, blocking until one frees or ctx ends.
func (s *Server) acquireCreate(ctx context.Context) error {
	select {
	case s.createSem <- struct{}{}:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (s *Server) releaseCreate() { <-s.createSem }

// createCold boots a brand-new sandbox from the base rootfs: full rootfs copy,
// kernel boot, and agent startup. It blocks until the in-guest agent answers,
// so callers can exec/write files the moment it returns. vcpus/memMIB override
// the template's resources when nonzero (already validated by the caller).
func (s *Server) createCold(ctx context.Context, name string, expiresAt *time.Time, hibernateAfterSec int, vcpus, memMIB int64) (registry.Sandbox, error) {
	id := uuid.NewString()
	rootfsPath := s.cfg.Provisioner.RootfsPathFor(id)

	// Allocate identity + admission BEFORE the rootfs copy: a capacity-rejected
	// create (pool/memory exhaustion — routine under gateway failover) must not
	// pay a multi-GB copy + cleanup on a host that's already full.
	sb, err := s.reg.Create(ctx, id, name, rootfsPath, expiresAt, "", hibernateAfterSec, vcpus, memMIB)
	if err != nil {
		return registry.Sandbox{}, fmt.Errorf("registry create: %w", err)
	}

	if _, err := s.cfg.Provisioner.PrepareRootfs(id); err != nil {
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("prepare rootfs: %w", err)
	}

	if err := s.cfg.Provisioner.CreateTap(sb.TapDevice); err != nil {
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("create tap: %w", err)
	}

	opts := s.cfg.VMTemplate
	opts.RootfsPath = rootfsPath
	opts.TapDevice = sb.TapDevice
	opts.GuestCIDR = fmt.Sprintf("%s/%d", sb.GuestIP, s.guestSubnetBits())
	opts.GatewayIP = s.cfg.GatewayIP
	opts.MacAddress = randomMAC()
	opts.SocketPath = "" // auto-generate per VM
	if vcpus > 0 {
		opts.Vcpus = vcpus
	}
	if memMIB > 0 {
		opts.MemMIB = memMIB
	}

	m, rt, err := vm.NewMachine(s.vmCtx, opts, false)
	if err != nil {
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("new machine: %w", err)
	}
	if err := vm.Start(s.vmCtx, m); err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("start: %w", err)
	}
	pid, err := vm.PID(m)
	if err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("pid: %w", err)
	}

	if err := s.pf.Open(id, sb.HostPort, s.cfg.Provisioner.Network.GuestPort); err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("port forward: %w", err)
	}

	if err := s.reg.FinishStart(ctx, id, pid, rt.VMID, rt.SocketPath); err != nil {
		s.pf.CloseSandbox(id)
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("finish start: %w", err)
	}

	s.machines.Store(id, m)
	s.act.touch(id)

	// Watch for early death so we don't silently leak rows.
	go func(id string) {
		err := vm.Wait(context.Background(), m)
		fmt.Fprintf(os.Stderr, "[%s] VM exited: %v\n", id, err)
	}(id)

	if err := waitForAgent(ctx, sb.GuestIP, 60*time.Second); err != nil {
		_ = s.destroy(context.Background(), id)
		return registry.Sandbox{}, fmt.Errorf("sandbox booted but agent never became ready: %w", err)
	}

	sb.PID = pid
	sb.VMID = rt.VMID
	sb.SocketPath = rt.SocketPath
	return sb, nil
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	// Running AND hibernated — a hibernated sandbox is still addressable
	// (its next exec wakes it), so hiding it from list would be a lie.
	sandboxes, err := s.reg.ListRouted(r.Context())
	if err != nil {
		httpError(w, 500, err)
		return
	}
	if sandboxes == nil {
		sandboxes = []registry.Sandbox{}
	}
	for i := range sandboxes {
		sandboxes[i] = s.effectiveResources(sandboxes[i])
	}
	writeJSON(w, 200, sandboxes)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 404, err)
		return
	}
	writeJSON(w, 200, s.effectiveResources(sb))
}

// Info is the GET /info payload: the host's template defaults and per-sandbox
// override limits, so clients can show effective resources and validate
// overrides without guessing.
type Info struct {
	// DefaultVcpus/DefaultMemMIB are the template resources a sandbox runs
	// with when created without overrides.
	DefaultVcpus  int64 `json:"default_vcpus"`
	DefaultMemMIB int64 `json:"default_mem_mib"`
	// MaxVcpus/MaxMemMIB bound per-sandbox overrides on this host.
	MaxVcpus  int64 `json:"max_vcpus"`
	MaxMemMIB int64 `json:"max_mem_mib"`
	// GuestPort is the primary in-guest port forwarded to a host port at create.
	GuestPort int `json:"guest_port"`
	// HotCreate reports whether POST /sandboxes is served from a golden snapshot.
	HotCreate bool `json:"hot_create"`
	// HibernateAfterSec is the host's default idle-hibernation window (0 = off).
	HibernateAfterSec int `json:"hibernate_after_sec"`
	// HostID identifies this host in fleet mode; empty standalone.
	HostID string `json:"host_id,omitempty"`
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	guestPort := 0
	if s.cfg.Provisioner != nil {
		guestPort = s.cfg.Provisioner.Network.GuestPort
	}
	writeJSON(w, 200, Info{
		DefaultVcpus:      s.cfg.VMTemplate.Vcpus,
		DefaultMemMIB:     s.cfg.VMTemplate.MemMIB,
		MaxVcpus:          maxVcpus(),
		MaxMemMIB:         s.maxMemMIB(),
		GuestPort:         guestPort,
		HotCreate:         s.cfg.HotCreate,
		HibernateAfterSec: int(s.cfg.HibernateAfter / time.Second),
		HostID:            s.cfg.HostID,
	})
}

// effectiveResources fills a zero Vcpus/MemMIB with the host template's
// defaults so API responses always report the resources the sandbox actually
// runs with. The registry keeps 0 (= template default) unchanged.
func (s *Server) effectiveResources(sb registry.Sandbox) registry.Sandbox {
	if sb.Vcpus == 0 {
		sb.Vcpus = s.cfg.VMTemplate.Vcpus
	}
	if sb.MemMIB == 0 {
		sb.MemMIB = s.cfg.VMTemplate.MemMIB
	}
	return sb
}

// handleRename sets a sandbox's display name; "" clears it.
func (s *Server) handleRename(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := validateName(body.Name); err != nil {
		httpError(w, 400, err)
		return
	}
	if err := s.reg.SetName(r.Context(), id, body.Name); err != nil {
		httpError(w, 404, err)
		return
	}
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, 200, s.effectiveResources(sb))
}

// handleRenameSnapshot sets a snapshot's display name; "" clears it.
func (s *Server) handleRenameSnapshot(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := validateName(body.Name); err != nil {
		httpError(w, 400, err)
		return
	}
	if err := s.reg.SetSnapshotName(r.Context(), id, body.Name); err != nil {
		httpError(w, 404, err)
		return
	}
	snap, err := s.reg.GetSnapshot(r.Context(), id)
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, 200, snap)
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.destroy(r.Context(), id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			httpError(w, 404, fmt.Errorf("sandbox %s not found", id))
			return
		}
		httpError(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- internals ---

// rollbackPreVM cleans up rootfs + tap + registry row when the VM never came up.
func (s *Server) rollbackPreVM(id string, sb registry.Sandbox) {
	ctx := context.Background()
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	_ = s.reg.Destroy(ctx, id)
}

// destroy is the inverse of handleCreate: graceful guest shutdown, then resource cleanup.
// The per-id wake lock serializes it against a concurrent hibernate/wake of
// the same sandbox.
func (s *Server) destroy(ctx context.Context, id string) error {
	mu := s.wakeLock(id)
	mu.Lock()
	defer mu.Unlock()
	defer s.act.forget(id)
	defer s.diffBase.Delete(id)

	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get sandbox: %w", err)
	}

	// A hibernated sandbox has no VM or tap — just its port listeners,
	// artifacts, and rows.
	if sb.Status == registry.StatusHibernated {
		s.pf.CloseSandbox(id)
		_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
		_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
		return s.reg.Destroy(ctx, id)
	}

	// Read extra port mappings before reg.Destroy deletes their rows.
	ports, err := s.reg.Ports(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] list extra ports: %v\n", id, err)
	}

	if v, ok := s.machines.LoadAndDelete(id); ok {
		m := v.(*vm.Machine)
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = vm.ShutdownGuest(shCtx, m)
		cancel()
		waitDone := make(chan struct{})
		go func() {
			_ = vm.Wait(context.Background(), m)
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Minute):
			_ = vm.StopForce(m)
			<-waitDone
		}
	}

	s.pf.CloseSandbox(id)
	// Legacy DNAT cleanup: port forwarding is a userspace proxy now, but hosts
	// upgrading from the DNAT scheme may still carry rules for this sandbox.
	// Removing a nonexistent rule is harmless.
	for _, pm := range ports {
		s.cfg.Provisioner.RemovePortForwardTo(pm.HostPort, sb.GuestIP, pm.GuestPort)
	}
	s.cfg.Provisioner.RemovePortForward(sb.HostPort, sb.GuestIP)
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	return s.reg.Destroy(ctx, id)
}

// --- helpers ---

// statusFor maps an ensureRunning error onto an HTTP status: unknown sandbox
// → 404, a wake rejected for capacity (pool or memory-budget exhaustion —
// waking re-commits the frozen VM's memory) → 503, anything else → 500.
func statusFor(err error) int {
	if errors.Is(err, sql.ErrNoRows) {
		return 404
	}
	if errors.Is(err, registry.ErrPoolExhausted) {
		return http.StatusServiceUnavailable
	}
	return 500
}

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

// capacityOrHTTPError distinguishes capacity-class failures from genuine
// server errors: pool exhaustion is 503 + Retry-After (it clears as sandboxes
// are destroyed or the autoscaler adds hosts, and the gateway fails the create
// over to another host on it); anything else keeps fallbackCode.
func capacityOrHTTPError(w http.ResponseWriter, fallbackCode int, err error) {
	if errors.Is(err, registry.ErrPoolExhausted) {
		w.Header().Set("Retry-After", "5")
		httpError(w, http.StatusServiceUnavailable, err)
		return
	}
	httpError(w, fallbackCode, err)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// maxNameLen bounds sandbox/snapshot display names.
const maxNameLen = 64

// validateName checks a display name ("" = unnamed, always valid): a short
// single-line label, not an identifier — any printable characters are fine.
func validateName(name string) error {
	if len(name) > maxNameLen {
		return fmt.Errorf("name exceeds %d bytes", maxNameLen)
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return errors.New("name must not contain control characters")
		}
	}
	return nil
}

// --- resource override validation ---

// minMemMIB is the smallest guest memory that reliably boots the devbox image;
// firecracker itself accepts less, but the kernel OOMs before sandboxd is up.
const minMemMIB = 128

// fcMaxVcpus is Firecracker's hard vCPU ceiling per microVM.
const fcMaxVcpus = 32

// guestSubnetBits is the prefix length shared by the gateway CIDR and every
// guest NIC. It gates the cold-boot GuestCIDR and the clone-path MMDS
// reidentify prefix; defaults to 24 when unset (single-/24 subnet).
func (s *Server) guestSubnetBits() int {
	if s.cfg.GuestSubnetBits <= 0 {
		return 24
	}
	return s.cfg.GuestSubnetBits
}

// fallbackMemCapMIB caps mem_mib when the host's total memory can't be read
// (non-Linux builds, tests).
const fallbackMemCapMIB = 64 * 1024

// validateResources bounds-checks per-sandbox vcpus/mem_mib overrides
// (0 = template default, always valid).
func (s *Server) validateResources(vcpus, memMIB int64) error {
	if vcpus < 0 {
		return errors.New("vcpus must be >= 0 (0 = template default)")
	}
	if memMIB < 0 {
		return errors.New("mem_mib must be >= 0 (0 = template default)")
	}
	if maxV := maxVcpus(); vcpus > maxV {
		return fmt.Errorf("vcpus %d exceeds host limit %d", vcpus, maxV)
	}
	if memMIB > 0 && memMIB < minMemMIB {
		return fmt.Errorf("mem_mib %d is below the minimum bootable %d", memMIB, minMemMIB)
	}
	if maxM := s.maxMemMIB(); memMIB > maxM {
		return fmt.Errorf("mem_mib %d exceeds host limit %d", memMIB, maxM)
	}
	return nil
}

// maxVcpus is the largest per-sandbox vCPU override: the host's core count,
// capped at Firecracker's per-VM maximum.
func maxVcpus() int64 {
	n := int64(runtime.NumCPU())
	if n > fcMaxVcpus {
		return fcMaxVcpus
	}
	return n
}

// maxMemMIB is the largest per-sandbox mem_mib override. With a memory budget
// configured it's the budget minus per-VM overhead — an override that can
// never be admitted 400s up front instead of burning gateway failover
// attempts + queue-wait before 503ing. (Caveat: on a heterogeneous fleet this
// 400 kills a create a bigger host could serve; fine while the MIG is
// uniform.) Without a budget it falls back to the host's total memory, which
// bounds a single sandbox only — the registry's admission check bounds the sum.
func (s *Server) maxMemMIB() int64 {
	if s.memBudgetMIB > 0 {
		return s.memBudgetMIB - fcOverheadMIB
	}
	if total := hostTotalMemMIB(); total > 0 {
		return total
	}
	return fallbackMemCapMIB
}

// hostTotalMemMIB reads MemTotal from /proc/meminfo; 0 when unreadable
// (non-Linux builds, tests). Note: inside a cgroup this is the MACHINE total,
// not the cgroup limit — which is why fleet hosts set mem_budget_mib explicitly.
func hostTotalMemMIB() int64 {
	b, err := os.ReadFile("/proc/meminfo")
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "MemTotal:") {
			continue
		}
		f := strings.Fields(line)
		if len(f) >= 2 {
			if kb, err := strconv.ParseInt(f[1], 10, 64); err == nil && kb > 0 {
				return kb / 1024
			}
		}
		break
	}
	return 0
}

// randomMAC returns a locally-administered unicast MAC.
func randomMAC() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	b[0] = (b[0] | 0x02) & 0xfe // locally administered, unicast
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5])
}
