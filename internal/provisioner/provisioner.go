package provisioner

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
)

// Network describes the host-side bridge and the guest-side application port
// that gets forwarded.
type Network struct {
	Bridge    string // e.g. "br-fc" — tap devices attach here
	GuestPort int    // e.g. 5173 — port the in-guest app listens on
}

// Provisioner performs host-side setup/teardown for sandboxes:
// rootfs copies, tap devices, iptables port-forwards.
type Provisioner struct {
	Network    Network
	RootfsBase string // path to immutable base rootfs (e.g. /opt/fc/devbox-rootfs.ext4)
	RootfsDir  string // directory to hold per-sandbox copies
}

// PrepareRootfs copies the base rootfs to a per-sandbox path (sparse).
func (p *Provisioner) PrepareRootfs(sandboxID string) (string, error) {
	if err := os.MkdirAll(p.RootfsDir, 0o755); err != nil {
		return "", err
	}
	dest := p.rootfsPath(sandboxID)
	cmd := exec.Command("cp", "--sparse=always", p.RootfsBase, dest)
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("cp rootfs: %w: %s", err, out)
	}
	return dest, nil
}

func (p *Provisioner) rootfsPath(id string) string {
	return filepath.Join(p.RootfsDir, id+".ext4")
}

// CleanupRootfs deletes the per-sandbox rootfs file (best-effort).
func (p *Provisioner) CleanupRootfs(sandboxID string) error {
	return os.Remove(p.rootfsPath(sandboxID))
}

// CreateTap creates a tap device and attaches it to the configured bridge.
func (p *Provisioner) CreateTap(tap string) error {
	steps := [][]string{
		{"ip", "tuntap", "add", "dev", tap, "mode", "tap"},
		{"ip", "link", "set", tap, "master", p.Network.Bridge},
		{"ip", "link", "set", tap, "up"},
	}
	for _, args := range steps {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w: %s", args, err, out)
		}
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

// AddPortForward sets up host:hostPort → guestIP:GuestPort DNAT (both PREROUTING
// for external clients and OUTPUT for loopback clients).
func (p *Provisioner) AddPortForward(hostPort int, guestIP string) error {
	target := guestIP + ":" + strconv.Itoa(p.Network.GuestPort)
	rules := [][]string{
		{"iptables", "-t", "nat", "-A", "PREROUTING", "-p", "tcp", "--dport", strconv.Itoa(hostPort), "-j", "DNAT", "--to-destination", target},
		{"iptables", "-t", "nat", "-A", "OUTPUT", "-p", "tcp", "-d", "127.0.0.1", "--dport", strconv.Itoa(hostPort), "-j", "DNAT", "--to-destination", target},
	}
	for _, args := range rules {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			return fmt.Errorf("%v: %w: %s", args, err, out)
		}
	}
	return nil
}

// RemovePortForward undoes AddPortForward (best-effort — rules may already be gone).
func (p *Provisioner) RemovePortForward(hostPort int, guestIP string) {
	target := guestIP + ":" + strconv.Itoa(p.Network.GuestPort)
	rules := [][]string{
		{"iptables", "-t", "nat", "-D", "PREROUTING", "-p", "tcp", "--dport", strconv.Itoa(hostPort), "-j", "DNAT", "--to-destination", target},
		{"iptables", "-t", "nat", "-D", "OUTPUT", "-p", "tcp", "-d", "127.0.0.1", "--dport", strconv.Itoa(hostPort), "-j", "DNAT", "--to-destination", target},
	}
	for _, args := range rules {
		_ = exec.Command(args[0], args[1:]...).Run()
	}
}
