package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ayush6624/sandbox/internal/registry"
)

// Config is the on-disk JSON describing the host's sandbox runtime.
type Config struct {
	// --- API ---
	SocketPath          string   `json:"socket_path"`          // root-owned unix socket the server listens on
	ListenAddr          string   `json:"listen_addr"`          // optional TCP listener
	ManagementTransport string   `json:"management_transport"` // tls, private_proxy, or explicit development
	TLSCertFile         string   `json:"tls_cert_file"`
	TLSKeyFile          string   `json:"tls_key_file"`
	APIToken            string   `json:"api_token"`      // legacy single client credential
	APITokens           []string `json:"api_tokens"`     // active client credentials; first is preferred
	APITokenFile        string   `json:"api_token_file"` // newline-delimited, atomically reloadable
	WorkerToken         string   `json:"worker_token"`   // gateway-to-worker callback credential
	WorkerTokens        []string `json:"worker_tokens"`
	WorkerTokenFile     string   `json:"worker_token_file"`
	// IngressDomain decorates exposed ports with
	// https://<guest-port>-<sandbox-id>.<domain>. Empty disables public URLs.
	IngressDomain string `json:"ingress_domain"`
	// DefaultURLOnly makes an omitted POST /ports host_port flag consume no
	// worker host port. False preserves the pre-ingress API default.
	DefaultURLOnly bool `json:"default_url_only"`

	// --- Gateway registration (optional; multi-host) ---
	GatewayURL              string   `json:"gateway_url"`           // register/heartbeat target; requires listen_addr
	GatewayToken            string   `json:"gateway_token"`         // legacy shared token; development only
	GatewayAPIToken         string   `json:"gateway_api_token"`     // optional CLI client credential
	GatewayControlToken     string   `json:"gateway_control_token"` // worker-to-gateway credential
	GatewayControlTokens    []string `json:"gateway_control_tokens"`
	GatewayControlTokenFile string   `json:"gateway_control_token_file"`
	AdvertiseAddr           string   `json:"advertise_addr"` // URL gateway dials; defaults from listener transport
	HostID                  string   `json:"host_id"`
	WorkerRelease           string   `json:"worker_release"`

	// --- Storage ---
	DBPath      string `json:"db_path"`      // SQLite registry
	RootfsBase  string `json:"rootfs_base"`  // immutable base rootfs image
	RootfsDir   string `json:"rootfs_dir"`   // per-sandbox rootfs copies live here
	SnapshotDir string `json:"snapshot_dir"` // per-snapshot artifacts (mem/state/rootfs) live here
	// SnapshotBucket is a GCS bucket that makes user snapshots durable and
	// restorable on any host: snapshots upload in the background after
	// creation, and a restore/fanout on a host that lacks the snapshot pulls
	// it down. Requires a service account with storage access on the VM
	// (metadata-server auth). Empty disables all GCS behavior — snapshots
	// stay host-local exactly as before.
	SnapshotBucket string `json:"snapshot_bucket"`
	// UsageBucket is where closed billable intervals are spooled as immutable
	// newline-JSON (see docs/usage-metering-plan.md). It defaults to
	// SnapshotBucket, which needs no new infrastructure — but it is a separate
	// knob because usage objects are billing evidence: they want their own
	// retention policy and, eventually, write-only IAM, neither of which suits
	// a bucket full of mutable snapshot artifacts.
	//
	// Empty (and no SnapshotBucket) keeps the ledger host-local. That is the
	// correct self-hosted default, and it is also why local pruning is gated on
	// a bucket being configured: with nowhere to spool to, the local rows ARE
	// the record and must not be deleted.
	UsageBucket string `json:"usage_bucket"`

	// --- Networking ---
	Bridge      string `json:"bridge"`      // e.g. "br-fc"
	GatewayIP   string `json:"gateway_ip"`  // bridge IP, used as guest default gateway
	Nameservers string `json:"nameservers"` // comma-separated DNS for the guest
	// GuestSubnetBits is the prefix length of the guest subnet shared by the
	// bridge (gateway) and every guest NIC. It caps how many sandboxes can run
	// concurrently on a host: a /24 holds ~253 usable IPs, a /22 ~1021, a /20
	// ~4093. Widen it (and the guest-IP pool) to run more than ~250 small
	// sandboxes at once. Must be the same on the gateway CIDR and the guest
	// CIDR or guests can't route to the gateway. 0 = default 24.
	GuestSubnetBits int `json:"guest_subnet_bits"`
	// AllowInterGuestNetwork permits direct traffic between sandbox tap devices
	// on the host bridge. It is off by default: production sandboxes are tenant
	// isolation boundaries and must not be able to address each other merely
	// because they share a worker. Enable only for trusted, explicitly
	// multi-service workloads.
	AllowInterGuestNetwork bool `json:"allow_inter_guest_network"`

	// --- Behavior ---
	// Hot create is on by default: the server maintains a golden snapshot of a
	// pristine booted sandbox and serves POST /sandboxes by cloning it (fan-out
	// mechanism), falling back to cold boot. Set true to always cold-boot.
	DisableHotCreate bool `json:"disable_hot_create"`
	// HibernateAfterSec freezes a sandbox idle this many seconds to disk
	// (memory snapshot + kill), releasing its slot/tap/IP; explicitly exposed
	// host ports remain reserved so they can wake the sandbox. Observable
	// external interaction counts as activity: agent API requests, shells, and
	// forwarded-port connections. Guest processes, CPU/I/O, and outbound
	// traffic are deliberately not inspected. 0 disables the host default;
	// shipped configs use 600 seconds.
	HibernateAfterSec int `json:"hibernate_after_sec"`
	// CreateConcurrency bounds concurrent sandbox bring-ups (cold boots and
	// golden clones); excess creates queue in-process so a burst can't
	// boot-storm the host into agent timeouts. 0 = server default
	// (min(2×NumCPU, 16)).
	CreateConcurrency int `json:"create_concurrency"`
	// WarmPoolSize keeps this many fully isolated golden clones ready to claim.
	// They consume normal VM capacity; 0 disables prewarming.
	WarmPoolSize int `json:"warm_pool_size"`
	// WarmPoolBudget caps aggregate ready and preparing VMs across all
	// templates. 0 inherits warm_pool_size for backward compatibility.
	WarmPoolBudget int `json:"warm_pool_budget"`
	// MetricsIntervalSec samples each running sandbox's CPU/memory/network/disk
	// from the host (cgroup leaf, tap counters, rootfs blocks) for
	// GET /sandboxes/{id}/metrics and the /metrics aggregates. It never
	// contacts the guest, so it neither counts as activity nor wakes a
	// hibernated sandbox. 0 = default 5 seconds; negative disables sampling.
	// MetricsHistory bounds the retained per-sandbox ring (0 = 360 samples,
	// i.e. 30 minutes at the default interval).
	MetricsIntervalSec int `json:"metrics_interval_sec"`
	MetricsHistory     int `json:"metrics_history"`
	// MetricsGuestStats additionally polls each running guest's sandboxd for
	// the two things the host cannot see from outside the VM: memory actually
	// in use (the hypervisor's charge only ever grows) and free disk. The poll
	// runs on every sampling tick and is deliberately routed around the
	// activity tracker, so it neither delays idle hibernation nor wakes a
	// frozen sandbox. Watch sandbox_guest_stat_failures_total: a guest whose
	// agent predates GET /stats degrades to host-only fields silently. Needs an agent with GET /stats — the agent is
	// image-pinned, so a fleet must rebake before enabling this.
	MetricsGuestStats bool `json:"metrics_guest_stats"`
	// Data-plane fan-in caps. They bound forwarded-port accepts and CONNECT
	// tunnels together, so a sandbox's budget is the same however traffic
	// reaches it. Each connection costs a goroutine, a dial budget, an activity
	// pin (which suppresses hibernation) and registry reads, so an unbounded
	// data plane is a way to starve creates on the same worker. Defaults are far
	// above any well-behaved workload; 0 = default, negative = disabled.
	MaxPortConnsPerSandbox int `json:"max_port_conns_per_sandbox"` // 0 = 256
	MaxPortConnsTotal      int `json:"max_port_conns_total"`       // 0 = 4096, host-wide
	// PortConnRatePerSec limits how fast NEW connections may be accepted for one
	// sandbox (burst = 2×). It is the control that matters against a connect
	// flood, since short-lived connections never accumulate against the
	// concurrency caps. 0 = 200.
	PortConnRatePerSec int `json:"port_conn_rate_per_sec"`
	// PlacementDelaySec keeps a freshly booted worker routable but advertises
	// zero create capacity until Linux boot age reaches this threshold. Fleet
	// deployments set it beyond the MIG standby initial delay so refill VMs
	// are suspended before they can receive sandboxes. /proc/uptime includes
	// suspended time, so a resumed suspended standby is immediately eligible.
	// 0 disables the gate.
	PlacementDelaySec int `json:"placement_delay_sec"`
	// UFFDRestore makes same-identity hibernation wakes restore the guest with
	// Firecracker's userfaultfd memory backend: the guest resumes before its
	// RAM is paged in and faults its working set from the mem file on demand,
	// cutting wake latency (and wake I/O) roughly to the working set instead of
	// the whole guest. Off = the eager File backend (whole-RAM fault-in before
	// resume). Only the same-identity restore path is UFFD-backed; the
	// clone-path wake still uses File. See docs/uffd-roadmap.md.
	UFFDRestore bool `json:"uffd_restore"`
	// UFFDChunkKiB selects the UFFD page source when UFFDRestore is on: 0 (default)
	// serves faults from a whole-file mmap of the mem image; >0 reads the mem file
	// in fixed chunks of this many KiB on demand, through a chunk cache. Behavior
	// is identical either way — this is the chunk-indexing/cache path a remote
	// (GCS) memory source will reuse for cross-host wake (roadmap Phase B). Rounded
	// down to a 4 KiB multiple, floored at one page. Typical: 1024 or 2048.
	UFFDChunkKiB int `json:"uffd_chunk_kib"`
	// UFFDChunkGCS turns the chunk source into a GCS-backed remote memory source
	// (roadmap Phase B2), requires a snapshot bucket and UFFDRestore. When on: a
	// FULL hibernation freeze uploads its mem image as content-addressed,
	// gzip-compressed chunks + a manifest to the bucket (dedup: unchanged chunks
	// are skipped), and a same-identity wake faults pages lazily from a local
	// chunk cache → GCS instead of the local mem file — so wake I/O tracks the
	// working set, not the whole guest, and works even off the creating host. Off
	// = local mem file (whole-file or local-chunk per UFFDChunkKiB). Diff freezes
	// are not chunk-uploaded (they hold only dirty pages); those wake locally.
	UFFDChunkGCS bool `json:"uffd_chunk_gcs"`
	// UFFDChunkPrefetch is the chunk-level fault-ahead window for the GCS source:
	// on a fault it kicks off background fetches of the next N chunks to hide the
	// per-chunk RTT behind sequential access. 0 = default (4). Ignored unless
	// UFFDChunkGCS is on.
	UFFDChunkPrefetch int `json:"uffd_chunk_prefetch"`
	// MemBudgetMIB caps the SUM of committed guest memory (each running
	// sandbox's effective mem_mib + per-VM firecracker overhead) so mem_mib
	// overrides can't oversubscribe the host past its cgroup/RAM — admission
	// beyond it returns 503 and the gateway places elsewhere. 0 = derive from
	// /proc/meminfo (MemTotal − 2 GiB host reserve); <0 = disabled. Fleet
	// deployments must set it explicitly (deploy-job.sh injects SLOTS×1180,
	// the Nomad cgroup minus serve's own reserve) because /proc/meminfo shows
	// the machine total, not the cgroup limit.
	MemBudgetMIB int64 `json:"mem_budget_mib"`

	// --- Resource pools ---
	Pools registry.Pools `json:"pools"`

	// --- VM template ---
	FirecrackerBin string `json:"firecracker_bin"`
	// VMIsolation selects the VMM host boundary. "jailer" is required for
	// production; "direct" is an explicit development-only escape hatch.
	VMIsolation             string `json:"vm_isolation"`
	JailerBin               string `json:"jailer_bin"`
	JailerChrootBase        string `json:"jailer_chroot_base"`
	JailerUIDStart          int    `json:"jailer_uid_start"`
	JailerGIDStart          int    `json:"jailer_gid_start"`
	JailerIdentityCount     int    `json:"jailer_identity_count"`
	JailerMemoryOverheadMIB int64  `json:"jailer_memory_overhead_mib"`
	JailerPIDsMax           int64  `json:"jailer_pids_max"`
	JailerCPUWeight         int64  `json:"jailer_cpu_weight"`
	JailerCPUPeriodUS       int64  `json:"jailer_cpu_period_us"`
	JailerIOReadBPS         int64  `json:"jailer_io_read_bps"`
	JailerIOWriteBPS        int64  `json:"jailer_io_write_bps"`
	JailerNoFile            uint64 `json:"jailer_no_file"`
	JailerFileSize          uint64 `json:"jailer_file_size"`
	KernelImage             string `json:"kernel_image"`
	KernelArgs              string `json:"kernel_args"`
	Vcpus                   int64  `json:"vcpus"`
	MemMIB                  int64  `json:"mem_mib"`
	// DisableSeccomp is a development-only escape hatch. Firecracker's built-in
	// restrictive seccomp filters are enabled by default on every launch path.
	DisableSeccomp bool `json:"disable_seccomp"`
	// FirecrackerLogMaxBytes caps each VMM's stdout/stderr file. Guest activity
	// can influence VMM output, so an unbounded file is a host disk-exhaustion
	// vector. Zero selects the conservative default.
	FirecrackerLogMaxBytes int64 `json:"firecracker_log_max_bytes"`
	// FirecrackerLogRetentionHours bounds retained VMM failure diagnostics by
	// age. Normal lifecycle exits are deleted immediately. Zero selects the
	// conservative default.
	FirecrackerLogRetentionHours int `json:"firecracker_log_retention_hours"`
	// FirecrackerLogMaxFiles bounds retained VMM failure diagnostics by count.
	// Active VMM logs are excluded. Zero selects the conservative default.
	FirecrackerLogMaxFiles int `json:"firecracker_log_max_files"`
}

const (
	DefaultFirecrackerLogMaxBytes       int64 = 16 << 20
	DefaultFirecrackerLogRetentionHours       = 24
	DefaultFirecrackerLogMaxFiles             = 128
)

// Defaults fills zero values with conservative defaults.
func (c *Config) Defaults() {
	if c.SocketPath == "" {
		c.SocketPath = "/run/sandbox.sock"
	}
	if c.DBPath == "" {
		c.DBPath = "/var/lib/sandbox/registry.db"
	}
	if c.RootfsBase == "" {
		c.RootfsBase = "/opt/fc/devbox-rootfs.ext4"
	}
	if c.RootfsDir == "" {
		c.RootfsDir = "/var/lib/sandbox/rootfs"
	}
	if c.SnapshotDir == "" {
		c.SnapshotDir = "/var/lib/sandbox/snapshots"
	}
	if c.Bridge == "" {
		c.Bridge = "br-fc"
	}
	if c.GatewayIP == "" {
		c.GatewayIP = "172.16.0.1"
	}
	if c.Nameservers == "" {
		c.Nameservers = "8.8.8.8"
	}
	if c.GuestSubnetBits == 0 {
		c.GuestSubnetBits = 24
	}
	if c.KernelArgs == "" {
		c.KernelArgs = "reboot=k panic=1 pci=off root=/dev/vda rw console=ttyS0"
	}
	if c.Vcpus == 0 {
		c.Vcpus = 2
	}
	if c.MemMIB == 0 {
		c.MemMIB = 1024
	}
	if c.FirecrackerBin == "" {
		c.FirecrackerBin = "/usr/local/bin/firecracker"
	}
	if c.VMIsolation == "" {
		c.VMIsolation = "direct"
	}
	if c.VMIsolation == "jailer" {
		if c.JailerBin == "" {
			c.JailerBin = "/usr/local/bin/jailer"
		}
		if c.JailerChrootBase == "" {
			c.JailerChrootBase = "/mnt/sandbox-data/jailer"
		}
		if c.JailerUIDStart == 0 {
			c.JailerUIDStart = 200000
		}
		if c.JailerGIDStart == 0 {
			c.JailerGIDStart = c.JailerUIDStart
		}
		if c.JailerIdentityCount == 0 {
			c.JailerIdentityCount = 4096
		}
		// Per-VM cgroup headroom above the guest's mem_mib, and the single
		// source of truth for per-VM overhead: memory admission charges at
		// least this much (internal/vm.CheckMemoryAdmission refuses to serve
		// otherwise), so raising it lowers how many sandboxes mem_budget_mib
		// admits rather than letting the per-VM allowances overcommit the
		// parent task cgroup. 156 matches the MEM_PER_SLOT_MIB arithmetic
		// deploy-job.sh has always used (1024 + 156 = 1180) and the ~91 MiB
		// per-VM footprint measured on the fleet (docs/benchmarks.md).
		if c.JailerMemoryOverheadMIB == 0 {
			c.JailerMemoryOverheadMIB = 156
		}
		if c.JailerPIDsMax == 0 {
			c.JailerPIDsMax = 64
		}
		if c.JailerCPUWeight == 0 {
			c.JailerCPUWeight = 100
		}
		if c.JailerCPUPeriodUS == 0 {
			c.JailerCPUPeriodUS = 100000
		}
		if c.JailerIOReadBPS == 0 {
			c.JailerIOReadBPS = 256 << 20
		}
		if c.JailerIOWriteBPS == 0 {
			c.JailerIOWriteBPS = 256 << 20
		}
		if c.JailerNoFile == 0 {
			c.JailerNoFile = 256
		}
		if c.JailerFileSize == 0 {
			c.JailerFileSize = 64 << 30
		}
	}
	if c.KernelImage == "" {
		c.KernelImage = "/opt/fc/vmlinux"
	}
	if c.FirecrackerLogMaxBytes == 0 {
		c.FirecrackerLogMaxBytes = DefaultFirecrackerLogMaxBytes
	}
	if c.FirecrackerLogRetentionHours == 0 {
		c.FirecrackerLogRetentionHours = DefaultFirecrackerLogRetentionHours
	}
	if c.FirecrackerLogMaxFiles == 0 {
		c.FirecrackerLogMaxFiles = DefaultFirecrackerLogMaxFiles
	}
	if c.Pools.TapPrefix == "" {
		c.Pools.TapPrefix = "fc"
	}
	if c.Pools.TapMax == 0 {
		c.Pools.TapMax = 64
	}
	if c.Pools.GuestIPMin == "" {
		c.Pools.GuestIPMin = "172.16.0.10"
	}
	if c.Pools.GuestIPMax == "" {
		c.Pools.GuestIPMax = "172.16.0.73"
	}
	if c.Pools.PortMin == 0 {
		c.Pools.PortMin = 5200
	}
	if c.Pools.PortMax == 0 {
		c.Pools.PortMax = 5263
	}
}

// Load reads and decodes path as JSON, applying defaults.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	c.Defaults()
	if c.PlacementDelaySec < 0 {
		return nil, fmt.Errorf("decode %s: placement_delay_sec must be >= 0", path)
	}
	if c.FirecrackerLogMaxBytes < 0 {
		return nil, fmt.Errorf("decode %s: firecracker_log_max_bytes must be >= 0", path)
	}
	if c.FirecrackerLogRetentionHours < 0 {
		return nil, fmt.Errorf("decode %s: firecracker_log_retention_hours must be >= 0", path)
	}
	if c.FirecrackerLogMaxFiles < 0 {
		return nil, fmt.Errorf("decode %s: firecracker_log_max_files must be >= 0", path)
	}
	if c.VMIsolation != "direct" && c.VMIsolation != "jailer" {
		return nil, fmt.Errorf("decode %s: vm_isolation must be direct or jailer", path)
	}
	if c.VMIsolation == "jailer" {
		if c.DisableSeccomp {
			return nil, fmt.Errorf("decode %s: disable_seccomp is forbidden with vm_isolation=jailer", path)
		}
		if c.JailerUIDStart <= 0 || c.JailerGIDStart <= 0 || c.JailerIdentityCount <= 0 {
			return nil, fmt.Errorf("decode %s: jailer identity pool must be positive", path)
		}
		if c.JailerMemoryOverheadMIB <= 0 || c.JailerPIDsMax <= 0 || c.JailerCPUWeight < 1 || c.JailerCPUWeight > 10000 ||
			c.JailerCPUPeriodUS <= 0 || c.JailerIOReadBPS <= 0 || c.JailerIOWriteBPS <= 0 ||
			c.JailerNoFile <= 0 || c.JailerFileSize <= 0 {
			return nil, fmt.Errorf("decode %s: jailer resource limits must be positive and cpu weight must be 1..10000", path)
		}
	}
	return &c, nil
}
