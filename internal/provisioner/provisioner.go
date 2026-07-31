package provisioner

import (
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

// Network describes the host-side bridge used by sandboxes.
type Network struct {
	Bridge                 string // e.g. "br-fc" — tap devices attach here
	GatewayCIDR            string // e.g. "172.16.0.1/24" — bridge address; subnet derived from it
	AllowInterGuestTraffic bool   // false isolates guests that share the bridge
}

// Provisioner performs host-side setup/teardown for sandboxes:
// rootfs copies, tap devices, bridge/NAT networking. (Port forwarding itself
// is a userspace TCP proxy in the server — see internal/server/portproxy.go;
// only legacy DNAT *removal* remains here, for hosts upgrading from the old
// DNAT scheme.)
type Provisioner struct {
	Network     Network
	RootfsBase  string // path to immutable base rootfs (e.g. /opt/fc/devbox-rootfs.ext4)
	RootfsDir   string // directory to hold per-sandbox copies
	SnapshotDir string // directory to hold per-snapshot artifacts (mem/state/rootfs)
}

// Range is a [Off, Off+Len) byte range of a file, produced by DiffExtents for
// diff uploads.
type Range struct {
	Off int64
	Len int64
}

// EnsureNetwork idempotently brings up the host networking the sandboxes need:
// the bridge with its gateway IP, IP-forwarding sysctls, and NAT/FORWARD rules.
// Bridges and iptables rules don't survive a reboot, so the server calls this
// on every startup instead of relying on a one-time setup script.
func (p *Provisioner) EnsureNetwork() error {
	_, subnet, err := net.ParseCIDR(p.Network.GatewayCIDR)
	if err != nil {
		return fmt.Errorf("parse gateway CIDR %q: %w", p.Network.GatewayCIDR, err)
	}

	if _, err := os.Stat("/sys/class/net/" + p.Network.Bridge); err != nil {
		if out, err := exec.Command("ip", "link", "add", "name", p.Network.Bridge, "type", "bridge").CombinedOutput(); err != nil {
			return fmt.Errorf("create bridge %s: %w: %s", p.Network.Bridge, err, out)
		}
	}
	if err := ensureBridgeNetfilter(); err != nil {
		return err
	}
	setup := [][]string{
		{"ip", "addr", "replace", p.Network.GatewayCIDR, "dev", p.Network.Bridge},
		{"ip", "link", "set", p.Network.Bridge, "up"},
		{"sysctl", "-w", "net.ipv4.ip_forward=1"},
		{"sysctl", "-w", "net.ipv4.conf.all.route_localnet=1"},
		{"sysctl", "-w", "net.bridge.bridge-nf-call-iptables=1"},
	}
	for _, args := range setup {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w: %s", args, err, out)
		}
	}

	hostIface, err := defaultInterface()
	if err != nil {
		return err
	}
	rules := [][]string{
		{"-t", "nat", "POSTROUTING", "-s", subnet.String(), "-o", hostIface, "-j", "MASQUERADE"},
		{"-t", "nat", "POSTROUTING", "-o", p.Network.Bridge, "-j", "MASQUERADE"},
		{"FORWARD", "-i", p.Network.Bridge, "-o", hostIface, "-j", "ACCEPT"},
		{"FORWARD", "-i", hostIface, "-o", p.Network.Bridge, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	}
	for _, rule := range rules {
		if err := ensureIptablesRule(rule); err != nil {
			return err
		}
	}
	// A shared bridge must not become an implicit tenant network. Remove the
	// legacy opposite rule before installing the configured policy so upgrades
	// become secure immediately rather than leaving an earlier ACCEPT ahead of
	// a new DROP.
	bridgeRule := interGuestRule(p.Network.Bridge, p.Network.AllowInterGuestTraffic)
	opposite := interGuestRule(p.Network.Bridge, !p.Network.AllowInterGuestTraffic)
	if err := removeIptablesRule(opposite); err != nil {
		return err
	}
	if err := ensureIptablesRule(bridgeRule); err != nil {
		return err
	}

	// Clamp TCP MSS on every forwarded handshake to what the path can actually
	// carry. Firecracker's virtio-net gives the guest a 1500-byte MTU and there
	// is no way to hand it the host's, so on a fabric with a smaller MTU (GCP's
	// VPC is 1460) the guest advertises an MSS 40 bytes too large. Connectivity
	// still "works" — the host drops the oversized frame and returns ICMP
	// frag-needed, and PMTU discovery recovers — but it costs a drop plus a
	// retransmit on every new connection to every new destination (measured on
	// the fleet: ~2.4 retransmits/connection), and it becomes a multi-second
	// stall wherever that ICMP is lost or rate-limited. Clamping is adaptive,
	// so this is a no-op on a 1500-MTU host.
	//
	// Best-effort on purpose: it needs xt_TCPMSS, and a host without that
	// module should still serve (slightly lossier) rather than refuse to start.
	clamp := []string{"-t", "mangle", "FORWARD", "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"}
	if err := ensureIptablesRule(clamp); err != nil {
		log.Printf("provisioner: MSS clamp unavailable, falling back to PMTU discovery: %v", err)
	}
	return nil
}

func ensureBridgeNetfilter() error {
	const knob = "/proc/sys/net/bridge/bridge-nf-call-iptables"
	if _, err := os.Stat(knob); err == nil {
		return nil
	}
	if out, err := exec.Command("modprobe", "br_netfilter").CombinedOutput(); err != nil {
		return fmt.Errorf("load br_netfilter: %w: %s", err, out)
	}
	if _, err := os.Stat(knob); err != nil {
		return fmt.Errorf("br_netfilter loaded but %s is unavailable: %w", knob, err)
	}
	return nil
}

func interGuestRule(bridge string, allow bool) []string {
	target := "DROP"
	if allow {
		target = "ACCEPT"
	}
	return []string{"FORWARD", "-i", bridge, "-o", bridge, "-j", target}
}

// ensureIptablesRule appends rule if an identical one isn't already present.
// rule is the iptables arg list without the -C/-A verb, e.g.
// ["-t","nat","POSTROUTING","-s",...] or ["FORWARD","-i",...].
func ensureIptablesRule(rule []string) error {
	verbAt := 0
	if rule[0] == "-t" {
		verbAt = 2
	}
	check := append(append(append([]string{}, rule[:verbAt]...), "-C", rule[verbAt]), rule[verbAt+1:]...)
	if exec.Command("iptables", check...).Run() == nil {
		return nil
	}
	add := append(append(append([]string{}, rule[:verbAt]...), "-A", rule[verbAt]), rule[verbAt+1:]...)
	if out, err := exec.Command("iptables", add...).CombinedOutput(); err != nil {
		return fmt.Errorf("iptables %v: %w: %s", add, err, out)
	}
	return nil
}

// removeIptablesRule removes every exact copy of rule. Older setup scripts
// could append duplicates, and leaving even one ACCEPT before the DROP would
// defeat isolation.
func removeIptablesRule(rule []string) error {
	verbAt := 0
	if rule[0] == "-t" {
		verbAt = 2
	}
	check := append(append(append([]string{}, rule[:verbAt]...), "-C", rule[verbAt]), rule[verbAt+1:]...)
	del := append(append(append([]string{}, rule[:verbAt]...), "-D", rule[verbAt]), rule[verbAt+1:]...)
	for exec.Command("iptables", check...).Run() == nil {
		if out, err := exec.Command("iptables", del...).CombinedOutput(); err != nil {
			return fmt.Errorf("iptables %v: %w: %s", del, err, out)
		}
	}
	return nil
}

// defaultInterface returns the interface of the default route.
func defaultInterface() (string, error) {
	out, err := exec.Command("ip", "route", "show", "default").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("ip route show default: %w: %s", err, out)
	}
	fields := strings.Fields(string(out))
	for i, f := range fields {
		if f == "dev" && i+1 < len(fields) {
			return fields[i+1], nil
		}
	}
	return "", fmt.Errorf("no default route found in %q", string(out))
}

// PrepareRootfs copies the base rootfs to a per-sandbox path (sparse).
func (p *Provisioner) PrepareRootfs(sandboxID string) (string, error) {
	if err := os.MkdirAll(p.RootfsDir, 0o755); err != nil {
		return "", err
	}
	dest := p.rootfsPath(sandboxID)
	if err := CloneFile(p.RootfsBase, dest); err != nil {
		return "", fmt.Errorf("clone rootfs: %w", err)
	}
	return dest, nil
}

func (p *Provisioner) rootfsPath(id string) string {
	return filepath.Join(p.RootfsDir, id+".ext4")
}

// RootfsPathFor returns the standard per-sandbox rootfs path for an id (without
// creating anything). Used by fan-out to record the row before laying the file.
func (p *Provisioner) RootfsPathFor(id string) string {
	return p.rootfsPath(id)
}

// CloneRootfs lays down a per-sandbox rootfs at the standard path by CoW-cloning
// srcRootfs (a snapshot's frozen rootfs) via reflink. Used by fan-out; the
// returned path is what the sandbox row records, so destroy() cleans it up.
func (p *Provisioner) CloneRootfs(sandboxID, srcRootfs string) (string, error) {
	if err := os.MkdirAll(p.RootfsDir, 0o755); err != nil {
		return "", err
	}
	dest := p.rootfsPath(sandboxID)
	if err := CloneFile(srcRootfs, dest); err != nil {
		return "", err
	}
	return dest, nil
}

// CleanupRootfs deletes the per-sandbox rootfs file (best-effort).
func (p *Provisioner) CleanupRootfs(sandboxID string) error {
	return os.Remove(p.rootfsPath(sandboxID))
}

// RemoveRootfs deletes a rootfs file by its exact path (best-effort). Used when
// the rootfs doesn't live at the default per-id path — e.g. a restored sandbox,
// whose disk sits at the source's original path (baked into the snapshot).
func (p *Provisioner) RemoveRootfs(path string) error {
	if path == "" {
		return nil
	}
	return os.Remove(path)
}

// SnapshotPaths returns the on-disk locations for a snapshot's artifacts and
// ensures the containing directory exists.
func (p *Provisioner) SnapshotPaths(snapshotID string) (mem, state, rootfs string, err error) {
	dir := filepath.Join(p.SnapshotDir, snapshotID)
	if err = os.MkdirAll(dir, 0o755); err != nil {
		return "", "", "", err
	}
	return filepath.Join(dir, "mem.bin"),
		filepath.Join(dir, "state.bin"),
		filepath.Join(dir, "rootfs.ext4"),
		nil
}

// CopyFileSparse copies a single file, creating the destination's parent
// directory if needed. Used to freeze a sandbox's rootfs into a snapshot
// directory, and to lay a snapshot's frozen rootfs back down for a restore.
// Routes through CloneFile so it's an instant reflink CoW clone on XFS/btrfs.
func (p *Provisioner) CopyFileSparse(src, dst string) error {
	return CloneFile(src, dst)
}

// CloneFile copies src to dst as a copy-on-write clone when the filesystem
// supports it: `cp --reflink=always` is instant on XFS/btrfs (src and dst share
// extents until written), which is the single biggest win for restore/fan-out
// latency — it removes the multi-GB rootfs copy. Falls back to a sparse copy on
// filesystems without reflink (e.g. ext4). Creates dst's parent dir if needed.
func CloneFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	if out, err := exec.Command("cp", "--reflink=always", src, dst).CombinedOutput(); err == nil {
		return nil
	} else if !reflinkUnsupported(out) {
		return fmt.Errorf("reflink %s -> %s: %w: %s", src, dst, err, out)
	}
	// Filesystem doesn't support reflink — fall back to a plain sparse copy.
	if out, err := exec.Command("cp", "--sparse=always", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("cp %s -> %s: %w: %s", src, dst, err, out)
	}
	return nil
}

// WriteSparseRanges creates dst with logical size and copies only ranges from
// src. Holes remain holes. It is used after composing snapshot layers into a
// full reflink: DiffExtents identifies every block no longer shared with the
// immutable golden, and this function turns those blocks back into a portable
// one-level sparse delta.
func WriteSparseRanges(src, dst string, size int64, ranges []Range) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	if err := out.Truncate(size); err != nil {
		return err
	}
	buf := make([]byte, 1<<20)
	for _, r := range ranges {
		if r.Off < 0 || r.Len < 0 || r.Off > size || r.Len > size-r.Off {
			return fmt.Errorf("range [%d,%d) outside file size %d", r.Off, r.Off+r.Len, size)
		}
		for off, remaining := r.Off, r.Len; remaining > 0; {
			n := remaining
			if n > int64(len(buf)) {
				n = int64(len(buf))
			}
			if _, err := in.ReadAt(buf[:n], off); err != nil {
				return fmt.Errorf("read %s @%d: %w", src, off, err)
			}
			if _, err := out.WriteAt(buf[:n], off); err != nil {
				return fmt.Errorf("write %s @%d: %w", dst, off, err)
			}
			off += n
			remaining -= n
		}
	}
	if err := out.Sync(); err != nil {
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

// reflinkUnsupported reports whether a failed `cp --reflink=always` failed
// because the filesystem can't reflink (vs. a real error like ENOSPC), so we
// know it's safe to fall back to a sparse copy. coreutils emits EOPNOTSUPP /
// "not supported" / "Invalid cross-device link" in that case.
func reflinkUnsupported(out []byte) bool {
	s := strings.ToLower(string(out))
	return strings.Contains(s, "not supported") ||
		strings.Contains(s, "operation not supported") ||
		strings.Contains(s, "invalid cross-device") ||
		strings.Contains(s, "cross-device link")
}

// CleanupSnapshot removes a snapshot's artifact directory (best-effort).
func (p *Provisioner) CleanupSnapshot(snapshotID string) error {
	return os.RemoveAll(filepath.Join(p.SnapshotDir, snapshotID))
}

// CreateTap creates a tap device and attaches it to the configured bridge.
func (p *Provisioner) CreateTap(tap string) error {
	if err := p.CreateTapUnbridged(tap); err != nil {
		return err
	}
	return p.AttachTapToBridge(tap)
}

// CreateTapUnbridged creates an up tap device WITHOUT attaching it to the
// bridge. Used by fan-out: a clone resumes on an unbridged tap so it can read
// MMDS and reidentify eth0 to its fresh IP/MAC before joining the shared
// bridge, so the source's baked IP never appears on br-fc (no collision).
func (p *Provisioner) CreateTapUnbridged(tap string) error {
	steps := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap"},
		{"ip", "link", "set", tap, "up"},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w: %s", args, err, out)
		}
	}
	return nil
}

// AttachTapToBridge attaches an existing tap to the configured bridge.
func (p *Provisioner) AttachTapToBridge(tap string) error {
	if out, err := exec.Command("ip", "link", "set", tap, "master", p.Network.Bridge).CombinedOutput(); err != nil {
		return fmt.Errorf("attach %s to %s: %w: %s", tap, p.Network.Bridge, err, out)
	}
	return nil
}

// DeleteTap removes a tap device (best-effort).
func (p *Provisioner) DeleteTap(tap string) error {
	out, err := exec.Command("ip", "link", "delete", tap).CombinedOutput()
	if err != nil {
		return fmt.Errorf("delete tap %s: %w: %s", tap, err, out)
	}
	return nil
}

// RemovePortForwardTo removes legacy host:hostPort → guestIP:guestPort DNAT
// rules (both the PREROUTING rule for external clients and the OUTPUT rule
// for loopback clients). Best-effort: kept only so hosts upgrading from the
// DNAT port-forwarding scheme get their stale rules cleaned up by
// destroy/reconcile — new sandboxes never install DNAT.
func (p *Provisioner) RemovePortForwardTo(hostPort int, guestIP string, guestPort int) {
	target := guestIP + ":" + strconv.Itoa(guestPort)
	rules := [][]string{
		{"iptables", "-t", "nat", "-D", "PREROUTING", "-p", "tcp", "--dport", strconv.Itoa(hostPort), "-j", "DNAT", "--to-destination", target},
		{"iptables", "-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-d", "127.0.0.1", "--dport", strconv.Itoa(hostPort), "-j", "DNAT", "--to-destination", target},
	}
	for _, args := range rules {
		_ = exec.Command(args[0], args[1:]...).Run()
	}
}
