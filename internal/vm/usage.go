package vm

import (
	"fmt"
	"strconv"
	"strings"
)

// UsageSample is a point-in-time reading of what a VM has consumed on the host.
//
// CPUUsec is the number that matters: it is monotonic for the life of one VMM
// and, because a jailed VM's cgroup leaf is created at launch and removed when
// the VMM exits, the final reading IS that VM's total consumed CPU. No
// cross-reading subtraction is needed, and no other VM's time can leak into it.
//
// MemBytes is the guest's current resident memory. It is recorded for capacity
// work, NOT for billing: with no virtio-balloon device a guest's dirtied pages
// never come back, so this converges on the allocated size and is not something
// a customer can control. Memory is billed on allocation.
type UsageSample struct {
	CPUUsec  int64
	MemBytes int64
	// FromCgroup is false when the reading came from the /proc fallback, which
	// an unjailed (development) launch has to use. Kept so a mixed dataset is
	// never mistaken for uniformly accurate accounting.
	FromCgroup bool
}

// clockTicksPerSecond is USER_HZ, which is 100 on every Linux/amd64 and
// Linux/arm64 kernel we run. Hard-coded rather than resolved through cgo, since
// the whole build is CGO_ENABLED=0 (pure-Go SQLite cross-compilation).
const clockTicksPerSecond = 100

// parseCgroupStatValue pulls one "<key> <value>" line out of a flat cgroup stat
// file such as cpu.stat.
//
// Parsing lives here, unbuilt-tagged and free of file I/O, so the formats can be
// tested on any platform. Only the reads are Linux-only.
func parseCgroupStatValue(content, key string) (int64, bool) {
	for _, line := range strings.Split(content, "\n") {
		name, value, ok := strings.Cut(strings.TrimSpace(line), " ")
		if !ok || name != key {
			continue
		}
		v, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
		if err != nil {
			return 0, false
		}
		return v, true
	}
	return 0, false
}

// parseProcStatCPUUsec sums utime+stime out of /proc/<pid>/stat content and
// converts clock ticks to microseconds.
//
// The comm field (2) is parenthesized and may itself contain spaces and
// parentheses — Firecracker's doesn't, but a naive Fields() split is the classic
// way to misparse this file, so fields are counted from the LAST ')'.
func parseProcStatCPUUsec(content string) (int64, error) {
	close := strings.LastIndexByte(content, ')')
	if close < 0 || close+2 >= len(content) {
		return 0, fmt.Errorf("malformed proc stat: no comm field")
	}
	// After comm(2) and state(3), utime is field 14 and stime 15 — indices 11
	// and 12 of what follows the comm field.
	fields := strings.Fields(content[close+2:])
	if len(fields) < 13 {
		return 0, fmt.Errorf("proc stat has %d fields after comm, need 13", len(fields))
	}
	utime, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse utime: %w", err)
	}
	stime, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse stime: %w", err)
	}
	return (utime + stime) * 1e6 / clockTicksPerSecond, nil
}
