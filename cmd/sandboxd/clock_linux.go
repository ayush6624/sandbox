//go:build linux

package main

import "golang.org/x/sys/unix"

// setClockRealtime steps the guest's CLOCK_REALTIME to the given Unix
// nanoseconds (sandboxd runs as root in the guest).
func setClockRealtime(unixNano int64) error {
	ts := unix.NsecToTimespec(unixNano)
	return unix.ClockSettime(unix.CLOCK_REALTIME, &ts)
}
