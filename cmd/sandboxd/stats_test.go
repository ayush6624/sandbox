package main

import (
	"os"
	"path/filepath"
	"testing"
)

func write(t *testing.T, name, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// MemAvailable, not MemFree: on a guest that has done any I/O, page cache makes
// MemFree read as "nearly out of memory" while the workload is fine.
func TestReadMeminfo(t *testing.T) {
	p := write(t, "meminfo", `MemTotal:        1017256 kB
MemFree:           82340 kB
MemAvailable:     812044 kB
Buffers:           12345 kB
`)
	total, available := readMeminfo(p)
	if total != 1017256*1024 {
		t.Errorf("total = %d, want %d bytes", total, 1017256*1024)
	}
	if available != 812044*1024 {
		t.Errorf("available = %d, want %d bytes (MemAvailable, not MemFree)", available, 812044*1024)
	}
}

func TestReadMeminfoMissingFileIsZero(t *testing.T) {
	if total, available := readMeminfo("/no/such/meminfo"); total != 0 || available != 0 {
		t.Errorf("= %d/%d, want 0/0: a surprising guest must not fail the whole sample", total, available)
	}
}

func TestReadLoadavg(t *testing.T) {
	p := write(t, "loadavg", "0.52 0.31 0.14 2/143 9182\n")
	load1, procs := readLoadavg(p)
	if load1 != 0.52 {
		t.Errorf("load1 = %v, want 0.52", load1)
	}
	if procs != 143 {
		t.Errorf("processes = %d, want 143 (the total, not the runnable count)", procs)
	}
}

func TestReadLoadavgMalformedIsZero(t *testing.T) {
	if load1, procs := readLoadavg(write(t, "loadavg", "garbage\n")); load1 != 0 || procs != 0 {
		t.Errorf("= %v/%d, want 0/0", load1, procs)
	}
}

// Free space is the unprivileged figure: reporting the reserved-blocks pool as
// free tells a caller they have room they cannot actually use.
func TestReadDisk(t *testing.T) {
	total, free := readDisk(t.TempDir())
	if total <= 0 {
		t.Fatalf("total = %d, want > 0", total)
	}
	if free <= 0 || free > total {
		t.Errorf("free = %d, want 0 < free <= total (%d)", free, total)
	}
	if total, free := readDisk("/no/such/mount"); total != 0 || free != 0 {
		t.Errorf("absent path = %d/%d, want 0/0", total, free)
	}
}
