package vm

import "testing"

func TestParseCgroupStatValue(t *testing.T) {
	// Real cgroup v2 cpu.stat: usage_usec is not the first key, and siblings
	// share its prefix.
	const cpuStat = `usage_usec 12345678
user_usec 9000000
system_usec 3345678
nr_periods 0
nr_throttled 0
throttled_usec 0
`
	got, ok := parseCgroupStatValue(cpuStat, "usage_usec")
	if !ok {
		t.Fatal("usage_usec not found")
	}
	if got != 12345678 {
		t.Fatalf("usage_usec = %d, want 12345678", got)
	}

	// A prefix match must not satisfy a different key.
	if v, ok := parseCgroupStatValue(cpuStat, "usage"); ok {
		t.Fatalf("key %q matched a prefix, returning %d", "usage", v)
	}
	if _, ok := parseCgroupStatValue(cpuStat, "absent_key"); ok {
		t.Fatal("absent key reported as found")
	}
	if _, ok := parseCgroupStatValue("usage_usec notanumber\n", "usage_usec"); ok {
		t.Fatal("unparsable value reported as found")
	}
}

func TestParseProcStatCPUUsec(t *testing.T) {
	// Fields: pid comm state ppid pgrp session tty tpgid flags minflt cminflt
	// majflt cmajflt utime stime ... — utime=200 stime=100 at fields 14/15.
	// 300 ticks at 100 Hz = 3s = 3_000_000 usec.
	line := "42 (firecracker) S 1 42 42 0 -1 4194304 100 0 0 0 200 100 0 0 20 0 3 0 999 0 0\n"
	got, err := parseProcStatCPUUsec(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != 3_000_000 {
		t.Fatalf("cpu usec = %d, want 3000000", got)
	}
}

// A comm containing spaces and parentheses is the classic way to misparse
// /proc/<pid>/stat: counting fields from the start of the line shifts every
// index and silently reports the wrong CPU time.
func TestParseProcStatCPUUsecHandlesAwkwardComm(t *testing.T) {
	line := "42 (fire cracker (odd)) S 1 42 42 0 -1 4194304 100 0 0 0 200 100 0 0 20 0 3 0 999 0 0\n"
	got, err := parseProcStatCPUUsec(line)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got != 3_000_000 {
		t.Fatalf("cpu usec = %d, want 3000000 (comm parsing shifted the fields)", got)
	}
}

func TestParseProcStatCPUUsecRejectsMalformed(t *testing.T) {
	for name, content := range map[string]string{
		"no comm":     "42 firecracker S 1 2 3\n",
		"truncated":   "42 (firecracker) S 1 2 3\n",
		"empty":       "",
		"bad numbers": "42 (firecracker) S 1 42 42 0 -1 4194304 100 0 0 0 x y 0 0 20 0 3 0 999 0 0\n",
	} {
		if _, err := parseProcStatCPUUsec(content); err == nil {
			t.Errorf("%s: expected an error, got none", name)
		}
	}
}
