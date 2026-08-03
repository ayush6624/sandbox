package vm

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// fakeLeaf builds a stand-in for a VM's cgroup v2 leaf.
func fakeLeaf(t *testing.T, files map[string]string) string {
	t.Helper()
	leaf := t.TempDir()
	for name, value := range files {
		if err := os.WriteFile(filepath.Join(leaf, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	return leaf
}

func readLeaf(t *testing.T, leaf, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(leaf, name))
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

// The ceiling is derived from what the guest has actually TOUCHED
// (memory.current), not from its configured size: untouched guest pages map to
// the shared zero page and are never charged, so mem_mib says nothing about the
// real headroom.
func TestArmSnapshotMemoryHighUsesCurrentPlusMargin(t *testing.T) {
	const current = int64(300 << 20)
	leaf := fakeLeaf(t, map[string]string{
		"memory.max":     strconv.FormatInt(1180<<20, 10),
		"memory.current": strconv.FormatInt(current, 10),
		"memory.high":    "max",
	})

	restore, err := armSnapshotMemoryHigh(leaf)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	want := strconv.FormatInt(current+snapshotMemoryHighMargin(1180<<20-current), 10)
	if got := readLeaf(t, leaf, "memory.high"); got != want {
		t.Fatalf("memory.high = %q, want %q", got, want)
	}

	// A running VM must never be left throttled: it may legitimately use all of
	// mem_mib, and a permanent ceiling would be a worse bug than the OOM.
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	if got := readLeaf(t, leaf, "memory.high"); got != "max" {
		t.Fatalf("restored memory.high = %q, want max", got)
	}
}

// The ceiling must sit ABOVE the current footprint. memory.swap.max is 0, so
// guest anonymous pages cannot be reclaimed at all — a ceiling below them would
// throttle against memory that can never be freed, producing a hard stall and
// then the OOM this exists to prevent.
func TestArmSnapshotMemoryHighStaysAboveCurrentFootprint(t *testing.T) {
	const current = int64(900 << 20)
	leaf := fakeLeaf(t, map[string]string{
		"memory.max":     strconv.FormatInt(1180<<20, 10),
		"memory.current": strconv.FormatInt(current, 10),
		"memory.high":    "max",
	})

	if _, err := armSnapshotMemoryHigh(leaf); err != nil {
		t.Fatalf("arm: %v", err)
	}
	high, err := strconv.ParseInt(readLeaf(t, leaf, "memory.high"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if high <= current {
		t.Fatalf("memory.high %d must exceed memory.current %d", high, current)
	}
}

// Without room for a full margin the ceiling CLAMPS to just under the fence
// rather than refusing. A narrow band still makes the kernel reclaim before
// memory.max, and refusing would break small sandboxes that already worked: a
// 128 MiB guest sits close to its own limit precisely because that limit is
// small. Refusing here was a regression caught in fleet verification.
func TestArmSnapshotMemoryHighClampsWhenMarginDoesNotFit(t *testing.T) {
	limit := int64(284 << 20) // a 128 MiB guest's leaf
	current := limit - (8 << 20)
	leaf := fakeLeaf(t, map[string]string{
		"memory.max":     strconv.FormatInt(limit, 10),
		"memory.current": strconv.FormatInt(current, 10),
		"memory.high":    "max",
	})

	if _, err := armSnapshotMemoryHigh(leaf); err != nil {
		t.Fatalf("a tight cgroup must still be protected, not refused: %v", err)
	}
	high, err := strconv.ParseInt(readLeaf(t, leaf, "memory.high"), 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if high >= limit {
		t.Fatalf("memory.high %d must stay below memory.max %d to engage first", high, limit)
	}
	if high < current {
		t.Fatalf("memory.high %d must not sit below the unreclaimable footprint %d", high, current)
	}
}

// Only a VM already at its limit is refused: there is nowhere to put a ceiling,
// so nothing stands between the write and an OOM kill. The refusal happens while
// the VMM is alive and paused, so the caller resumes it and the sandbox survives.
func TestArmSnapshotMemoryHighRefusesAtTheFence(t *testing.T) {
	limit := int64(1180 << 20)
	leaf := fakeLeaf(t, map[string]string{
		"memory.max":     strconv.FormatInt(limit, 10),
		"memory.current": strconv.FormatInt(limit, 10),
		"memory.high":    "max",
	})

	if _, err := armSnapshotMemoryHigh(leaf); err == nil {
		t.Fatal("expected a refusal for a VM already at its memory limit")
	} else if !strings.Contains(err.Error(), "already using") {
		t.Fatalf("error should say the VM is already at its limit, got: %v", err)
	}
	// The refusal must not leave a ceiling behind on a VM that keeps running.
	if got := readLeaf(t, leaf, "memory.high"); got != "max" {
		t.Fatalf("memory.high = %q after a refusal, want max", got)
	}
}

// An unlimited memory.max cannot OOM-kill anything, so adding a throttle would
// only slow the snapshot down for no benefit.
func TestArmSnapshotMemoryHighSkipsUnlimitedCgroup(t *testing.T) {
	leaf := fakeLeaf(t, map[string]string{
		"memory.max":     "max",
		"memory.current": strconv.FormatInt(300<<20, 10),
		"memory.high":    "max",
	})

	restore, err := armSnapshotMemoryHigh(leaf)
	if err != nil {
		t.Fatalf("arm: %v", err)
	}
	if got := readLeaf(t, leaf, "memory.high"); got != "max" {
		t.Fatalf("memory.high = %q, want max (no throttle needed)", got)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
}

// If the guard cannot be read at all, the snapshot must not proceed: silently
// skipping it is how the VMM got OOM-killed in the first place.
func TestArmSnapshotMemoryHighFailsClosedOnMissingFiles(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"no memory.high": {
			"memory.max":     strconv.FormatInt(1180<<20, 10),
			"memory.current": strconv.FormatInt(300<<20, 10),
		},
		"no memory.max": {
			"memory.current": strconv.FormatInt(300<<20, 10),
			"memory.high":    "max",
		},
		"no memory.current": {
			"memory.max":  strconv.FormatInt(1180<<20, 10),
			"memory.high": "max",
		},
		"empty leaf": {},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := armSnapshotMemoryHigh(fakeLeaf(t, files)); err == nil {
				t.Fatal("expected the guard to fail closed")
			}
		})
	}
}

// The memory guard is installed even on a host with no io.max configuration —
// the two protect different halves of the same burst, and the memory half is the
// one that kills the VMM.
func TestSnapshotWriteWindowGuardsMemoryWithoutIOLimits(t *testing.T) {
	cfg := JailerConfig{CgroupRoot: t.TempDir(), CgroupParent: "task"}
	leafPath := jailerCgroupLeaf(cfg, "vm-1")
	if err := os.MkdirAll(leafPath, 0o755); err != nil {
		t.Fatal(err)
	}
	const current = int64(200 << 20)
	for name, value := range map[string]string{
		"memory.max":     strconv.FormatInt(1180<<20, 10),
		"memory.current": strconv.FormatInt(current, 10),
		"memory.high":    "max",
	} {
		if err := os.WriteFile(filepath.Join(leafPath, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}

	window := snapshotWriteWindow(cfg, "vm-1")
	if window == nil {
		t.Fatal("jailed launch must always get a snapshot window: the memory guard is not optional")
	}
	restore, err := window()
	if err != nil {
		t.Fatalf("open window: %v", err)
	}
	want := strconv.FormatInt(current+snapshotMemoryHighMargin(1180<<20-current), 10)
	if got := readLeaf(t, leafPath, "memory.high"); got != want {
		t.Fatalf("memory.high = %q, want %q", got, want)
	}
	if err := restore(); err != nil {
		t.Fatalf("restore: %v", err)
	}
	// Restoration is idempotent: Snapshot can hit a primary failure and still
	// run its deferred policy cleanup.
	if err := restore(); err != nil {
		t.Fatalf("restore again: %v", err)
	}
	if got := readLeaf(t, leafPath, "memory.high"); got != "max" {
		t.Fatalf("restored memory.high = %q, want max", got)
	}
}

// If the memory guard fails, the window must not leave the VM's write bandwidth
// cap lifted — a half-open window would outlive the snapshot that needed it.
func TestSnapshotWriteWindowRestoresIOWhenMemoryGuardFails(t *testing.T) {
	cfg := JailerConfig{
		CgroupRoot: t.TempDir(), CgroupParent: "task",
		IODevice: "8:16", IOReadBPS: 10 << 20, IOWriteBPS: 5 << 20,
	}
	leafPath := jailerCgroupLeaf(cfg, "vm-1")
	if err := os.MkdirAll(leafPath, 0o755); err != nil {
		t.Fatal(err)
	}
	limit := int64(1180 << 20)
	for name, value := range map[string]string{
		"memory.max":     strconv.FormatInt(limit, 10),
		"memory.current": strconv.FormatInt(limit, 10), // at the fence: refused
		"memory.high":    "max",
		"io.max":         ioMaxValue(cfg, false),
	} {
		if err := os.WriteFile(filepath.Join(leafPath, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := snapshotWriteWindow(cfg, "vm-1")(); err == nil {
		t.Fatal("expected the window to refuse for a VM at its memory limit")
	}
	if got, want := readLeaf(t, leafPath, "io.max"), ioMaxValue(cfg, false); got != want {
		t.Fatalf("io.max = %q after a refused window, want the throttled value %q", got, want)
	}
}

func TestCgroupLimitBytes(t *testing.T) {
	leaf := fakeLeaf(t, map[string]string{
		"finite":  "1237319680\n",
		"maxed":   "max\n",
		"garbage": "not-a-number\n",
	})
	if got, err := cgroupLimitBytes(filepath.Join(leaf, "finite")); err != nil || got != 1237319680 {
		t.Fatalf("finite = %d (err %v), want 1237319680", got, err)
	}
	// "max" reads as 0 — the caller treats that as "unlimited", not "zero".
	if got, err := cgroupLimitBytes(filepath.Join(leaf, "maxed")); err != nil || got != 0 {
		t.Fatalf("max = %d (err %v), want 0", got, err)
	}
	if _, err := cgroupLimitBytes(filepath.Join(leaf, "garbage")); err == nil {
		t.Fatal("garbage value should error rather than read as 0/unlimited")
	}
	if _, err := cgroupLimitBytes(filepath.Join(leaf, "absent")); err == nil {
		t.Fatal("absent file should error")
	}
}

// The margin is a fraction of the available slack, capped at both ends. A tight
// leaf must keep most of its slack as absorption room for writeback overshoot —
// a fixed margin ate that buffer and lost 256 MiB sandboxes on the fleet.
func TestSnapshotMemoryHighMarginScalesWithSlack(t *testing.T) {
	for _, tc := range []struct {
		name  string
		slack int64
		want  int64
	}{
		{"roomy leaf caps at the maximum", 830 << 20, snapshotMemoryHighMarginMax},
		{"tight leaf takes a quarter", 112 << 20, 28 << 20},
		{"very tight leaf floors at the minimum", 8 << 20, snapshotMemoryHighMarginMin},
		{"no slack still floors", 0, snapshotMemoryHighMarginMin},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := snapshotMemoryHighMargin(tc.slack); got != tc.want {
				t.Fatalf("margin(%d MiB slack) = %d MiB, want %d MiB",
					tc.slack>>20, got>>20, tc.want>>20)
			}
		})
	}
	// However tight, the band must leave the majority of the slack free to
	// absorb overshoot before the fence.
	slack := int64(112 << 20)
	if margin := snapshotMemoryHighMargin(slack); margin > slack/2 {
		t.Fatalf("margin %d MiB consumes more than half of %d MiB of slack", margin>>20, slack>>20)
	}
}
