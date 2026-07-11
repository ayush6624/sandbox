//go:build !linux

package main

// setClockRealtime needs clock_settime; sandboxd only ever runs in Linux guests.
func setClockRealtime(unixNano int64) error { return nil }
