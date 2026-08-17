package main

import (
	"bufio"
	"net/http"
	"os"
	"strconv"
	"strings"
	"syscall"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

// handleStats reports the guest's own view of its memory and disk for the
// host's utilization sampler. Read-only, cheap (three small /proc reads and a
// statfs), and deliberately unauthenticated like /health: it exposes nothing a
// process in this guest cannot already read for itself.
//
// Every reader below degrades to zero rather than failing the response. A
// sample missing one field is still worth having, and a guest whose /proc
// layout surprises us must not take out the whole series.
func handleStats(w http.ResponseWriter, r *http.Request) {
	s := agentapi.Stats{}
	s.MemTotalBytes, s.MemAvailableBytes = readMeminfo("/proc/meminfo")
	s.DiskTotalBytes, s.DiskFreeBytes = readDisk("/")
	s.Load1, s.Processes = readLoadavg("/proc/loadavg")
	writeJSON(w, http.StatusOK, s)
}

// readMeminfo returns MemTotal and MemAvailable in bytes. /proc/meminfo reports
// kibibytes.
func readMeminfo(path string) (total, available int64) {
	f, err := os.Open(path)
	if err != nil {
		return 0, 0
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		key, value, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) == 0 {
			continue
		}
		kib, err := strconv.ParseInt(fields[0], 10, 64)
		if err != nil {
			continue
		}
		switch key {
		case "MemTotal":
			total = kib * 1024
		case "MemAvailable":
			available = kib * 1024
		}
		if total > 0 && available > 0 {
			break
		}
	}
	return total, available
}

// readDisk reports the filesystem holding path. Free space is the UNPRIVILEGED
// figure (Bavail), not Bfree: the reserved-blocks pool is not usable by the
// workload, so reporting Bfree tells a caller they have room they cannot use.
func readDisk(path string) (total, free int64) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, 0
	}
	bsize := int64(st.Bsize)
	return int64(st.Blocks) * bsize, int64(st.Bavail) * bsize
}

// readLoadavg returns the 1-minute load average and the total process count
// from /proc/loadavg's "running/total" field.
func readLoadavg(path string) (load1 float64, processes int) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, 0
	}
	fields := strings.Fields(string(b))
	if len(fields) < 4 {
		return 0, 0
	}
	load1, _ = strconv.ParseFloat(fields[0], 64)
	if _, total, ok := strings.Cut(fields[3], "/"); ok {
		processes, _ = strconv.Atoi(total)
	}
	return load1, processes
}
