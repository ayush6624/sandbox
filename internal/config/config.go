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
	SocketPath string `json:"socket_path"` // unix socket the server listens on (and the CLI dials)
	ListenAddr string `json:"listen_addr"` // optional TCP listener, e.g. "100.99.183.74:8080" (tailnet); requires api_token
	APIToken   string `json:"api_token"`   // bearer token required on the TCP listener

	// --- Gateway registration (optional; multi-host) ---
	GatewayURL    string `json:"gateway_url"`    // register/heartbeat target, e.g. "http://100.x:9090"; requires listen_addr
	GatewayToken  string `json:"gateway_token"`  // bearer presented to the gateway
	AdvertiseAddr string `json:"advertise_addr"` // addr the gateway dials back; defaults to listen_addr
	HostID        string `json:"host_id"`        // stable host identity; defaults to hostname

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

	// --- Networking ---
	Bridge      string `json:"bridge"`      // e.g. "br-fc"
	GatewayIP   string `json:"gateway_ip"`  // bridge IP, used as guest default gateway
	Nameservers string `json:"nameservers"` // comma-separated DNS for the guest
	GuestPort   int    `json:"guest_port"`  // port the in-guest app listens on, forwarded to a host port
	// GuestSubnetBits is the prefix length of the guest subnet shared by the
	// bridge (gateway) and every guest NIC. It caps how many sandboxes can run
	// concurrently on a host: a /24 holds ~253 usable IPs, a /22 ~1021, a /20
	// ~4093. Widen it (and the guest-IP pool) to run more than ~250 small
	// sandboxes at once. Must be the same on the gateway CIDR and the guest
	// CIDR or guests can't route to the gateway. 0 = default 24.
	GuestSubnetBits int `json:"guest_subnet_bits"`

	// --- Behavior ---
	// Hot create is on by default: the server maintains a golden snapshot of a
	// pristine booted sandbox and serves POST /sandboxes by cloning it (fan-out
	// mechanism), falling back to cold boot. Set true to always cold-boot.
	DisableHotCreate bool `json:"disable_hot_create"`
	// HibernateAfterSec freezes a sandbox idle this many seconds to disk
	// (memory snapshot + kill), releasing its slot/tap/IP/port; the next
	// exec/files/shell request wakes it transparently (~1-2 s). Only API
	// activity counts — traffic on forwarded host ports does not reset the
	// idle clock, nor does it wake a hibernated sandbox. 0 disables.
	HibernateAfterSec int `json:"hibernate_after_sec"`
	// CreateConcurrency bounds concurrent sandbox bring-ups (cold boots and
	// golden clones); excess creates queue in-process so a burst can't
	// boot-storm the host into agent timeouts. 0 = server default
	// (min(2×NumCPU, 16)).
	CreateConcurrency int `json:"create_concurrency"`
	// UFFDRestore makes same-identity hibernation wakes restore the guest with
	// Firecracker's userfaultfd memory backend: the guest resumes before its
	// RAM is paged in and faults its working set from the mem file on demand,
	// cutting wake latency (and wake I/O) roughly to the working set instead of
	// the whole guest. Off = the eager File backend (whole-RAM fault-in before
	// resume). Only the same-identity restore path is UFFD-backed; the
	// clone-path wake still uses File. See docs/scale-to-zero.md.
	UFFDRestore bool `json:"uffd_restore"`
	// UFFDChunkKiB selects the UFFD page source when UFFDRestore is on: 0 (default)
	// serves faults from a whole-file mmap of the mem image; >0 reads the mem file
	// in fixed chunks of this many KiB on demand, through a chunk cache. Behavior
	// is identical either way — this is the chunk-indexing/cache path a remote
	// (GCS) memory source will reuse for cross-host wake (roadmap Phase B). Rounded
	// down to a 4 KiB multiple, floored at one page. Typical: 1024 or 2048.
	UFFDChunkKiB int `json:"uffd_chunk_kib"`
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
	KernelImage    string `json:"kernel_image"`
	KernelArgs     string `json:"kernel_args"`
	Vcpus          int64  `json:"vcpus"`
	MemMIB         int64  `json:"mem_mib"`
}

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
	if c.GuestPort == 0 {
		c.GuestPort = 3000
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
	if c.KernelImage == "" {
		c.KernelImage = "/opt/fc/vmlinux"
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
	return &c, nil
}
