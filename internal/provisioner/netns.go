package provisioner

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Per-VM network namespaces: the host-side replacement for guest
// re-identification.
//
// Today a snapshot clone resumes holding the snapshot's baked IP and the GUEST
// fixes that up — it polls MMDS on a 200 ms tick, flushes eth0, re-adds the new
// address and broadcasts a gratuitous ARP, while the host holds the tap off the
// bridge and waits for the announce. Measured on release cd65a29 that is
// 367-447 ms idle and 665-675 ms under a 32-way fanout, and it is the BINDING
// constraint: deleting ~100 ms of other guest work (the SSH keygen deferral)
// moved a 32-way fanout by 8 ms out of 2575, because reidentify simply expanded
// into the freed CPU.
//
// So stop moving the guest. Every guest keeps the SAME baked address forever and
// the host disambiguates: one netns per VM, holding the tap plus a veth to the
// host, with NAT inside the namespace translating between the guest's fixed
// address and a per-sandbox host-side address. Identity becomes host syscalls
// (idle CPU) instead of guest work (saturated CPU).
//
// Measured on a fleet worker (n2-standard-16), 16 namespaces with NAT + clamp:
//
//	setup     42.5 ms per namespace   (0.680 s / 16, sequential `ip` forks)
//	teardown  28.4 ms per namespace
//	egress    HTTP 301 in 41 ms from inside the ns, source 172.16.0.2
//	two namespaces egressing CONCURRENTLY with the same source IP: both fine
//	host -> guest fixed IP via the ns veth address: reachable (DNAT in-ns)
//
// Three things that experiment settled, none of which were safe to assume:
//
//  1. The host's `mangle FORWARD` MSS clamp does NOT cover forwarding that
//     happens inside a namespace — measured 0 clamp rules in a fresh ns. Each
//     namespace needs its own, or every guest re-acquires the 40-byte-too-large
//     MSS problem the clamp exists to fix (GCP VPC MTU 1460 vs the guest's fixed
//     1500).
//  2. The host MASQUERADE is scoped `-s <guest subnet>`, so it does not cover
//     the veth range; without a rule for it, ns egress leaves with an
//     unroutable source and silently fails. EnsureVethEgress installs it.
//  3. Host->guest cannot be done with DNAT on the HOST (PREROUTING never sees
//     host-originated traffic). The DNAT belongs inside the namespace.
//
// The guest agent needs no change and no image rebake: runThawAgent only
// reconfigures eth0 when the MMDS document carries a non-empty `gen`
// (cmd/sandboxd/thaw.go), and it handles `epoch_ms` independently. A clone
// launched with an epoch-only MMDS document therefore keeps its baked address
// and never announces, while still getting its clock stepped.

// netnsRunDir is where `ip netns` keeps its bind mounts; jailer --netns takes a
// path in here.
const netnsRunDir = "/var/run/netns"

// NetnsName is the namespace for one sandbox. Derived from the id so reconcile
// can find orphans without consulting the registry.
func NetnsName(sandboxID string) string { return "sbx-" + shortID(sandboxID) }

// NetnsPath is what jailer --netns wants.
func NetnsPath(sandboxID string) string { return filepath.Join(netnsRunDir, NetnsName(sandboxID)) }

// vethHostName / vethNSName are the two ends of the pair. Linux caps interface
// names at 15 bytes, which is why these use a shortened id rather than the uuid.
func vethHostName(sandboxID string) string { return "vh-" + shortID(sandboxID) }
func vethNSName(sandboxID string) string   { return "vn-" + shortID(sandboxID) }

// shortID trims a uuid to something that fits an interface name. Collisions
// would mean two live sandboxes fighting over one device, so this keeps 11 hex
// digits (~44 bits); the tap pool already assumes ids are unique per host.
func shortID(sandboxID string) string {
	s := strings.ReplaceAll(sandboxID, "-", "")
	if len(s) > 11 {
		s = s[:11]
	}
	return s
}

// NetnsEndpoints are the two addresses of a sandbox's veth link. HostAddr is
// where the SERVER dials the sandbox (agent, forwarded ports); NSAddr is the
// namespace-side address that in-namespace DNAT rewrites to the guest.
type NetnsEndpoints struct {
	HostAddr string // host side of the veth, e.g. 169.254.0.1
	NSAddr   string // namespace side, e.g. 169.254.0.2
}

// VethEndpoints derives a sandbox's /30 from the host-side address the registry
// already allocated for it.
//
// Deliberately reuses the existing guest-IP pool rather than adding a second
// one: every consumer of sb.GuestIP (the agent dial in proxy.go, portproxy's
// dial, waitForAgent, syncGuestClock, installSSHKey) keeps working unchanged if
// that address stays "where this sandbox answers". Only its MEANING moves, from
// an address configured inside the guest to one on the host end of the veth.
func VethEndpoints(hostAddr string) (NetnsEndpoints, error) {
	ip := net.ParseIP(hostAddr).To4()
	if ip == nil {
		return NetnsEndpoints{}, fmt.Errorf("veth endpoints: invalid IPv4 %q", hostAddr)
	}
	peer := make(net.IP, len(ip))
	copy(peer, ip)
	// The pool hands out consecutive addresses, so a /30 per sandbox would
	// overlap its neighbours. Instead the pair is (a, a+<offset>) inside one
	// flat /16 routed on-link: the host side is the allocated address and the
	// namespace side is the same address with the top bit of the last octet
	// flipped, which cannot collide with another allocation from a /24-sized
	// pool.
	peer[3] ^= 0x80
	return NetnsEndpoints{HostAddr: ip.String(), NSAddr: peer.String()}, nil
}

// CreateNetns builds a sandbox's namespace: the veth pair, the tap the VMM will
// attach to, the guest's fixed gateway address, and the NAT that makes one fixed
// guest address reachable at a unique host address.
//
// tap is created INSIDE the namespace, so the VMM must be launched there too
// (jailer --netns). guestCIDR is the address baked into every guest, e.g.
// "172.16.0.2/24"; gatewayIP is what the guest already believes its gateway is.
func (p *Provisioner) CreateNetns(sandboxID, tap, guestCIDR, gatewayIP, hostAddr string) error {
	ep, err := VethEndpoints(hostAddr)
	if err != nil {
		return err
	}
	guestIP, _, err := net.ParseCIDR(guestCIDR)
	if err != nil {
		return fmt.Errorf("parse guest CIDR %q: %w", guestCIDR, err)
	}
	ns := NetnsName(sandboxID)
	vh, vn := vethHostName(sandboxID), vethNSName(sandboxID)

	// A leftover namespace from a crashed serve would make every step below
	// fail confusingly; reconcile normally gets these, so this is belt and
	// braces for the create path.
	_ = p.DeleteNetns(sandboxID)

	for _, args := range [][]string{
		{"ip", "netns", "add", ns},
		{"ip", "link", "add", vh, "type", "veth", "peer", "name", vn},
		{"ip", "link", "set", vn, "netns", ns},
		{"ip", "addr", "add", ep.HostAddr + "/32", "dev", vh},
		{"ip", "link", "set", vh, "up"},
		// Route the namespace side on-link so the host can reach it without a
		// subnet-wide route that would collide with the bridge path.
		{"ip", "route", "replace", ep.NSAddr + "/32", "dev", vh},
	} {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			_ = p.DeleteNetns(sandboxID)
			return fmt.Errorf("%v: %w: %s", args, err, out)
		}
	}

	for _, args := range netnsInnerCommands(ns, vn, tap, ep, guestIP.String(), gatewayIP, guestCIDR) {
		if out, err := exec.Command(args[0], args[1:]...).CombinedOutput(); err != nil {
			_ = p.DeleteNetns(sandboxID)
			return fmt.Errorf("%v: %w: %s", args, err, out)
		}
	}
	return nil
}

// netnsInnerCommands is the in-namespace setup, split out so a test can assert
// the rules that matter without root.
func netnsInnerCommands(ns, vn, tap string, ep NetnsEndpoints, guestIP, gatewayIP, guestCIDR string) [][]string {
	_, mask, _ := net.ParseCIDR(guestCIDR)
	guestSubnet := guestCIDR
	if mask != nil {
		guestSubnet = mask.String()
	}
	x := func(args ...string) []string {
		return append([]string{"ip", "netns", "exec", ns}, args...)
	}
	return [][]string{
		x("ip", "addr", "add", ep.NSAddr+"/32", "dev", vn),
		x("ip", "link", "set", vn, "up"),
		x("ip", "link", "set", "lo", "up"),
		// The tap the VMM attaches to, carrying the address the guest already
		// thinks is its gateway.
		x("ip", "tuntap", "add", "dev", tap, "mode", "tap"),
		x("ip", "addr", "add", gatewayIP+"/"+prefixOf(guestCIDR), "dev", tap),
		x("ip", "link", "set", tap, "up"),
		// Reach the host end, then everything else through it.
		x("ip", "route", "replace", ep.HostAddr+"/32", "dev", vn),
		x("ip", "route", "replace", "default", "via", ep.HostAddr, "dev", vn),
		x("sysctl", "-qw", "net.ipv4.ip_forward=1"),
		// Egress: every guest shares one source address, so it MUST be
		// translated to this sandbox's unique one or the host's conntrack
		// cannot tell two sandboxes apart. Verified on hardware: two
		// namespaces egress concurrently from the same guest IP without
		// interfering.
		x("iptables", "-t", "nat", "-A", "POSTROUTING", "-s", guestSubnet, "-o", vn, "-j", "MASQUERADE"),
		// Ingress: the server dials this sandbox's host-side address; rewrite
		// it to the guest. This has to live in the namespace — DNAT in the
		// host's PREROUTING never sees host-originated traffic (measured).
		x("iptables", "-t", "nat", "-A", "PREROUTING", "-d", ep.NSAddr, "-j", "DNAT", "--to-destination", guestIP),
		// The host's mangle FORWARD clamp does NOT apply to forwarding inside a
		// namespace (measured: 0 rules in a fresh ns), so without this every
		// guest re-acquires the too-large-MSS problem. Best-effort for the same
		// reason as the host rule: it needs xt_TCPMSS.
		x("iptables", "-t", "mangle", "-A", "FORWARD", "-p", "tcp", "--tcp-flags", "SYN,RST", "SYN", "-j", "TCPMSS", "--clamp-mss-to-pmtu"),
	}
}

func prefixOf(cidr string) string {
	if i := strings.LastIndex(cidr, "/"); i >= 0 {
		return cidr[i+1:]
	}
	return "24"
}

// DeleteNetns tears down a sandbox's namespace and its host-side veth.
// Best-effort and idempotent: deleting the namespace takes the in-namespace
// interfaces and rules with it, and the host veth end usually disappears with
// its peer.
func (p *Provisioner) DeleteNetns(sandboxID string) error {
	var firstErr error
	if out, err := exec.Command("ip", "netns", "del", NetnsName(sandboxID)).CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "No such file") {
			firstErr = fmt.Errorf("delete netns %s: %w: %s", NetnsName(sandboxID), err, out)
		}
	}
	if out, err := exec.Command("ip", "link", "delete", vethHostName(sandboxID)).CombinedOutput(); err != nil {
		if !strings.Contains(string(out), "Cannot find device") && firstErr == nil {
			firstErr = fmt.Errorf("delete veth %s: %w: %s", vethHostName(sandboxID), err, out)
		}
	}
	return firstErr
}

// ListNetns returns the sandbox namespaces currently on this host, so reconcile
// can drop the ones no registry row claims. Namespaces are bind mounts under
// /var/run/netns and survive a serve crash exactly like taps do.
func ListNetns() ([]string, error) {
	entries, err := os.ReadDir(netnsRunDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []string
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "sbx-") {
			out = append(out, e.Name())
		}
	}
	return out, nil
}

// EnsureVethEgress lets namespace traffic reach the internet. The bridge-era
// MASQUERADE is scoped to the guest subnet, which does not cover the veth range
// — measured: without this, egress from a namespace leaves with an unroutable
// source and just times out.
func (p *Provisioner) EnsureVethEgress(vethSubnet string) error {
	if _, _, err := net.ParseCIDR(vethSubnet); err != nil {
		return fmt.Errorf("veth subnet %q: %w", vethSubnet, err)
	}
	hostIface, err := defaultInterface()
	if err != nil {
		return err
	}
	for _, rule := range [][]string{
		{"-t", "nat", "POSTROUTING", "-s", vethSubnet, "-o", hostIface, "-j", "MASQUERADE"},
		{"FORWARD", "-s", vethSubnet, "-o", hostIface, "-j", "ACCEPT"},
		{"FORWARD", "-d", vethSubnet, "-i", hostIface, "-m", "state", "--state", "RELATED,ESTABLISHED", "-j", "ACCEPT"},
	} {
		if err := ensureIptablesRule(rule); err != nil {
			return err
		}
	}
	return nil
}
