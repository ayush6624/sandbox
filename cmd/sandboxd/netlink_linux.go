//go:build linux

package main

// Reidentifying a clone means reconfiguring eth0, and the obvious way to do it
// — shelling out to `ip` — makes iproute2 a hard requirement of every guest
// image. That is fine for the base rootfs, which we build, and wrong for a
// template built from a container image, which we do not: published images
// (every Terminal-Bench task image, for one) ship bash but no iproute2, and a
// clone that cannot reconfigure eth0 resumes still holding the template's
// address — reachable at the old ip, invisible at the new one, i.e. broken in
// the least obvious way available.
//
// So when `ip` is absent, do the same work over netlink directly. The library
// is already in the module graph (the Firecracker SDK's CNI dependencies pull
// it in) and is pure Go, so this costs a file rather than a dependency.
//
// The `ip` path is left in place and still preferred when the binary exists:
// the fleet's base image has run it for every clone in production, and there is
// no reason to put a second implementation in that path.

import (
	"fmt"
	"net"
	"os/exec"
	"sync"

	"github.com/vishvananda/netlink"
)

var (
	haveIPOnce sync.Once
	haveIPCmd  bool
)

// haveIPCommand reports whether iproute2 is installed. Cached: it cannot change
// under a running guest in any way that matters, and applyIdentity is on the
// clone hot path.
func haveIPCommand() bool {
	haveIPOnce.Do(func() {
		_, err := exec.LookPath("ip")
		haveIPCmd = err == nil
	})
	return haveIPCmd
}

// applyIdentityNetlink is the `ip -batch` sequence in applyIdentity, expressed
// as netlink calls: down, flush addresses, set MAC, up, add address, replace
// the default route, route MMDS on-link.
func applyIdentityNetlink(iface, ipAddr, mac, gw string, bits int) error {
	link, err := netlink.LinkByName(iface)
	if err != nil {
		return fmt.Errorf("find %s: %w", iface, err)
	}
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return fmt.Errorf("parse mac %q: %w", mac, err)
	}

	if err := netlink.LinkSetDown(link); err != nil {
		return fmt.Errorf("link down: %w", err)
	}
	addrs, err := netlink.AddrList(link, netlink.FAMILY_V4)
	if err != nil {
		return fmt.Errorf("list addrs: %w", err)
	}
	for _, addr := range addrs {
		if err := netlink.AddrDel(link, &addr); err != nil {
			return fmt.Errorf("delete addr %s: %w", addr.IPNet, err)
		}
	}
	if err := netlink.LinkSetHardwareAddr(link, hw); err != nil {
		return fmt.Errorf("set mac: %w", err)
	}
	if err := netlink.LinkSetUp(link); err != nil {
		return fmt.Errorf("link up: %w", err)
	}
	if err := netlink.AddrAdd(link, &netlink.Addr{IPNet: &net.IPNet{
		IP:   net.ParseIP(ipAddr),
		Mask: net.CIDRMask(bits, 32),
	}}); err != nil {
		return fmt.Errorf("add addr %s/%d: %w", ipAddr, bits, err)
	}

	// RouteReplace, like `ip route replace`, both adds and overwrites — the
	// resumed guest still carries the template's default route here.
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Gw:        net.ParseIP(gw),
	}); err != nil {
		return fmt.Errorf("replace default via %s: %w", gw, err)
	}
	return mmdsRouteNetlink(link)
}

// mmdsRouteNetlink puts the link-local MMDS address on eth0. A
// kernel-configured guest has no such route, and without it the request follows
// the default route out through the gateway.
func mmdsRouteNetlink(link netlink.Link) error {
	if link == nil {
		var err error
		if link, err = netlink.LinkByName(mmdsIface); err != nil {
			return fmt.Errorf("find %s: %w", mmdsIface, err)
		}
	}
	if err := netlink.RouteReplace(&netlink.Route{
		LinkIndex: link.Attrs().Index,
		Dst:       &net.IPNet{IP: net.ParseIP(mmdsAddr), Mask: net.CIDRMask(32, 32)},
		Scope:     netlink.SCOPE_LINK,
	}); err != nil {
		return fmt.Errorf("route %s via %s: %w", mmdsAddr, mmdsIface, err)
	}
	return nil
}
