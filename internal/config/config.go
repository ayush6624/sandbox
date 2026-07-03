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

	// --- Behavior ---
	// Hot create is on by default: the server maintains a golden snapshot of a
	// pristine booted sandbox and serves POST /sandboxes by cloning it (fan-out
	// mechanism), falling back to cold boot. Set true to always cold-boot.
	DisableHotCreate bool `json:"disable_hot_create"`

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
