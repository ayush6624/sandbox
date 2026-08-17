package provisioner

import (
	"net"
	"strings"
	"testing"
)

// The host and namespace ends of a sandbox's veth must never collide with
// another sandbox's, because the pool hands out CONSECUTIVE addresses. Flipping
// the top bit of the last octet keeps the pair inside one flat /16 while
// guaranteeing the peer of a /24-sized allocation lands outside it.
func TestVethEndpointsCannotCollideAcrossConsecutiveAllocations(t *testing.T) {
	seen := map[string]string{} // address -> which sandbox claimed it
	for i := 10; i < 130; i++ {
		host := net.IPv4(172, 16, 0, byte(i)).String()
		ep, err := VethEndpoints(host)
		if err != nil {
			t.Fatalf("%s: %v", host, err)
		}
		if ep.HostAddr != host {
			t.Fatalf("host addr = %s, want the allocated %s", ep.HostAddr, host)
		}
		if ep.HostAddr == ep.NSAddr {
			t.Fatalf("%s: host and namespace ends are the same address", host)
		}
		for _, addr := range []string{ep.HostAddr, ep.NSAddr} {
			if prev, dup := seen[addr]; dup {
				t.Fatalf("address %s claimed by both %s and %s — two live sandboxes would fight over it", addr, prev, host)
			}
			seen[addr] = host
		}
	}
}

func TestVethEndpointsRejectsGarbage(t *testing.T) {
	for _, bad := range []string{"", "not-an-ip", "::1", "172.16.0.999"} {
		if _, err := VethEndpoints(bad); err == nil {
			t.Errorf("VethEndpoints(%q) accepted an invalid address", bad)
		}
	}
}

// Interface names are capped at 15 bytes by Linux, and a uuid does not fit.
func TestVethNamesFitInterfaceLimit(t *testing.T) {
	id := "f17f0020-6801-4602-808f-0d1e9b8481f6"
	for _, name := range []string{vethHostName(id), vethNSName(id), NetnsName(id)} {
		if len(name) > 15 {
			t.Errorf("%q is %d bytes; Linux caps interface names at 15", name, len(name))
		}
	}
	if NetnsPath(id) != "/var/run/netns/"+NetnsName(id) {
		t.Errorf("NetnsPath = %q, not where `ip netns` keeps its bind mounts", NetnsPath(id))
	}
	// Distinct sandboxes must get distinct namespaces.
	if NetnsName(id) == NetnsName("aaaaaaaa-6801-4602-808f-0d1e9b8481f6") {
		t.Error("two different sandbox ids produced the same namespace name")
	}
}

// The in-namespace rules are the security and correctness boundary, and three of
// them were only settled by measuring on a worker (see netns.go). Pin them, so a
// later edit cannot quietly drop one:
//   - SNAT, or two guests sharing one source address collide in host conntrack
//   - DNAT inside the ns, because host PREROUTING never sees host-originated
//     traffic, so without it the server cannot reach the agent at all
//   - the MSS clamp, because the host's mangle FORWARD does NOT cover forwarding
//     inside a namespace (measured: 0 rules in a fresh ns)
func TestNetnsInnerCommandsCarryNATAndClamp(t *testing.T) {
	ep := NetnsEndpoints{HostAddr: "172.16.0.10", NSAddr: "172.16.0.138"}
	cmds := netnsInnerCommands("sbx-abc", "vn-abc", "tap-abc", ep, "172.16.0.2", "172.16.0.1", "172.16.0.2/24")

	var flat []string
	for _, c := range cmds {
		flat = append(flat, strings.Join(c, " "))
	}
	joined := strings.Join(flat, "\n")

	for _, want := range []string{
		// every command must actually run inside the namespace
		"ip netns exec sbx-abc",
		// guest source translated to this sandbox's unique address
		"-t nat -A POSTROUTING -s 172.16.0.0/24 -o vn-abc -j MASQUERADE",
		// inbound rewritten to the guest's fixed address
		"-t nat -A PREROUTING -d 172.16.0.138 -j DNAT --to-destination 172.16.0.2",
		// per-namespace MSS clamp
		"-t mangle -A FORWARD -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu",
		// the tap the VMM attaches to, carrying the guest's believed gateway
		"ip tuntap add dev tap-abc mode tap",
		"ip addr add 172.16.0.1/24 dev tap-abc",
		"net.ipv4.ip_forward=1",
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("in-namespace setup is missing %q\ngot:\n%s", want, joined)
		}
	}
	for _, c := range cmds {
		if len(c) < 4 || c[0] != "ip" || c[1] != "netns" || c[2] != "exec" {
			t.Fatalf("command escapes the namespace: %v", c)
		}
	}
}
