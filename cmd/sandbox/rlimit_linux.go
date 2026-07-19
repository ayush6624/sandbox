//go:build linux

package main

import (
	"fmt"
	"os"
	"syscall"
)

// raiseNoFileLimit lifts RLIMIT_NOFILE's soft limit to the hard limit. The
// server holds several fds per sandbox (firecracker API socket, log file, log
// FIFO, port-proxy listeners and their connections) and the gateway holds
// pooled connections per host; the usual 1024 soft default exhausts long
// before the resource pools do. Best-effort — a failure is logged, not fatal.
func raiseNoFileLimit() {
	var rl syscall.Rlimit
	if err := syscall.Getrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		fmt.Fprintf(os.Stderr, "rlimit: get NOFILE: %v\n", err)
		return
	}
	if rl.Cur >= rl.Max {
		return
	}
	rl.Cur = rl.Max
	if err := syscall.Setrlimit(syscall.RLIMIT_NOFILE, &rl); err != nil {
		fmt.Fprintf(os.Stderr, "rlimit: raise NOFILE to %d: %v\n", rl.Max, err)
	}
}
