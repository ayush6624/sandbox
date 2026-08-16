//go:build !linux

package main

import "errors"

// The guest is always Linux; these exist so the agent still builds on a dev
// machine. Reporting `ip` as present keeps the non-linux build on the shell-out
// path, which is the one with a stub for every other guest operation too.
func haveIPCommand() bool { return true }

func applyIdentityNetlink(iface, ipAddr, mac, gw string, bits int) error {
	return errors.New("netlink identity is linux-only")
}

func mmdsRouteNetlink(_ any) error { return errors.New("netlink routing is linux-only") }
