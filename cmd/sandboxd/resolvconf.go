package main

import (
	"fmt"
	"os"
	"strings"
)

const (
	resolvConfPath = "/etc/resolv.conf"
	pnpPath        = "/proc/net/pnp"
)

// materializeResolvConf copies the kernel's `ip=` autoconf DNS settings from
// /proc/net/pnp into a *regular* /etc/resolv.conf.
//
// The rootfs used to symlink /etc/resolv.conf -> /proc/net/pnp so a guest would
// honor whatever nameservers the host config set without baking them into the
// image. glibc reads that happily, but /proc files report st_size=0, and any
// resolver that sizes the file before reading it sees an empty config. c-ares —
// which backs Node's dns.resolve*/undici, and therefore Claude Code's
// "Checking connectivity..." probe — is one of those: it finds no nameservers
// and falls back to 127.0.0.1:53, where nothing listens. The symptom is DNS
// that works for curl/git/npm/python (glibc) and fails for anything c-ares
// based, i.e. an "internet connection" that looks intermittently broken
// depending on which tool you reach for. A regular file keeps the
// host-config-driven nameservers and satisfies both resolvers.
//
// Cold boot re-runs this on every start. Snapshot-restored guests (hot create,
// fan-out, hibernation wake) resume a live process instead, but they inherit
// the file through the rootfs the golden was snapshotted with, so they are
// covered too.
func materializeResolvConf() error {
	pnp, err := os.ReadFile(pnpPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", pnpPath, err)
	}
	conf := resolverDirectives(pnp)
	if conf == "" {
		return fmt.Errorf("%s carried no resolver directives", pnpPath)
	}
	// Write-and-rename rather than truncate in place: on a rootfs built before
	// this change the path is still the symlink, and writing through it would
	// land in /proc (read-only) instead of replacing it.
	tmp := resolvConfPath + ".tmp"
	if err := os.WriteFile(tmp, []byte(conf), 0o644); err != nil {
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, resolvConfPath); err != nil {
		return fmt.Errorf("rename onto %s: %w", resolvConfPath, err)
	}
	return nil
}

// resolverDirectives keeps only the lines resolv.conf understands. /proc/net/pnp
// also carries a "#MANUAL" marker and, when the kernel got its config from a
// boot server, "bootserver" lines that resolv.conf has no business seeing.
func resolverDirectives(pnp []byte) string {
	var b strings.Builder
	for line := range strings.SplitSeq(string(pnp), "\n") {
		switch fields := strings.Fields(line); {
		case len(fields) == 0:
		case fields[0] == "nameserver", fields[0] == "search",
			fields[0] == "domain", fields[0] == "options":
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	return b.String()
}
