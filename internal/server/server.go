package server

import (
	"context"
	"crypto/rand"
	"crypto/tls"
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

	"github.com/ayush6624/sandbox/internal/apiv1"
	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/httpapi"
	"github.com/ayush6624/sandbox/internal/management"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
	"github.com/ayush6624/sandbox/internal/wsutil"
)

// Config bundles everything the server needs at startup.
type Config struct {
	SocketPath          string
	ListenAddr          string
	ManagementTransport string
	TLSCertFile         string
	TLSKeyFile          string
	APIToken            string
	APITokens           []string
	APITokenFile        string
	WorkerToken         string
	WorkerTokens        []string
	WorkerTokenFile     string
	IngressDomain       string // public wildcard suffix; empty omits URL fields
	DefaultURLOnly      bool   // omitted POST /ports host_port defaults to false when set
	Provisioner         *provisioner.Provisioner
	GatewayIP           string // bridge IP; used as the guest's default gateway
	// GuestSubnetBits is the prefix length shared by the gateway and every
	// guest NIC (cold-boot GuestCIDR and the clone-path MMDS reidentify
	// prefix). Must match the gateway CIDR. <=0 falls back to 24.
	GuestSubnetBits int
	VMTemplate      vm.RunOptions // base options (firecracker bin, kernel, args, vcpus, mem, dns)
	HotCreate       bool          // maintain a golden snapshot and serve POST /sandboxes by cloning it
	// CreateConcurrency bounds concurrent sandbox bring-ups (cold boots and
	// golden clones); excess creates queue. <=0 = default: min(2×NumCPU, 16).
	CreateConcurrency int
	// WarmPoolSize keeps fully started, independently identified golden clones
	// off the routed inventory until a create atomically claims one.
	WarmPoolSize int
	// Accept-side data-plane fan-in caps shared by forwarded host ports and
	// CONNECT tunnels. 0 = default, negative = disabled. See connLimits in
	// portproxy.go and config.MaxPortConnsPerSandbox.
	MaxPortConnsPerSandbox int
	MaxPortConnsTotal      int
	PortConnRatePerSec     int
	// PlacementDelay suppresses advertised create capacity until Linux boot
	// age reaches this duration. Routing and heartbeat registration are not
	// delayed. See config.PlacementDelaySec.
	PlacementDelay time.Duration
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
	// config.UFFDChunkKiB and vm.RunOptions.UFFDChunkBytes. Also the chunk size
	// used for GCS chunk upload/fetch when UFFDChunkGCS is on (0 → 2 MiB default).
	UFFDChunkBytes uint64
	// UFFDChunkGCS backs UFFD faults with GCS-resident chunks (roadmap Phase B2):
	// full hibernation freezes upload chunks+manifest, same-identity wakes fault
	// lazily from local cache → GCS. Needs a snapshot bucket. See config.UFFDChunkGCS.
	UFFDChunkGCS bool
	// UFFDChunkPrefetch is the chunk-level fault-ahead window for the GCS source
	// (0 → 4). See config.UFFDChunkPrefetch.
	UFFDChunkPrefetch int
	// SnapshotBucket enables GCS snapshot durability: user snapshots upload
	// in the background and restore/fanout pull missing snapshots down from
	// the bucket, so any host can serve them. Empty = host-local only.
	SnapshotBucket string
	// UsageBucket receives the billing spool; defaults to SnapshotBucket.
	// Empty (with no SnapshotBucket) keeps the usage ledger host-local.
	UsageBucket string

	// --- Gateway registration (optional; Phase-1 multi-host) ---
	// When GatewayURL is set, the server periodically heartbeats to the gateway
	// so it can route requests to this host. Requires ListenAddr (the gateway
	// dials back over TCP using APIToken).
	GatewayURL       string // e.g. "http://100.x.y.z:9090"; empty disables registration
	GatewayToken     string // worker-control bearer presented to the gateway
	GatewayTokens    []string
	GatewayTokenFile string
	AdvertiseAddr    string // URL the gateway dials back; defaults from ListenAddr
	HostID           string // stable host identity; defaults to hostname
	WorkerRelease    string // deployed worker generation reported in heartbeats
}

// Server holds runtime state for the sandbox API.
type Server struct {
	cfg                Config
	reg                *registry.Registry
	machines           sync.Map        // map[string]*vm.Machine
	vmCtx              context.Context // long-lived; tied to Serve's ctx, NOT request ctx
	gatewayCredentials *management.Credentials
	workerCredentials  *management.Credentials

	// golden is the snapshot POST /sandboxes clones from when hot create is on.
	// nil until ensureGolden adopts or builds one; cleared if it's deleted.
	golden atomic.Pointer[registry.Snapshot]
	// stageLocks serialize every Firecracker load that needs the same baked
	// rootfs path. A snapshot fanout may temporarily stage and then unlink that
	// path; without this lock, concurrent fanout/restore requests can remove it
	// while a sibling VMM is still opening the drive.
	stageLocks keyedMutexes

	// blob is the GCS client for snapshot durability; nil when disabled.
	blob *gcsblob.Client
	// deleteObject overrides the durability store's object delete. Kept
	// injectable because blob is a concrete client, and the behaviour of the
	// hibernation invalidation paths when the object store is UNAVAILABLE is the
	// part that must be tested (see hib_durable.go: it must never gate a freeze,
	// a wake, or a destroy). nil in production.
	deleteObject func(ctx context.Context, object string) error
	// baseUpMu/basesUploaded gate the once-per-base template upload.
	baseUpMu      sync.Mutex
	basesUploaded map[string]bool
	// pulls serializes concurrent GCS pulls of the same snapshot id.
	pulls keyedMutexes
	// snapshotLocks serialize a snapshot's creation/upload, restore/fanout use,
	// and deletion. uploads are separately cancellable so delete never waits
	// for the full background timeout or lets a cancelled upload re-commit.
	snapshotLocks   keyedMutexes
	snapshotUpMu    sync.Mutex
	snapshotUploads map[string]*backgroundUpload
	// chunkUpMu/chunksUploaded remember content-addressed chunks this process has
	// already pushed, so re-hibernations skip re-uploading unchanged chunks
	// without an Exists round-trip each (roadmap Phase B2 dedup/CoW).
	chunkUpMu      sync.Mutex
	chunksUploaded map[string]bool

	// act tracks per-sandbox API activity for idle hibernation; wakesMu/wakes
	// serialize hibernate/wake/destroy per sandbox id.
	act   *activityTracker
	wakes keyedMutexes
	// Hibernation payloads upload after the VM is stopped. Wake/destroy cancel
	// and join the current upload before consuming or deleting its local files,
	// preventing late commit-marker resurrection and read-vs-unlink races.
	hibUpMu    sync.Mutex
	hibUploads map[string]*backgroundUpload

	// diffBase maps a live machine's sandbox id → the snapshot represented by
	// the baseline of its dirty-page bitmap. A successful diff snapshot becomes
	// the next baseline because Firecracker resets the bitmap after capture.
	// sb.BaseSnapshotID alone is NOT sufficient: it is never cleared and cannot
	// describe repeated snapshots or hibernation generations.
	diffBase sync.Map // sandbox id → snapshot id
	// hibLineage keeps the exact full-memory baseline loaded by a successful
	// diff hibernation wake. Hibernation has no public snapshot row, so this
	// private reflink is what lets the next snapshot (or hibernation) compose
	// Firecracker's new dirty layer back onto the immutable golden.
	hibLineage sync.Map // sandbox id → hibernationLineage

	// pf owns the userspace host-port → guest-port TCP proxies (see
	// portproxy.go). Its listeners persist through hibernation so a connection
	// to a frozen sandbox's port wakes it.
	pf *portForwarder

	// createSem bounds concurrent sandbox bring-ups (cold boots AND golden
	// clones) so a burst of creates queues instead of boot-storming the host —
	// the 60 s agent gate only starts ticking once a slot is acquired. Fanout
	// keeps its own separate budget.
	createSem chan struct{}
	warmOnce  sync.Once
	warmKick  chan struct{}
	// readyPoolSettled closes when the initial configured ready pool is full,
	// or after a bounded failure window. Heartbeats keep placement closed until
	// then so the first request does not race pool construction.
	readyPoolSettled     chan struct{}
	readyPoolSettledOnce sync.Once
	// warmed is closed once ensureGolden has settled (adopted, built, or
	// failed). Until then the heartbeat advertises SlotsFree=0 so the gateway
	// doesn't route a burst of guaranteed-cold creates at a host that's still
	// building its golden snapshot. Pre-closed when hot create is disabled.
	warmed chan struct{}

	// memBudgetMIB is the resolved committed-guest-memory ceiling (0 =
	// disabled). Mirrors what reg.SetMemAccounting was given; kept here so
	// validateResources/handleInfo can clamp the per-sandbox override too.
	memBudgetMIB int64

	// vmOverheadMIB is the per-VM memory admission charges on top of the
	// guest's mem_mib. It is NEVER below what the jailer's per-VM cgroup
	// allows (JailerConfig.MemoryOverheadMIB): if admission charged less, the
	// per-VM memory.max values at full admitted occupancy would sum above
	// mem_budget_mib and therefore above the parent task cgroup that also
	// contains serve — the parent OOMs and every VM on the host dies at once
	// instead of one create failing. vm.CheckMemoryAdmission re-proves this at
	// startup; see fcOverheadMIB for the floor.
	vmOverheadMIB int64

	// met holds process-lifetime lifecycle counters exposed on /metrics as
	// Prometheus counters (monotonic; reset only on a server restart).
	met serverMetrics

	// startedAt stamps process start so /metrics can export uptime.
	startedAt time.Time
	// bootAge reads Linux boot age (/proc/uptime, CLOCK_BOOTTIME semantics).
	// Kept injectable for deterministic placement-quarantine tests.
	bootAge func() (time.Duration, error)

	// finishCloneFn is finishClone, injectable so the fanout phase-2
	// concurrency bound is testable without a real VMM (bring-up needs Linux
	// and KVM). nil selects the real method.
	finishCloneFn func(context.Context, *clone) error

	// phases records the worker boot/readiness timeline (see bootphase.go) so
	// the autoscale "host becomes usable" span is attributable per stage
	// instead of one opaque block.
	phases *phaseRecorder

	// usagePut writes one immutable billing-spool object; nil disables
	// durability and keeps the ledger host-local (which also disables local
	// pruning — see pruneUsage). Injected as a function rather than a
	// *gcsblob.Client so the flush/dedup/retry behaviour is testable without a
	// bucket: that behaviour is what stands between a scaled-in worker and
	// unrecoverable revenue, so it must be exercised in unit tests.
	usagePut        func(ctx context.Context, object string, data []byte) error
	usageBucketName string

	// shuttingDown marks the shutdownAll drain. Billing reads it so a freeze
	// driven by SIGTERM is recorded as a platform event rather than an ordinary
	// idle hibernation — otherwise a fleet-wide roll looks, on an invoice, like
	// every customer's sandbox happening to go idle at the same instant.
	shuttingDown atomic.Bool
}

type backgroundUpload struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// serverMetrics are monotonic counts of lifecycle events, incremented at the
// single choke point for each transition. All access is via the atomics, so no
// lock is needed on the scrape path.
type serverMetrics struct {
	createsOK    atomic.Int64 // POST /sandboxes that returned 201 (hot clone or cold boot)
	createsErr   atomic.Int64 // POST /sandboxes that failed to bring a sandbox up (post-validation)
	hibernations atomic.Int64 // sandboxes frozen to disk (idle reaper, manual, or shutdown)
	wakes        atomic.Int64 // successful thaws from hibernation
	wakeFailures atomic.Int64 // wake attempts that rolled back to hibernated
	warmClaims   atomic.Int64 // creates served by a fully initialized ready VM
	warmMisses   atomic.Int64 // eligible creates that found the ready pool empty
	warmFailures atomic.Int64 // background ready-VM builds that failed
	// Billable totals, credited when an interval closes. Integer units, not
	// float: interval timestamps have one-second resolution and vcpus/mem_mib
	// are integers, so every billed quantity is exactly an integer and stays
	// that way — a float accumulator would make the cheapest possible number
	// (a sum of integers) the one place rounding could creep into a bill.
	billableVcpuSeconds   atomic.Int64 // allocated vCPU-seconds billed
	billableMemMIBSeconds atomic.Int64 // allocated MiB-seconds billed
	consumedCPUUsec       atomic.Int64 // host CPU consumed by guests (recorded, not billed)
}

// fcOverheadMIB is the FLOOR on per-VM memory charged on top of the guest's
// mem_mib: firecracker/VMM overhead. 1024 (template) + 156 = 1180, matching the
// MiB-per-slot arithmetic deploy-job.sh sizes the Nomad cgroup with. Under the
// jailer the effective charge is raised to the per-VM cgroup allowance
// (jailer_memory_overhead_mib) so the two can't drift — see vmOverheadMIB.
const fcOverheadMIB = 156

func New(cfg Config, reg *registry.Registry) *Server {
	s := &Server{cfg: cfg, reg: reg, basesUploaded: map[string]bool{},
		snapshotUploads: map[string]*backgroundUpload{},
		chunksUploaded:  map[string]bool{}, act: newActivityTracker(),
		hibUploads: map[string]*backgroundUpload{},
		startedAt:  time.Now(), bootAge: linuxBootAge, phases: newPhaseRecorder()}
	sem := cfg.CreateConcurrency
	if sem <= 0 {
		sem = 2 * runtime.NumCPU()
		if sem > 16 {
			sem = 16
		}
	}
	s.createSem = make(chan struct{}, sem)
	s.warmKick = make(chan struct{}, 1)
	s.readyPoolSettled = make(chan struct{})
	if cfg.WarmPoolSize <= 0 {
		close(s.readyPoolSettled)
	}
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
	// One per-VM overhead number, derived from the jailer's per-VM cgroup
	// allowance so admission can never charge less than the kernel will let a
	// VM use (that mismatch is what lets the per-VM leaves overcommit the
	// parent cgroup); never below the measured VMM floor.
	s.vmOverheadMIB = fcOverheadMIB
	if allowance := cfg.VMTemplate.Jailer.EffectiveMemoryOverheadMIB(); allowance > s.vmOverheadMIB {
		s.vmOverheadMIB = allowance
	}
	reg.SetMemAccounting(registry.MemAccounting{
		TemplateMemMIB: cfg.VMTemplate.MemMIB,
		BudgetMIB:      budget,
		OverheadMIB:    s.vmOverheadMIB,
	})
	if budget > 0 && budget < cfg.VMTemplate.MemMIB+s.vmOverheadMIB {
		fmt.Fprintf(os.Stderr, "WARNING: mem_budget_mib %d cannot fit even one template sandbox (%d+%d MiB) — every create (incl. the golden build) will be rejected\n",
			budget, cfg.VMTemplate.MemMIB, s.vmOverheadMIB)
	}

	s.pf = newPortForwarder(s.dialGuest, s.act.begin,
		newConnLimits(cfg.MaxPortConnsPerSandbox, cfg.MaxPortConnsTotal, cfg.PortConnRatePerSec))
	if cfg.SnapshotBucket != "" {
		s.blob = gcsblob.New(cfg.SnapshotBucket)
		fmt.Fprintf(os.Stderr, "snapshot durability on: gs://%s\n", cfg.SnapshotBucket)
	}
	// Usage spooling defaults to the snapshot bucket so fleets that already have
	// one get billing durability without new infrastructure, while a dedicated
	// bucket (its own retention, eventually write-only IAM) is one config line.
	if bucket := cfg.UsageBucket; bucket != "" || cfg.SnapshotBucket != "" {
		if bucket == "" {
			bucket = cfg.SnapshotBucket
		}
		client := s.blob
		if bucket != cfg.SnapshotBucket {
			client = gcsblob.New(bucket)
		}
		s.usageBucketName = bucket
		s.usagePut = client.PutBytes
		fmt.Fprintf(os.Stderr, "usage metering durability on: gs://%s/usage/\n", bucket)
	} else {
		fmt.Fprintf(os.Stderr, "usage metering is host-local: no usage_bucket or snapshot_bucket configured (ledger is never pruned)\n")
	}
	return s
}

// Serve listens on the configured Unix socket — and, if ListenAddr is set, on
// TCP with bearer-token auth — until ctx is cancelled. On shutdown, running
// sandboxes are hibernated (frozen to disk, wakeable on next start) rather
// than destroyed — see shutdownAll.
func (s *Server) Serve(ctx context.Context) error {
	// Fail closed before anything can be admitted: with the jailer on, the
	// per-VM cgroup allowances of a fully admitted host must fit inside the
	// parent task cgroup that also holds serve. If they don't, the failure is
	// the parent OOMing — every VM on the host at once — so refusing to start
	// is strictly better than serving. Checked here (not in New) because it
	// needs the RESOLVED budget.
	if jailer := s.cfg.VMTemplate.Jailer; jailer != nil {
		if err := vm.CheckMemoryAdmission(*jailer, vm.MemoryAdmission{
			BudgetMIB:          s.memBudgetMIB,
			ChargedOverheadMIB: s.vmOverheadMIB,
			TemplateMemMIB:     s.cfg.VMTemplate.MemMIB,
		}); err != nil {
			return fmt.Errorf("memory admission: %w", err)
		}
	}

	// vmCtx must NOT be the serve ctx: the firecracker SDK (and the raw clone
	// path's CommandContext) kill their VMs the moment their context cancels,
	// and the serve ctx cancels on SIGTERM — before shutdownAll gets a chance
	// to freeze anything. Decouple it and cancel explicitly on the way out as
	// the backstop for VMs that outlive shutdown.
	vmCtx, vmCancel := context.WithCancel(context.Background())
	defer vmCancel()
	s.vmCtx = vmCtx

	// Fold in the boot scripts' phase stamps + the kernel-boot anchor before
	// anything else, so the timeline is complete even if startup below fails.
	s.initBootPhases()

	s.reconcile(ctx)
	s.phases.mark(phaseReconcileDone)
	// Hibernated sandboxes survived reconcile; re-bind their port-forward
	// listeners or wake-on-connect breaks after a server restart.
	s.reopenPortListeners(ctx)
	go s.reapExpired(ctx)
	// Advances every open billable interval's heartbeat, bounding what a host
	// crash can lose to one tick.
	go s.usageSampler(ctx)
	if s.usagePut != nil {
		go s.usageSpooler(ctx)
	}
	if s.cfg.HotCreate {
		go s.ensureGolden(ctx)
	}
	if s.cfg.GatewayURL != "" {
		gatewayCreds, err := management.NewCredentials(
			append([]string{s.cfg.GatewayToken}, s.cfg.GatewayTokens...),
			s.cfg.GatewayTokenFile,
		)
		if err != nil {
			return fmt.Errorf("gateway worker-control credentials: %w", err)
		}
		s.gatewayCredentials = gatewayCreds
		if s.cfg.ManagementTransport != string(management.TransportDevelopment) &&
			!management.IsEncryptedOrPrivateEndpoint(s.cfg.GatewayURL) {
			return fmt.Errorf("gateway_url %q must use HTTPS or a verifiably private IP", s.cfg.GatewayURL)
		}
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
	if err := secureUnixSocket(s.cfg.SocketPath); err != nil {
		_ = ln.Close()
		return err
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /info", s.handleInfo)
	mux.HandleFunc("GET /metrics", s.handleMetrics)
	// Ledger reads. These answer from usage_intervals, which outlives the
	// sandboxes it bills, so neither is id-resolved against the sandbox table.
	mux.HandleFunc("GET /usage", s.handleUsage)
	mux.HandleFunc("GET /sandboxes/{id}/usage", s.handleSandboxUsage)
	mux.HandleFunc("POST /sandboxes", s.handleCreate)
	mux.HandleFunc("GET /sandboxes", s.handleList)
	mux.HandleFunc("GET /sandboxes/{id}", s.handleGet)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.handleDestroy)
	mux.HandleFunc("POST /sandboxes/{id}/timeout", s.handleSetTimeout)
	mux.HandleFunc("POST /sandboxes/{id}/rename", s.handleRename)
	mux.HandleFunc("POST /sandboxes/{id}/ports", s.handleExposePort)
	mux.HandleFunc("GET /sandboxes/{id}/ports", s.handleListPorts)
	mux.HandleFunc("DELETE /sandboxes/{id}/ports/{port}", s.handleDeletePort)
	mux.HandleFunc("PUT /sandboxes/{id}/ports/{port}/public", s.handleSetPublicPort)
	mux.HandleFunc("PUT /sandboxes/{id}/ssh-key", s.handleAuthorizeSSHKey)
	mux.HandleFunc("CONNECT /sandboxes/{id}/connect/{port}", s.handleConnectPort)
	mux.HandleFunc("GET /sandboxes/{id}/connect/{port}", s.handleConnectPortWebSocket)
	mux.HandleFunc("POST /sandboxes/{id}/exec", s.handleAgentProxy("exec"))
	mux.HandleFunc("POST /sandboxes/{id}/exec/stream", s.handleAgentProxy("exec/stream"))
	mux.HandleFunc("GET /sandboxes/{id}/files", s.handleAgentProxy("files"))
	mux.HandleFunc("PUT /sandboxes/{id}/files", s.handleAgentProxy("files"))
	mux.HandleFunc("GET /sandboxes/{id}/dir", s.handleAgentProxy("dir"))
	mux.HandleFunc("GET /sandboxes/{id}/shell", s.handleShellProxy())
	mux.HandleFunc("POST /sandboxes/{id}/snapshot", s.handleSnapshot)
	mux.HandleFunc("POST /sandboxes/{id}/hibernate", s.handleHibernate)
	mux.HandleFunc("POST /sandboxes/{id}/resume", s.handleResume)
	mux.HandleFunc("PATCH /sandboxes/{id}/public-fields", s.handlePublicFields)
	// Cross-host wake (roadmap B4): the gateway dispatches adopt (reconstruct +
	// wake here) on a route miss or a drain, and release (freeze + drop local,
	// keep GCS) on the drain source.
	mux.HandleFunc("POST /sandboxes/{id}/adopt", s.handleAdopt)
	mux.HandleFunc("POST /sandboxes/{id}/release", s.handleRelease)
	mux.HandleFunc("POST /internal/v1/sandboxes/{action}", s.handleInternalSandboxAction)
	mux.HandleFunc("GET /snapshots", s.handleListSnapshots)
	mux.HandleFunc("POST /snapshots/{id}/rename", s.handleRenameSnapshot)
	mux.HandleFunc("POST /snapshots/{id}/restore", s.handleRestore)
	mux.HandleFunc("POST /snapshots/{id}/fanout", s.handleFanout)
	mux.HandleFunc("PATCH /snapshots/{id}/public-fields", s.handleSnapshotPublicFields)
	mux.HandleFunc("DELETE /snapshots/{id}", s.handleDeleteSnapshot)
	apiv1.New(mux).Register(mux)

	publicHandler := httpapi.Middleware(mux)
	servers := []*http.Server{{Handler: publicHandler}}
	srvErr := make(chan error, 2)
	go func() { srvErr <- servers[0].Serve(ln) }()

	if s.cfg.ListenAddr != "" {
		transport := management.Transport{
			Mode:     management.TransportMode(s.cfg.ManagementTransport),
			CertFile: s.cfg.TLSCertFile,
			KeyFile:  s.cfg.TLSKeyFile,
		}
		if err := transport.ValidateListener(s.cfg.ListenAddr); err != nil {
			return err
		}
		clientCreds, err := optionalCredentials(
			append([]string{s.cfg.APIToken}, s.cfg.APITokens...),
			s.cfg.APITokenFile,
		)
		if err != nil {
			return fmt.Errorf("client API credentials: %w", err)
		}
		workerCreds, err := optionalCredentials(
			append([]string{s.cfg.WorkerToken}, s.cfg.WorkerTokens...),
			s.cfg.WorkerTokenFile,
		)
		if err != nil {
			return fmt.Errorf("gateway-to-worker credentials: %w", err)
		}
		if transport.Mode == management.TransportDevelopment && workerCreds == nil {
			workerCreds = clientCreds // legacy single-token compatibility is dev-only
		}
		if clientCreds == nil && workerCreds == nil {
			return errors.New("TCP listener requires client or worker credentials")
		}
		if s.cfg.GatewayURL != "" && workerCreds == nil {
			return errors.New("fleet worker requires a separate worker_token or worker_token_file")
		}
		if transport.Mode != management.TransportDevelopment &&
			clientCreds != nil && workerCreds != nil &&
			clientCreds.Overlaps(workerCreds) {
			return errors.New("worker_token must differ from api_token outside development mode")
		}
		s.workerCredentials = workerCreds
		tcpLn, err := net.Listen("tcp", s.cfg.ListenAddr)
		if err != nil {
			return fmt.Errorf("listen tcp %s: %w", s.cfg.ListenAddr, err)
		}
		tlsConfig, err := transport.TLSConfig()
		if err != nil {
			_ = tcpLn.Close()
			return err
		}
		if tlsConfig != nil {
			tcpLn = tls.NewListener(tcpLn, tlsConfig)
		}
		tcpSrv := &http.Server{
			Handler:   httpapi.Middleware(bearerAuth(clientCreds, workerCreds, mux)),
			TLSConfig: tlsConfig,
		}
		servers = append(servers, tcpSrv)
		go func() { srvErr <- tcpSrv.Serve(tcpLn) }()
		fmt.Fprintf(os.Stderr, "TCP API listening on %s (transport=%s, separated bearer auth)\n",
			s.cfg.ListenAddr, transport.Mode)
	}

	select {
	case <-ctx.Done():
		// Short drain: freezing sandboxes (below) matters more than letting
		// slow API requests finish — the whole stop window is ~120 s (Nomad
		// kill_timeout / GCE stop) and hibernation needs most of it.
		shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		for _, srv := range servers {
			if err := srv.Shutdown(shCtx); err != nil {
				// Shutdown's deadline does not close active connections. Force
				// them closed so their request contexts cancel and in-flight
				// starts unwind instead of racing the machine snapshot below.
				_ = srv.Close()
			}
		}
		cancel()
		s.shutdownAll()
		// shutdownAll froze every sandbox, which closed their intervals. This is
		// the most valuable flush of a worker's life — and on a scale-in that
		// deletes the instance, the only one that can still happen.
		s.drainUsage()
		s.pf.CloseAll() // hibernated sandboxes' listeners; reopened next startup
		return nil
	case err := <-srvErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

func (s *Server) handleInternalSandboxAction(w http.ResponseWriter, r *http.Request) {
	id, action, ok := strings.Cut(r.PathValue("action"), ":")
	if !ok || id == "" {
		http.NotFound(w, r)
		return
	}
	r.SetPathValue("id", id)
	switch action {
	case "adopt":
		s.handleAdopt(w, r)
	case "release":
		s.handleRelease(w, r)
	default:
		http.NotFound(w, r)
	}
}

// bearerAuth keeps client and worker trust domains separate. Internal control
// routes require the worker credential. Public routes accept either a direct
// client credential or the worker credential used by the gateway proxy.
// WebSocket query credentials are deliberately not accepted; upgrade requests
// authenticate via the Sec-WebSocket-Protocol bearer entry instead (see
// wsutil.UpgradeAuthorization), which browsers can set and logs don't capture.
func bearerAuth(clientCreds, workerCreds *management.Credentials, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		internal := strings.HasPrefix(r.URL.Path, "/internal/v1/") ||
			strings.HasSuffix(r.URL.Path, "/adopt") ||
			strings.HasSuffix(r.URL.Path, "/release")
		// Subprotocol credentials reach public routes only; an internal control
		// route must never be authenticable by a channel a browser can drive.
		if auth == "" && !internal {
			auth = wsutil.UpgradeAuthorization(r)
		}
		workerMatch := workerCreds != nil && workerCreds.MatchAuthorization(auth)
		clientMatch := clientCreds != nil && clientCreds.MatchAuthorization(auth)
		// A token present in both independently rotatable domains is never
		// allowed to cross the internal-control boundary.
		ok := workerMatch && (!internal || !clientMatch)
		if !internal && !ok && clientCreds != nil {
			ok = clientMatch
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

func optionalCredentials(tokens []string, file string) (*management.Credentials, error) {
	hasToken := false
	for _, token := range tokens {
		if strings.TrimSpace(token) != "" {
			hasToken = true
			break
		}
	}
	if !hasToken && strings.TrimSpace(file) == "" {
		return nil, nil
	}
	return management.NewCredentials(tokens, file)
}

func secureUnixSocket(path string) error {
	if os.Geteuid() != 0 {
		return errors.New("sandbox management Unix socket requires root ownership")
	}
	if err := os.Chown(path, 0, 0); err != nil {
		return fmt.Errorf("chown management Unix socket: %w", err)
	}
	if err := os.Chmod(path, 0o600); err != nil {
		return fmt.Errorf("chmod management Unix socket: %w", err)
	}
	return nil
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
	// Every interval closed from here on is a platform event, not a customer
	// one: a fleet roll must not read on an invoice as every sandbox happening
	// to go idle at the same instant.
	s.shuttingDown.Store(true)

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
			if sb, err := s.reg.Get(ctx, id); err == nil &&
				(sb.Status == registry.StatusPreparing || sb.Status == registry.StatusWarming ||
					sb.Status == registry.StatusStarting || sb.Status == registry.StatusStopping) {
				if err := s.destroy(context.Background(), id); err != nil {
					fmt.Fprintf(os.Stderr, "[%s] shutdown destroy warm sandbox: %v\n", id, err)
				}
				return
			}
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

	// sync.Map.Range is explicitly not a point-in-time snapshot. A bring-up
	// that was already inside an HTTP handler when draining began can publish
	// its Machine just after the range passed its key. Reconcile the registry
	// once more so no starting/running row or late VMM escapes shutdown.
	rows, err := s.reg.All(context.Background())
	if err != nil {
		fmt.Fprintf(os.Stderr, "shutdown: list stragglers: %v\n", err)
		return
	}
	for _, sb := range rows {
		switch sb.Status {
		case registry.StatusHibernated:
			continue
		case registry.StatusRunning:
			if _, ok := s.machines.Load(sb.ID); ok {
				if err := s.hibernate(ctx, sb.ID, true); err == nil {
					continue
				}
			}
		}
		if err := s.destroy(context.Background(), sb.ID); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] shutdown destroy straggler: %v\n", sb.ID, err)
		}
	}
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
		SSHPubkey         string `json:"ssh_pubkey"`
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
	if err := validateSSHPubkey(body.SSHPubkey); err != nil {
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
	var sb registry.Sandbox
	hot := false
	if body.Vcpus == 0 && body.MemMIB == 0 {
		if ready, ok := s.claimWarm(ctx, body.Name, expiresAt, body.HibernateAfterSec); ok {
			sb, hot = ready, true
		}
	}
	if snap := s.golden.Load(); snap != nil && body.Vcpus == 0 && body.MemMIB == 0 {
		if !hot {
			s2, err := s.createFromSnapshot(ctx, *snap, body.Name, expiresAt, body.HibernateAfterSec)
			if err == nil {
				sb, hot = s2, true
			} else {
				fmt.Fprintf(os.Stderr, "hot create from golden snapshot %s failed, cold-booting instead: %v\n", snap.ID, err)
			}
		}
	}
	if !hot {
		s2, err := s.createCold(ctx, body.Name, expiresAt, body.HibernateAfterSec, body.Vcpus, body.MemMIB)
		if err != nil {
			s.met.createsErr.Add(1)
			capacityOrHTTPError(w, 500, err)
			return
		}
		sb = s2
	}

	// Install the SSH key (both paths return with the agent health-gated). If the
	// user asked for SSH access and we can't provision it, the sandbox can't serve
	// its purpose — destroy it and fail the create rather than hand back a
	// half-provisioned box. The key lands in the rootfs, so it needs no re-push on
	// a later hibernation wake.
	if body.SSHPubkey != "" {
		if err := installSSHKey(ctx, sb.GuestIP, body.SSHPubkey); err != nil {
			_ = s.destroy(context.Background(), sb.ID)
			s.met.createsErr.Add(1)
			httpError(w, 500, fmt.Errorf("sandbox created but ssh key install failed: %w", err))
			return
		}
	}

	s.met.createsOK.Add(1)
	writeJSON(w, 201, s.effectiveResources(sb))
}

// sshKeyPrefixes are the authorized_keys key-type tokens we accept.
const sshPort = 22

var sshKeyPrefixes = []string{
	"ssh-ed25519 ", "ssh-rsa ", "ssh-dss ", "ecdsa-sha2-",
	"sk-ecdsa-sha2-", "sk-ssh-ed25519@openssh.com ",
}

// validateSSHPubkey checks an optional POST /sandboxes ssh_pubkey: empty is
// fine (no SSH), otherwise it must be a single authorized_keys line with a
// recognized key type. This is a well-formedness check, not a security boundary
// — it's the user's own key for their own sandbox — but rejecting embedded
// newlines stops a multi-line value from smuggling extra authorized_keys
// entries or sshd options.
func validateSSHPubkey(key string) error {
	if key == "" {
		return nil
	}
	if strings.ContainsAny(key, "\r\n") {
		return errors.New("ssh_pubkey must be a single line")
	}
	if len(key) > 8<<10 {
		return errors.New("ssh_pubkey too long")
	}
	for _, p := range sshKeyPrefixes {
		if strings.HasPrefix(key, p) {
			return nil
		}
	}
	return errors.New("ssh_pubkey must be an OpenSSH public key (ssh-ed25519, ssh-rsa, ecdsa-sha2-*, …)")
}

// handleAuthorizeSSHKey installs a key on an existing sandbox for the CLI SSH
// flow. The request pins and wakes a hibernated sandbox before calling the same
// guest operation used during creation.
func (s *Server) handleAuthorizeSSHKey(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PublicKey string `json:"public_key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	body.PublicKey = strings.TrimSpace(body.PublicKey)
	if body.PublicKey == "" {
		httpError(w, http.StatusBadRequest, errors.New("public_key is required"))
		return
	}
	if err := validateSSHPubkey(body.PublicKey); err != nil {
		httpError(w, http.StatusBadRequest, err)
		return
	}
	id := r.PathValue("id")
	if _, err := s.reg.Get(r.Context(), id); err != nil {
		capacityOrHTTPError(w, statusFor(err), err)
		return
	}
	done := s.act.begin(id)
	defer done()
	sb, err := s.ensureRunning(r.Context(), id)
	if err != nil {
		capacityOrHTTPError(w, statusFor(err), err)
		return
	}
	if err := installSSHKey(r.Context(), sb.GuestIP, body.PublicKey); err != nil {
		httpError(w, http.StatusBadGateway, err)
		return
	}
	// CONNECT is authorized by the same registry row as every other forwarded
	// stream. This row intentionally has no host port and needs no ingress
	// domain: it is a tunnel permission consumed only through the authenticated
	// API, not a public URL allocation.
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	defer lifecycle.Unlock()
	if _, err := s.reg.Get(r.Context(), id); err != nil {
		capacityOrHTTPError(w, statusFor(err), err)
		return
	}
	if _, err := s.reg.AddURLPort(r.Context(), id, sshPort); err != nil {
		httpError(w, http.StatusInternalServerError, fmt.Errorf("authorize SSH tunnel: %w", err))
		return
	}
	w.WriteHeader(http.StatusNoContent)
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

// tryAcquireCreate takes one bring-up slot only if one is free right now. A
// fanout uses it to widen its parallelism beyond the one permit it waited for:
// blocking for further permits while already holding some would let two
// concurrent fanouts split a small budget and deadlock each other.
func (s *Server) tryAcquireCreate() bool {
	select {
	case s.createSem <- struct{}{}:
		return true
	default:
		return false
	}
}

// createCold boots a brand-new sandbox from the base rootfs: full rootfs copy,
// kernel boot, and agent startup. It blocks until the in-guest agent answers,
// so callers can exec/write files the moment it returns. vcpus/memMIB override
// the template's resources when nonzero (already validated by the caller).
func (s *Server) createCold(ctx context.Context, name string, expiresAt *time.Time, hibernateAfterSec int, vcpus, memMIB int64) (registry.Sandbox, error) {
	id := uuid.NewString()
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	defer lifecycle.Unlock()
	rootfsPath := s.cfg.Provisioner.RootfsPathFor(id)

	// Allocate identity + admission BEFORE the rootfs copy: a capacity-rejected
	// create (pool/memory exhaustion — routine under gateway failover) must not
	// pay a multi-GB copy + cleanup on a host that's already full.
	sb, err := s.reg.CreateStarting(ctx, id, name, rootfsPath, expiresAt, "", hibernateAfterSec, vcpus, memMIB)
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

	if err := s.reg.FinishStart(ctx, id, pid, rt.VMID, rt.SocketPath); err != nil {
		s.pf.CloseSandbox(id)
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		return registry.Sandbox{}, fmt.Errorf("finish start: %w", err)
	}

	s.machines.Store(id, m)
	s.act.touch(id)

	s.watchMachine(id, m, "VM")

	if err := waitForAgent(ctx, sb.GuestIP, 60*time.Second); err != nil {
		_ = s.destroyLocked(context.Background(), id)
		return registry.Sandbox{}, fmt.Errorf("sandbox booted but agent never became ready: %w", err)
	}
	if err := initializeGuestIdentity(ctx, sb.GuestIP, id); err != nil {
		_ = s.destroyLocked(context.Background(), id)
		return registry.Sandbox{}, fmt.Errorf("sandbox booted but identity initialization failed: %w", err)
	}
	if err := s.reg.MarkRunning(ctx, id); err != nil {
		_ = s.destroyLocked(context.Background(), id)
		return registry.Sandbox{}, fmt.Errorf("publish running sandbox: %w", err)
	}

	sb.PID = pid
	sb.VMID = rt.VMID
	sb.SocketPath = rt.SocketPath
	sb.Status = registry.StatusRunning
	// Billing starts HERE, not at create acceptance: everything above — the
	// clone or cold boot, the agent gate, identity rotation — is our latency.
	s.meterStart(ctx, sb)
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
		sandboxes[i] = s.withIngress(r.Context(), s.effectiveResources(sandboxes[i]))
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
	writeJSON(w, 200, s.withIngress(r.Context(), s.effectiveResources(sb)))
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
	// HotCreate reports whether POST /sandboxes is served from a golden snapshot.
	HotCreate bool `json:"hot_create"`
	// WarmPoolSize is the configured number of hidden, fully initialized VMs.
	WarmPoolSize int `json:"warm_pool_size"`
	// HibernateAfterSec is the host's default idle-hibernation window (0 = off).
	HibernateAfterSec int `json:"hibernate_after_sec"`
	// HostID identifies this host in fleet mode; empty standalone.
	HostID string `json:"host_id,omitempty"`
}

func (s *Server) handleInfo(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, Info{
		DefaultVcpus:      s.cfg.VMTemplate.Vcpus,
		DefaultMemMIB:     s.cfg.VMTemplate.MemMIB,
		MaxVcpus:          maxVcpus(),
		MaxMemMIB:         s.maxMemMIB(),
		HotCreate:         s.cfg.HotCreate,
		WarmPoolSize:      s.cfg.WarmPoolSize,
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

func (s *Server) withIngress(ctx context.Context, sb registry.Sandbox) registry.Sandbox {
	ports, err := s.reg.Ports(ctx, sb.ID)
	if err != nil {
		return sb
	}
	s.decoratePorts(sb.ID, ports)
	sb.Ports = ports
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
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	defer lifecycle.Unlock()
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
	op := s.snapshotLock(id)
	op.Lock()
	defer op.Unlock()
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

// handleResume is the explicit lifecycle seam used by v1. Legacy clients
// retain implicit wake-on-use behavior.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if _, err := s.reg.Get(r.Context(), id); err != nil {
		httpError(w, statusFor(err), err)
		return
	}
	done := s.act.begin(id)
	defer done()
	sb, err := s.ensureRunning(r.Context(), id)
	if err != nil {
		httpError(w, statusFor(err), err)
		return
	}
	writeJSON(w, http.StatusOK, s.effectiveResources(sb))
}

// handlePublicFields persists descriptive state owned by the v1 adapter.
func (s *Server) handlePublicFields(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var body struct {
		Name               *string            `json:"name"`
		Metadata           *map[string]string `json:"metadata"`
		SourceType         *string            `json:"source_type"`
		SourceID           *string            `json:"source_id"`
		TTLSeconds         *int               `json:"ttl_seconds"`
		IdleTimeoutSeconds *int               `json:"idle_timeout_seconds"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	lifecycle := s.wakeLock(id)
	lifecycle.Lock()
	defer lifecycle.Unlock()
	current, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, http.StatusNotFound, err)
		return
	}
	name, metadata := current.Name, current.Metadata
	sourceType, sourceID := current.SourceType, current.SourceID
	idle, expiresAt := current.HibernateAfterSec, current.ExpiresAt
	if body.Name != nil {
		name = *body.Name
	}
	if body.Metadata != nil {
		metadata = *body.Metadata
	}
	if body.SourceType != nil {
		sourceType = *body.SourceType
	}
	if body.SourceID != nil {
		sourceID = *body.SourceID
	}
	if body.IdleTimeoutSeconds != nil {
		idle = *body.IdleTimeoutSeconds
	}
	if body.TTLSeconds != nil {
		expiresAt = nil
		if *body.TTLSeconds > 0 {
			expiry := time.Now().Add(time.Duration(*body.TTLSeconds) * time.Second)
			expiresAt = &expiry
		}
	}
	if err := validateName(name); err != nil {
		httpError(w, 400, err)
		return
	}
	if idle < 0 {
		httpError(w, 400, errors.New("idle_timeout_seconds must be non-negative"))
		return
	}
	if len(metadata) > 64 {
		httpError(w, 400, errors.New("metadata must contain at most 64 entries"))
		return
	}
	updated, err := s.reg.UpdatePublicFields(r.Context(), current.ID, name, metadata, expiresAt, idle)
	if err == nil && (sourceType != current.SourceType || sourceID != current.SourceID) {
		updated, err = s.reg.SetPublicFields(r.Context(), current.ID, sourceType, sourceID, metadata)
	}
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, 200, s.effectiveResources(updated))
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
	return s.destroyLocked(ctx, id)
}

// destroyLocked tears down id while wakeLock(id) is held.
func (s *Server) destroyLocked(ctx context.Context, id string) error {
	defer s.act.forget(id)
	defer s.diffBase.Delete(id)
	defer s.clearHibernationLineage(id)

	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get sandbox: %w", err)
	}
	// Stop the durability writer before anything else: it must not publish a
	// commit marker for a sandbox that is being torn down.
	s.cancelHibernationUpload(id)
	// Un-route the sandbox BEFORE invalidating its durable generation. The
	// reverse order cost us the recovery path on every failure: MarkStopping can
	// legitimately fail (a status a sandbox can't stop from, a registry error),
	// and having already deleted record.json left a local-only sandbox that the
	// caller was then told it could not delete.
	if err := s.reg.MarkStopping(ctx, id); err != nil {
		return fmt.Errorf("mark stopping: %w", err)
	}
	// A durable generation must not outlive an explicit destroy. Best-effort in
	// the object store, authoritative locally — see invalidateHibernationRecord.
	if err := s.invalidateHibernationRecord(ctx, id); err != nil {
		return err
	}

	// Read port mappings before reg.Destroy deletes their rows.
	ports, err := s.reg.Ports(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] list ports: %v\n", id, err)
	}

	if v, ok := s.machines.Load(id); ok {
		m := v.(*vm.Machine)
		// Close the interval before the VMM exits and takes its cgroup leaf —
		// and therefore its consumed-CPU total — with it. The few hundred ms of
		// teardown CPU this misses is not worth a hook inside vm's cleanup.
		s.meterStop(ctx, id, registry.EndDestroy)
		if err := stopMachineBounded(m); err != nil {
			return fmt.Errorf("stop VM: %w", err)
		}
		s.machines.Delete(id)
	} else {
		// No live VM (a hibernated sandbox being deleted, or one whose VMM
		// already died). Any open interval is closed defensively; for a
		// hibernated row the freeze already closed it and this is a no-op.
		s.meterStop(ctx, id, registry.EndDestroy)
	}

	s.pf.CloseSandbox(id)
	// Legacy DNAT cleanup: port forwarding is a userspace proxy now, but hosts
	// upgrading from the DNAT scheme may still carry rules for this sandbox.
	// Removing a nonexistent rule is harmless. A hibernated row holds no
	// identity, so there is nothing to clean up — and nothing to get wrong:
	// deleting the tap named by a hibernated row USED to mean deleting whichever
	// running sandbox had since been handed that tap.
	if sb.GuestIP != "" {
		for _, pm := range ports {
			if pm.HostPort != 0 {
				s.cfg.Provisioner.RemovePortForwardTo(pm.HostPort, sb.GuestIP, pm.GuestPort)
			}
		}
	}
	if sb.TapDevice != "" {
		_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	}
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(id))
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	if err := s.reg.Destroy(ctx, id); err != nil {
		return err
	}
	go s.deleteHibernationObjects(id)
	return nil
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
		return s.memBudgetMIB - s.vmOverheadMIB
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
