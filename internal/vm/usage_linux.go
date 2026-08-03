//go:build linux

package vm

import (
	"fmt"
	"os"
	"strconv"
	"strings"
)

// SampleUsage reads what m has consumed so far.
//
// Prefers the VM's cgroup v2 leaf, which accounts for the whole VMM including
// its kernel-side work. Falls back to the Firecracker process's own utime+stime
// when there is no leaf (an unjailed development launch), which undercounts —
// hence UsageSample.FromCgroup.
//
// CALL ORDER MATTERS: the leaf is removed by the launch cleanup that runs when
// the VMM exits, so a final sample must be taken BEFORE the VM is stopped. A
// sample attempted afterwards returns an error, which callers treat as "keep
// the last heartbeat value" rather than as a failure.
func SampleUsage(m *Machine) (UsageSample, error) {
	if m == nil {
		return UsageSample{}, fmt.Errorf("nil machine")
	}
	if leaf := m.usageCgroupLeaf(); leaf != "" {
		s, err := sampleCgroup(leaf)
		if err == nil {
			return s, nil
		}
		// A vanished leaf means the VMM is already gone. Fall through: the
		// /proc read will fail too, and the caller keeps its last sample.
		if !os.IsNotExist(err) {
			return UsageSample{}, err
		}
	}
	pid, err := PID(m)
	if err != nil {
		return UsageSample{}, err
	}
	return sampleProc(pid)
}

// usageCgroupLeaf resolves the leaf for either machine flavor: SDK-backed VMs
// carry it directly, raw ones (clone and UFFD paths) through their rawMachine.
func (m *Machine) usageCgroupLeaf() string {
	if m.cgroupLeaf != "" {
		return m.cgroupLeaf
	}
	if m.raw != nil {
		return m.raw.cgroupLeaf
	}
	return ""
}

// sampleCgroup reads usage_usec from cpu.stat and memory.current.
func sampleCgroup(leaf string) (UsageSample, error) {
	b, err := os.ReadFile(leaf + "/cpu.stat")
	if err != nil {
		return UsageSample{}, err
	}
	usec, ok := parseCgroupStatValue(string(b), "usage_usec")
	if !ok {
		return UsageSample{}, fmt.Errorf("cpu.stat in %s has no usage_usec", leaf)
	}
	s := UsageSample{CPUUsec: usec, FromCgroup: true}
	// memory.current is diagnostic only, so its absence must not fail the CPU
	// reading that margin analysis depends on.
	if mb, err := os.ReadFile(leaf + "/memory.current"); err == nil {
		if v, err := strconv.ParseInt(strings.TrimSpace(string(mb)), 10, 64); err == nil {
			s.MemBytes = v
		}
	}
	return s, nil
}

// sampleProc is the unjailed fallback: utime+stime from /proc/<pid>/stat, plus
// resident pages from statm.
func sampleProc(pid int) (UsageSample, error) {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return UsageSample{}, err
	}
	usec, err := parseProcStatCPUUsec(string(b))
	if err != nil {
		return UsageSample{}, fmt.Errorf("/proc/%d/stat: %w", pid, err)
	}
	s := UsageSample{CPUUsec: usec}
	if mb, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", pid)); err == nil {
		if f := strings.Fields(string(mb)); len(f) >= 2 {
			if pages, err := strconv.ParseInt(f[1], 10, 64); err == nil {
				s.MemBytes = pages * int64(os.Getpagesize())
			}
		}
	}
	return s, nil
}
