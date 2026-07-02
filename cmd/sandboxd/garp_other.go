//go:build !linux

package main

// announceIdentity needs AF_PACKET; sandboxd only ever runs in Linux guests.
func announceIdentity(iface, ip, mac string) error { return nil }
