package vm

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestProcessFamilyContainsOnlyAncestorChain(t *testing.T) {
	parents := map[int]int{42: 21, 21: 7, 7: 1, 1: 0}
	got := processFamily(42, func(pid int) int { return parents[pid] })
	for _, pid := range []int{42, 21, 7, 1} {
		if !got[pid] {
			t.Fatalf("ancestor %d missing from process family: %v", pid, got)
		}
	}
	if got[99] {
		t.Fatalf("unrelated process admitted to family: %v", got)
	}
}

func TestProcessFamilyStopsAtCycle(t *testing.T) {
	got := processFamily(42, func(pid int) int {
		if pid == 42 {
			return 21
		}
		return 42
	})
	if len(got) != 2 || !got[42] || !got[21] {
		t.Fatalf("cyclic process family = %v, want only 42 and 21", got)
	}
}

func TestJailerPrepareStagesAssetsAndAppliesOnePolicy(t *testing.T) {
	base, err := os.MkdirTemp("/tmp", "jailer-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(base) })
	jailer := writeFixtureFile(t, filepath.Join(base, "bin", "jailer"), "jailer", 0755)
	firecracker := writeFixtureFile(t, filepath.Join(base, "bin", "firecracker"), "firecracker", 0755)
	kernel := writeFixtureFile(t, filepath.Join(base, "assets", "vmlinux"), "kernel", 0600)
	rootfs := writeFixtureFile(t, filepath.Join(base, "rootfs", "vm.ext4"), "rootfs", 0600)
	mem := writeFixtureFile(t, filepath.Join(base, "snapshots", "memory"), "memory", 0600)
	state := writeFixtureFile(t, filepath.Join(base, "snapshots", "state"), "state", 0600)
	cgroupRoot := filepath.Join(base, "cgroup")
	if err := os.MkdirAll(filepath.Join(cgroupRoot, "nomad", "task"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cgroupRoot, "cgroup.controllers"), []byte("cpu memory pids io"), 0644); err != nil {
		t.Fatal(err)
	}
	// The task cgroup's aggregate limit, as production always has it: the
	// snapshot window reserves against it and refuses when it cannot be read.
	if err := os.WriteFile(filepath.Join(cgroupRoot, "nomad", "task", "memory.max"),
		[]byte(strconv.FormatInt(57<<30, 10)), 0644); err != nil {
		t.Fatal(err)
	}
	jailUID, jailGID := os.Geteuid(), os.Getegid()
	if jailUID == 0 {
		jailUID, jailGID = 200000, 200000
	}
	cfg := JailerConfig{
		JailerBin:       jailer,
		ChrootBaseDir:   filepath.Join(base, "jails"),
		UIDStart:        jailUID,
		GIDStart:        jailGID,
		IdentityCount:   1,
		CgroupParent:    "nomad/task",
		CgroupRoot:      cgroupRoot,
		TrustedOwnerUID: os.Geteuid(),
		IODevice:        "8:16",
		IOReadBPS:       10 << 20,
		IOWriteBPS:      5 << 20,
	}
	req := LaunchRequest{
		Mode:           LaunchUFFDRestore,
		FirecrackerBin: firecracker,
		VMID:           "vm-123",
		HostAPIPath:    filepath.Join(base, "ignored.sock"),
		KernelImage:    kernel,
		RootfsPath:     rootfs,
		SnapshotMem:    mem,
		SnapshotState:  state,
		UFFDHostPath:   filepath.Join(base, "ignored.uffd"),
		Vcpus:          2,
		MemMIB:         1024,
	}
	prepared, err := newJailerProcessLauncher(cfg).Prepare(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.cleanup()

	root := filepath.Join(cfg.ChrootBaseDir, "firecracker", req.VMID, "root")
	if prepared.HostAPIPath != filepath.Join(root, "run", "firecracker.socket") {
		t.Fatalf("host API path = %q", prepared.HostAPIPath)
	}
	wantPaths := LaunchPaths{
		API: "/run/firecracker.socket", Kernel: "/kernel/vmlinux",
		Rootfs: "/disks/rootfs.ext4", SnapshotMem: "/snapshots/memory",
		SnapshotState: "/snapshots/state", UFFD: "/run/uffd.socket",
	}
	if !reflect.DeepEqual(prepared.Paths, wantPaths) {
		t.Fatalf("paths = %+v, want %+v", prepared.Paths, wantPaths)
	}
	for path, want := range map[string]string{
		filepath.Join(root, "kernel", "vmlinux"):    "kernel",
		filepath.Join(root, "disks", "rootfs.ext4"): "rootfs",
		filepath.Join(root, "snapshots", "memory"):  "memory",
		filepath.Join(root, "snapshots", "state"):   "state",
	} {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", path, got, want)
		}
		if path != filepath.Join(root, "disks", "rootfs.ext4") {
			st, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if st.Mode().Perm() != 0444 || statUID(st) != os.Geteuid() {
				t.Fatalf("trusted staged input %s mode/uid = %o/%d", path, st.Mode().Perm(), statUID(st))
			}
		}
	}
	assertSameInode(t, rootfs, filepath.Join(root, "disks", "rootfs.ext4"))

	args := strings.Join(prepared.Command.Args[1:], "\x00")
	for _, want := range []string{
		"--new-pid-ns", "--cgroup-version\x002", "--parent-cgroup\x00nomad/task/vm-123",
		"no-file=256", "fsize=68719476736",
		"--\x00--api-sock\x00/run/firecracker.socket",
	} {
		if !strings.Contains(args, want) {
			t.Errorf("jailer args %q do not contain %q", prepared.Command.Args, want)
		}
	}
	if strings.Count(args, "--id") != 1 {
		t.Fatalf("jailer must own the sole --id argument: %q", prepared.Command.Args)
	}
	if strings.Contains("\x00"+args+"\x00", "\x00--cgroup\x00") {
		t.Fatalf("jailer must use the preconfigured cgroup leaf: %q", prepared.Command.Args)
	}
	leaf := jailerCgroupLeaf(cfg, req.VMID)
	for file, want := range map[string]string{
		// 1024 MiB guest + the 156 MiB per-VM overhead the server's memory
		// admission charges (defaultJailerMemoryOverhead). The two MUST agree:
		// see CheckMemoryAdmission.
		"memory.max":      "1237319680",
		"memory.swap.max": "0",
		// A running VM is never throttled; the snapshot window installs a
		// ceiling only for the duration of a snapshot.
		"memory.high": "max",
		"pids.max":    "64",
		"cpu.max":     "200000 100000",
		"cpu.weight":  "100",
		"io.max":      "8:16 rbps=10485760 wbps=5242880",
	} {
		got, err := os.ReadFile(filepath.Join(leaf, file))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", file, got, want)
		}
	}
	if prepared.BeginSnapshotWrite == nil {
		t.Fatal("jailed launch omitted snapshot write window")
	}
	// memory.current is kernel-provided in a real cgroup; the fake leaf must
	// supply it. 300 MiB stands in for a guest that has touched some of its RAM.
	memCurrent := int64(300 << 20)
	if err := os.WriteFile(filepath.Join(leaf, "memory.current"),
		[]byte(strconv.FormatInt(memCurrent, 10)), 0600); err != nil {
		t.Fatal(err)
	}
	restoreWriteLimit, err := prepared.BeginSnapshotWrite(true)
	if err != nil {
		t.Fatalf("begin snapshot write: %v", err)
	}
	// The window must also cap page-cache growth, or a full snapshot's write
	// charges into memory.max and the kernel OOM-kills the VMM.
	memHighPath := filepath.Join(leaf, "memory.high")
	if got, err := os.ReadFile(memHighPath); err != nil {
		t.Fatal(err)
	} else if want := strconv.FormatInt(memCurrent+snapshotMemoryHighMargin(1237319680-memCurrent), 10); string(got) != want {
		t.Fatalf("snapshot memory.high = %q, want %q (current + margin)", got, want)
	}
	ioMaxPath := filepath.Join(leaf, "io.max")
	if got, err := os.ReadFile(ioMaxPath); err != nil {
		t.Fatal(err)
	} else if want := "8:16 rbps=10485760 wbps=max"; string(got) != want {
		t.Fatalf("snapshot io.max = %q, want %q", got, want)
	}
	if err := restoreWriteLimit(); err != nil {
		t.Fatalf("restore snapshot write limit: %v", err)
	}
	// Restoration is idempotent because Snapshot may encounter a primary
	// failure and still run its deferred policy cleanup.
	if err := restoreWriteLimit(); err != nil {
		t.Fatalf("restore snapshot write limit again: %v", err)
	}
	if got, err := os.ReadFile(ioMaxPath); err != nil {
		t.Fatal(err)
	} else if want := "8:16 rbps=10485760 wbps=5242880"; string(got) != want {
		t.Fatalf("restored io.max = %q, want %q", got, want)
	}
	// A running VM must never be left throttled.
	if got, err := os.ReadFile(memHighPath); err != nil {
		t.Fatal(err)
	} else if string(got) != "max" {
		t.Fatalf("restored memory.high = %q, want %q", got, "max")
	}

	hostOutput := filepath.Join(base, "published", "snapshot.mem")
	guestOutput, finalize, err := prepared.PrepareOutput(hostOutput)
	if err != nil {
		t.Fatal(err)
	}
	if guestOutput != "/snapshots/output-1" {
		t.Fatalf("guest output = %q", guestOutput)
	}
	insideOutput := filepath.Join(root, guestOutput)
	if err := os.WriteFile(insideOutput, []byte("published"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := finalize(); err != nil {
		t.Fatal(err)
	}
	if got, err := os.ReadFile(hostOutput); err != nil || string(got) != "published" {
		t.Fatalf("published output = %q, %v", got, err)
	}

	ln, err := net.ListenUnix("unix", &net.UnixAddr{Name: prepared.HostUFFDPath, Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if err := prepared.ConfigureSocket(prepared.HostUFFDPath); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(prepared.HostUFFDPath)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0600 || statUID(st) != jailUID {
		t.Fatalf("UFFD socket mode/uid = %o/%d", st.Mode().Perm(), statUID(st))
	}

	if err := os.WriteFile(filepath.Join(root, "firecracker.pid"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	if pid, err := prepared.ProcessPID(); err == nil || pid != 0 {
		t.Fatalf("unjailed PID file was trusted: PID=%d err=%v", pid, err)
	}

	prepared.cleanup()
	prepared.cleanup()
	if _, err := os.Stat(filepath.Join(cfg.ChrootBaseDir, "firecracker", req.VMID)); !os.IsNotExist(err) {
		t.Fatalf("jail survived cleanup: %v", err)
	}
	allocs, err := os.ReadDir(filepath.Join(cfg.ChrootBaseDir, ".allocations"))
	if err != nil {
		t.Fatal(err)
	}
	if len(allocs) != 0 {
		t.Fatalf("identity allocation survived cleanup: %v", allocs)
	}
}

func TestJailerRejectsSymlinkInputAndExistingJail(t *testing.T) {
	base := t.TempDir()
	target := writeFixtureFile(t, filepath.Join(base, "target"), "x", 0600)
	link := filepath.Join(base, "link")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if err := validateRegularInput(link); err == nil {
		t.Fatal("symlink input accepted")
	}
}

func TestIdentityReservationIsExclusiveAndPersistent(t *testing.T) {
	cfg := JailerConfig{
		ChrootBaseDir: filepath.Join(t.TempDir(), "jails"),
		UIDStart:      200000,
		GIDStart:      300000,
		IdentityCount: 1,
	}
	uid, gid, release, err := reserveJailerIdentity(cfg, "vm-a")
	if err != nil {
		t.Fatal(err)
	}
	if uid != 200000 || gid != 300000 {
		t.Fatalf("identity = %d:%d", uid, gid)
	}
	if _, _, _, err := reserveJailerIdentity(cfg, "vm-b"); err == nil {
		t.Fatal("double allocation succeeded")
	}
	release()
	if _, _, release2, err := reserveJailerIdentity(cfg, "vm-b"); err != nil {
		t.Fatal(err)
	} else {
		release2()
	}
}

func TestValidateCgroupParentFailsClosed(t *testing.T) {
	cfg := JailerConfig{CgroupRoot: t.TempDir(), CgroupParent: "../../escape", TrustedOwnerUID: os.Geteuid()}
	if err := validateCgroupParent(cfg); err == nil {
		t.Fatal("escaping cgroup parent accepted")
	}
}

// TestMemoryAdmissionFailsClosedOnInconsistentPair pins the gate that keeps the
// admission budget and the per-VM cgroup allowances from drifting apart: an
// under-charging admission (or an unbounded one) lets a fully occupied host sum
// its per-VM memory.max values above the parent task cgroup, whose OOM takes
// serve and every VM with it.
func TestMemoryAdmissionFailsClosedOnInconsistentPair(t *testing.T) {
	// 32 template VMs at 1024+156 fit inside a 40960 MiB task cgroup.
	const taskMaxMIB = 40960
	for name, tc := range map[string]struct {
		allowanceMIB int64
		adm          MemoryAdmission
		wantErr      string
	}{
		"consistent pair": {
			allowanceMIB: 156,
			adm:          MemoryAdmission{BudgetMIB: 37760, ChargedOverheadMIB: 156, TemplateMemMIB: 1024},
		},
		"admission charges less than the cgroup allows": {
			allowanceMIB: 512,
			adm:          MemoryAdmission{BudgetMIB: 37760, ChargedOverheadMIB: 156, TemplateMemMIB: 1024},
			wantErr:      "jailer_memory_overhead_mib",
		},
		"admission disabled": {
			allowanceMIB: 156,
			adm:          MemoryAdmission{BudgetMIB: 0, ChargedOverheadMIB: 156, TemplateMemMIB: 1024},
			wantErr:      "mem_budget_mib must be set positive",
		},
		"budget exceeds the parent cgroup": {
			allowanceMIB: 156,
			adm:          MemoryAdmission{BudgetMIB: 56640, ChargedOverheadMIB: 156, TemplateMemMIB: 1024},
			wantErr:      "exceeds the serve task cgroup memory.max",
		},
	} {
		t.Run(name, func(t *testing.T) {
			err := checkMemoryAdmission(tc.allowanceMIB, tc.adm, taskMaxMIB, "/sys/fs/cgroup/nomad/task")
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("consistent pair rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("inconsistent memory accounting accepted")
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not name the knob to change (%q)", err, tc.wantErr)
			}
		})
	}
}

// TestTaskMemoryMaxRequiresFiniteLimit covers the read CheckMemoryAdmission
// shares with the prerequisite gate: without a finite parent limit neither the
// budget nor the per-VM caps bound anything the kernel enforces.
func TestTaskMemoryMaxRequiresFiniteLimit(t *testing.T) {
	task := t.TempDir()
	if _, err := taskMemoryMaxMIB(task); err == nil {
		t.Fatal("absent memory.max accepted")
	}
	writeFixtureFile(t, filepath.Join(task, "memory.max"), "max", 0644)
	if _, err := taskMemoryMaxMIB(task); err == nil {
		t.Fatal("unlimited memory.max accepted")
	}
	writeFixtureFile(t, filepath.Join(task, "memory.max"), "42949672960\n", 0644)
	got, err := taskMemoryMaxMIB(task)
	if err != nil {
		t.Fatal(err)
	}
	if got != 40960 {
		t.Fatalf("memory.max = %d MiB, want 40960", got)
	}
}

// TestEffectiveMemoryOverheadResolvesZeroToDefault guards the single source of
// truth: the server charges this number, so a zero-valued config must resolve to
// the same default the cgroup writer uses, and an unjailed launch (nil) allows
// no extra memory because it installs no cgroup.
func TestEffectiveMemoryOverheadResolvesZeroToDefault(t *testing.T) {
	var nilCfg *JailerConfig
	if got := nilCfg.EffectiveMemoryOverheadMIB(); got != 0 {
		t.Fatalf("nil (unjailed) overhead = %d, want 0", got)
	}
	if got := (&JailerConfig{}).EffectiveMemoryOverheadMIB(); got != defaultJailerMemoryOverhead {
		t.Fatalf("zero-value overhead = %d, want %d", got, defaultJailerMemoryOverhead)
	}
	if got := (&JailerConfig{MemoryOverheadMIB: 300}).EffectiveMemoryOverheadMIB(); got != 300 {
		t.Fatalf("explicit overhead = %d, want 300", got)
	}
}

func TestJailerCgroupLeafMatchesFirecrackerV115Layout(t *testing.T) {
	cfg := JailerConfig{CgroupRoot: "/sys/fs/cgroup", CgroupParent: "nomad/alloc/task"}
	if got, want := jailerCgroupLeaf(cfg, "vm-123"), "/sys/fs/cgroup/nomad/alloc/task/vm-123"; got != want {
		t.Fatalf("cgroup leaf = %q, want Firecracker v1.15 layout %q", got, want)
	}
}

func writeFixtureFile(t *testing.T, path, contents string, mode os.FileMode) string {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
	return path
}

func assertSameInode(t *testing.T, a, b string) {
	t.Helper()
	ast, err := os.Stat(a)
	if err != nil {
		t.Fatal(err)
	}
	bst, err := os.Stat(b)
	if err != nil {
		t.Fatal(err)
	}
	if ast.Sys().(*syscall.Stat_t).Ino != bst.Sys().(*syscall.Stat_t).Ino {
		t.Fatalf("%s and %s are not the same rootfs inode", a, b)
	}
}

// Delegation must accept the processes serve controls and refuse everything
// else. serve forks short-lived helpers (`cp --reflink` in CloneRootfs, `ip
// tuntap add` in CreateTapUnbridged, iptables in EnsureNetwork) and they land
// in the task cgroup as its children. Treating them as foreign made delegation
// a race against our own subprocesses that failed closed permanently: the
// refusal happens before serve moves into the control leaf, so the short-circuit
// never engages and every later launch re-runs the same losing race.
func TestCgroupProcOursAcceptsOwnFamilyAndRefusesForeign(t *testing.T) {
	const self = 100
	// 1 -> 50 (supervisor) -> 100 (serve) -> 300 (cp) -> 301 (grandchild)
	// 900 is an unrelated tenant under a different root.
	parents := map[int]int{50: 1, 100: 50, 300: 100, 301: 300, 900: 800, 800: 1}
	parent := func(pid int) int { return parents[pid] }
	ancestors := processFamily(self, parent)

	for _, tc := range []struct {
		name string
		pid  int
		want bool
	}{
		{"serve itself", self, true},
		{"shell supervisor (ancestor)", 50, true},
		{"forked helper (child)", 300, true},
		{"helper's child (grandchild)", 301, true},
		{"unrelated tenant", 900, false},
		{"unrelated tenant's parent", 800, false},
		{"pid that no longer resolves", 4242, false},
	} {
		if got := cgroupProcOurs(tc.pid, self, ancestors, parent); got != tc.want {
			t.Errorf("%s: cgroupProcOurs(%d) = %v, want %v", tc.name, tc.pid, got, tc.want)
		}
	}
}

// A cycle in the parent chain must not hang or wrongly claim ownership.
func TestCgroupProcOursSurvivesParentCycle(t *testing.T) {
	const self = 100
	parents := map[int]int{100: 1, 500: 501, 501: 500}
	parent := func(pid int) int { return parents[pid] }
	if cgroupProcOurs(500, self, processFamily(self, parent), parent) {
		t.Fatal("a pid in a parent cycle must not be treated as ours")
	}
}

// Read-only inputs that are identical for every VM must be staged ONCE and
// hardlinked, not copied per VM. This is not a disk-space optimization: the
// Linux page cache is keyed on inode, so per-VM copies make N clones read N
// copies of the same bytes off the disk. Measured on a fleet worker, 16
// concurrent cold reads of one 1 GiB snapshot mem file took 33.7s as 16
// reflinked copies versus 2.9s as 16 hardlinks — and a 16-way fanout sat at
// 45-64% iowait pulling ~500 MB/s.
func TestStageSharedReadonlySharesOneInode(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "mem.bin")
	want := []byte("snapshot memory contents")
	if err := os.WriteFile(src, want, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := JailerConfig{ChrootBaseDir: filepath.Join(dir, "jailer"), TrustedOwnerUID: os.Getuid()}
	if err := os.MkdirAll(cfg.ChrootBaseDir, 0o700); err != nil {
		t.Fatal(err)
	}

	inodes := map[uint64]int{}
	for i := range 8 {
		jail := filepath.Join(cfg.ChrootBaseDir, "firecracker", fmt.Sprintf("vm-%d", i), "root", "snapshots")
		if err := os.MkdirAll(jail, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(jail, "memory")
		if err := stageSharedReadonly(cfg, src, dst, cfg.TrustedOwnerUID); err != nil {
			t.Fatalf("stage %d: %v", i, err)
		}
		got, err := os.ReadFile(dst)
		if err != nil || string(got) != string(want) {
			t.Fatalf("staged content = %q (err %v), want %q", got, err, want)
		}
		st, err := os.Stat(dst)
		if err != nil {
			t.Fatal(err)
		}
		sys, ok := st.Sys().(*syscall.Stat_t)
		if !ok {
			t.Skip("no Stat_t on this platform")
		}
		inodes[uint64(sys.Ino)]++
		if perm := st.Mode().Perm(); perm != 0o444 {
			t.Fatalf("staged mode = %o, want 0444", perm)
		}
	}
	if len(inodes) != 1 {
		t.Fatalf("8 jails used %d distinct inodes, want 1 — page cache is per-inode, so copies defeat sharing", len(inodes))
	}
	for _, n := range inodes {
		if n != 8 {
			t.Fatalf("shared inode linked %d times, want 8", n)
		}
	}

	// The SOURCE artifact must keep its own ownership and mode: guest memory is
	// 0600 on disk. Hardlinking the original would have rewritten it.
	srcInfo, err := os.Stat(src)
	if err != nil {
		t.Fatal(err)
	}
	if perm := srcInfo.Mode().Perm(); perm != 0o600 {
		t.Fatalf("source mode changed to %o; the shared copy must not be a link to the original", perm)
	}
}

// A changed source (rebuilt kernel or golden) must not be served from a stale
// shared copy: identity is (path, size, mtime).
func TestStageSharedReadonlyRestagesChangedSource(t *testing.T) {
	dir := t.TempDir()
	src := filepath.Join(dir, "vmlinux")
	cfg := JailerConfig{ChrootBaseDir: filepath.Join(dir, "jailer"), TrustedOwnerUID: os.Getuid()}
	if err := os.MkdirAll(cfg.ChrootBaseDir, 0o700); err != nil {
		t.Fatal(err)
	}
	stage := func(name string) string {
		jail := filepath.Join(cfg.ChrootBaseDir, name)
		if err := os.MkdirAll(jail, 0o755); err != nil {
			t.Fatal(err)
		}
		dst := filepath.Join(jail, "kernel")
		if err := stageSharedReadonly(cfg, src, dst, cfg.TrustedOwnerUID); err != nil {
			t.Fatalf("stage %s: %v", name, err)
		}
		b, err := os.ReadFile(dst)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}

	if err := os.WriteFile(src, []byte("kernel v1"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := stage("a"); got != "kernel v1" {
		t.Fatalf("first stage = %q", got)
	}
	// Rewrite with a different size and a distinctly newer mtime.
	if err := os.WriteFile(src, []byte("kernel version 2"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(src, time.Now().Add(time.Hour), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if got := stage("b"); got != "kernel version 2" {
		t.Fatalf("after source change, stage = %q — a stale shared copy was reused", got)
	}
}
