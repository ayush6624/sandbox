package vm

import (
	"context"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
)

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
	}
	assertSameInode(t, rootfs, filepath.Join(root, "disks", "rootfs.ext4"))

	args := strings.Join(prepared.Command.Args[1:], "\x00")
	for _, want := range []string{
		"--new-pid-ns", "--cgroup-version\x002", "--parent-cgroup\x00nomad/task",
		"memory.max=1342177280", "memory.swap.max=0", "pids.max=64",
		"cpu.max=200000 100000", "cpu.weight=100",
		"io.max=8:16 rbps=10485760 wbps=5242880",
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
	if pid, err := prepared.ProcessPID(); err != nil || pid != os.Getpid() {
		t.Fatalf("child PID = %d, %v", pid, err)
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
