package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	defaultJailerUIDStart       = 200000
	defaultJailerIdentityCount  = 4096
	defaultJailerMemoryOverhead = int64(512)
	defaultJailerPIDsMax        = int64(64)
	defaultJailerIOBPS          = int64(256 << 20)
	defaultJailerNoFile         = uint64(256)
	defaultJailerFileSize       = uint64(64 << 30)
)

var delegationMu sync.Mutex

// JailerConfig defines the host isolation policy shared by all launch modes.
// The zero values of limits select conservative production defaults.
type JailerConfig struct {
	JailerBin         string
	ChrootBaseDir     string
	UIDStart          int
	GIDStart          int
	IdentityCount     int
	CgroupParent      string
	CgroupRoot        string // defaults to /sys/fs/cgroup; injectable for probes/tests
	MemoryOverheadMIB int64
	PIDsMax           int64
	CPUWeight         int64
	CPUPeriodUS       int64
	IODevice          string // cgroup v2 major:minor, e.g. 8:16
	IOReadBPS         int64
	IOWriteBPS        int64
	NoFile            uint64
	FileSize          uint64

	// TrustedOwnerUID exists for rootless unit tests and development probes.
	// Production must leave it at zero.
	TrustedOwnerUID int
}

type jailerProcessLauncher struct {
	cfg JailerConfig
}

func newJailerProcessLauncher(cfg JailerConfig) ProcessLauncher {
	return &jailerProcessLauncher{cfg: cfg}
}

// CheckJailerPrerequisites performs the read-only production gate used by
// doctor and serve before any VMM is admitted.
func CheckJailerPrerequisites(cfg JailerConfig, firecrackerBin, rootfsBase, rootfsDir, snapshotDir string) (string, error) {
	cfg.defaults()
	if os.Geteuid() != 0 {
		return "", fmt.Errorf("jailer production profile requires root")
	}
	if err := validateTrustedFile(cfg.JailerBin, 0, true); err != nil {
		return "", fmt.Errorf("jailer binary: %w", err)
	}
	if err := validateTrustedFile(firecrackerBin, 0, true); err != nil {
		return "", fmt.Errorf("firecracker binary: %w", err)
	}
	jailerVersion, err := executableVersion(cfg.JailerBin)
	if err != nil {
		return "", fmt.Errorf("jailer version: %w", err)
	}
	firecrackerVersion, err := executableVersion(firecrackerBin)
	if err != nil {
		return "", fmt.Errorf("firecracker version: %w", err)
	}
	if jailerVersion != firecrackerVersion {
		return "", fmt.Errorf("jailer %s does not match firecracker %s", jailerVersion, firecrackerVersion)
	}
	st, err := os.Stat(cfg.ChrootBaseDir)
	if err != nil || !st.IsDir() {
		return "", fmt.Errorf("jailer chroot base must already exist: %w", err)
	}
	if err := validateTrustedParents(filepath.Join(cfg.ChrootBaseDir, "sentinel"), 0); err != nil {
		return "", fmt.Errorf("jailer chroot base: %w", err)
	}
	baseDev, err := pathDevice(cfg.ChrootBaseDir)
	if err != nil {
		return "", err
	}
	for name, path := range map[string]string{
		"rootfs base": rootfsBase, "rootfs directory": rootfsDir, "snapshot directory": snapshotDir,
	} {
		dev, err := pathDevice(path)
		if err != nil {
			return "", fmt.Errorf("%s: %w", name, err)
		}
		if dev != baseDev {
			return "", fmt.Errorf("%s %s is not on the jailer's reflink filesystem", name, path)
		}
	}
	if err := checkIdentityRangeUnused(cfg.UIDStart, cfg.IdentityCount); err != nil {
		return "", err
	}
	rel, err := currentUnifiedCgroup()
	if err != nil {
		return "", err
	}
	if filepath.Base(rel) == "sandbox-control" {
		rel = filepath.Dir(rel)
	}
	task := filepath.Join(cfg.CgroupRoot, rel)
	limit, err := os.ReadFile(filepath.Join(task, "memory.max"))
	if err != nil || strings.TrimSpace(string(limit)) == "max" {
		return "", fmt.Errorf("serve task cgroup must have a finite aggregate memory.max")
	}
	controllers, err := os.ReadFile(filepath.Join(task, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("read task cgroup controllers: %w", err)
	}
	have := make(map[string]bool)
	for _, controller := range strings.Fields(string(controllers)) {
		have[controller] = true
	}
	for _, controller := range []string{"cpu", "memory", "pids", "io"} {
		if !have[controller] {
			return "", fmt.Errorf("serve task does not delegate cgroup v2 %s controller", controller)
		}
	}
	return fmt.Sprintf(" (jailer/firecracker %s, task cgroup %s, UID/GID pool %d..%d)",
		firecrackerVersion, rel, cfg.UIDStart, cfg.UIDStart+cfg.IdentityCount-1), nil
}

func executableVersion(path string) (string, error) {
	out, err := exec.Command(path, "--version").CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("%s --version: %w", path, err)
	}
	for _, field := range strings.Fields(string(out)) {
		value := strings.TrimPrefix(field, "v")
		parts := strings.Split(value, ".")
		if len(parts) != 3 {
			continue
		}
		patch := strings.TrimRight(parts[2], ",;)")
		if _, err := strconv.Atoi(parts[0]); err != nil {
			continue
		}
		if _, err := strconv.Atoi(parts[1]); err != nil {
			continue
		}
		if _, err := strconv.Atoi(patch); err == nil {
			return strings.Join([]string{parts[0], parts[1], patch}, "."), nil
		}
	}
	return "", fmt.Errorf("no semantic version in %q", strings.TrimSpace(string(out)))
}

func checkIdentityRangeUnused(start, count int) error {
	b, err := os.ReadFile("/etc/passwd")
	if err != nil {
		return err
	}
	for _, line := range strings.Split(string(b), "\n") {
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err == nil && uid >= start && uid < start+count {
			return fmt.Errorf("jailer UID pool overlaps host account %s (%d)", fields[0], uid)
		}
	}
	return nil
}

func (c *JailerConfig) defaults() {
	if c.JailerBin == "" {
		c.JailerBin = "/usr/local/bin/jailer"
	}
	if c.ChrootBaseDir == "" {
		c.ChrootBaseDir = "/mnt/sandbox-data/jailer"
	}
	if c.UIDStart == 0 {
		c.UIDStart = defaultJailerUIDStart
	}
	if c.GIDStart == 0 {
		c.GIDStart = c.UIDStart
	}
	if c.IdentityCount == 0 {
		c.IdentityCount = defaultJailerIdentityCount
	}
	if c.CgroupRoot == "" {
		c.CgroupRoot = "/sys/fs/cgroup"
	}
	if c.MemoryOverheadMIB == 0 {
		c.MemoryOverheadMIB = defaultJailerMemoryOverhead
	}
	if c.PIDsMax == 0 {
		c.PIDsMax = defaultJailerPIDsMax
	}
	if c.CPUWeight == 0 {
		c.CPUWeight = 100
	}
	if c.CPUPeriodUS == 0 {
		c.CPUPeriodUS = 100000
	}
	if c.IOReadBPS == 0 {
		c.IOReadBPS = defaultJailerIOBPS
	}
	if c.IOWriteBPS == 0 {
		c.IOWriteBPS = defaultJailerIOBPS
	}
	if c.NoFile == 0 {
		c.NoFile = defaultJailerNoFile
	}
	if c.FileSize == 0 {
		c.FileSize = defaultJailerFileSize
	}
}

func (l *jailerProcessLauncher) Prepare(ctx context.Context, req LaunchRequest) (_ PreparedLaunch, retErr error) {
	cfg := l.cfg
	cfg.defaults()
	if err := validateLaunchRequest(req); err != nil {
		return PreparedLaunch{}, err
	}
	if os.Geteuid() != 0 && cfg.TrustedOwnerUID != os.Geteuid() {
		return PreparedLaunch{}, fmt.Errorf("jailer launch requires root (effective uid %d)", os.Geteuid())
	}
	if err := validateTrustedFile(cfg.JailerBin, cfg.TrustedOwnerUID, true); err != nil {
		return PreparedLaunch{}, fmt.Errorf("trusted jailer binary: %w", err)
	}
	if err := validateTrustedFile(req.FirecrackerBin, cfg.TrustedOwnerUID, true); err != nil {
		return PreparedLaunch{}, fmt.Errorf("trusted firecracker binary: %w", err)
	}
	if err := ensureTrustedDir(cfg.ChrootBaseDir, cfg.TrustedOwnerUID); err != nil {
		return PreparedLaunch{}, fmt.Errorf("trusted chroot base: %w", err)
	}
	if cfg.CgroupParent != "" && cfg.TrustedOwnerUID == 0 && cfg.CgroupRoot == "/sys/fs/cgroup" {
		return PreparedLaunch{}, fmt.Errorf("production cgroup parent is auto-detected from the bounded serve task; explicit parent %q is refused", cfg.CgroupParent)
	}
	if cfg.CgroupParent == "" {
		parent, err := prepareCurrentCgroupDelegation(cfg)
		if err != nil {
			return PreparedLaunch{}, err
		}
		cfg.CgroupParent = parent
	}
	if err := validateCgroupParent(cfg); err != nil {
		return PreparedLaunch{}, err
	}
	if cfg.IODevice == "" {
		device, err := backingDevice(req.RootfsPath)
		if err != nil {
			return PreparedLaunch{}, fmt.Errorf("resolve rootfs backing device for io.max: %w", err)
		}
		cfg.IODevice = device
	}
	if err := prepareVMMCgroup(cfg, req); err != nil {
		return PreparedLaunch{}, err
	}
	cleanupCgroup := true
	defer func() {
		if retErr != nil && cleanupCgroup {
			_ = os.Remove(jailerCgroupLeaf(cfg, req.VMID))
		}
	}()

	uid, gid, releaseIdentity, err := reserveJailerIdentity(cfg, req.VMID)
	if err != nil {
		return PreparedLaunch{}, err
	}
	cleanupIdentity := true
	defer func() {
		if retErr != nil && cleanupIdentity {
			releaseIdentity()
		}
	}()

	execName := filepath.Base(req.FirecrackerBin)
	jailDir := filepath.Join(cfg.ChrootBaseDir, execName, req.VMID)
	rootDir := filepath.Join(jailDir, "root")
	if _, err := os.Lstat(jailDir); err == nil {
		releaseIdentity()
		cleanupIdentity = false
		return PreparedLaunch{}, fmt.Errorf("refusing to reuse pre-existing jail %s", jailDir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return PreparedLaunch{}, err
	}
	if err := prepareJailDirs(rootDir); err != nil {
		return PreparedLaunch{}, fmt.Errorf("prepare jail directories: %w", err)
	}
	cleanupJail := func() {
		_ = os.RemoveAll(jailDir)
		_ = os.Remove(jailerCgroupLeaf(cfg, req.VMID))
		releaseIdentity()
	}
	cleanupCgroup = false
	cleanupIdentity = false
	if err := os.Chown(filepath.Join(rootDir, "run"), uid, gid); err != nil {
		cleanupJail()
		return PreparedLaunch{}, fmt.Errorf("own jail runtime directory: %w", err)
	}
	if err := os.Chmod(filepath.Join(rootDir, "run"), 0700); err != nil {
		cleanupJail()
		return PreparedLaunch{}, fmt.Errorf("protect jail runtime directory: %w", err)
	}

	paths := LaunchPaths{
		API:           "/run/firecracker.socket",
		Kernel:        "/kernel/vmlinux",
		Rootfs:        "/disks/rootfs.ext4",
		SnapshotMem:   "/snapshots/memory",
		SnapshotState: "/snapshots/state",
		UFFD:          "/run/uffd.socket",
	}
	if err := stageReadonly(req.KernelImage, filepath.Join(rootDir, paths.Kernel), cfg.TrustedOwnerUID); err != nil {
		cleanupJail()
		return PreparedLaunch{}, fmt.Errorf("stage kernel: %w", err)
	}
	if err := stageWritableRootfs(req.RootfsPath, filepath.Join(rootDir, paths.Rootfs), uid, gid); err != nil {
		cleanupJail()
		return PreparedLaunch{}, fmt.Errorf("stage rootfs: %w", err)
	}
	if req.SnapshotMem != "" {
		if err := stageReadonly(req.SnapshotMem, filepath.Join(rootDir, paths.SnapshotMem), cfg.TrustedOwnerUID); err != nil {
			cleanupJail()
			return PreparedLaunch{}, fmt.Errorf("stage snapshot memory: %w", err)
		}
	}
	if req.SnapshotState != "" {
		if err := stageReadonly(req.SnapshotState, filepath.Join(rootDir, paths.SnapshotState), cfg.TrustedOwnerUID); err != nil {
			cleanupJail()
			return PreparedLaunch{}, fmt.Errorf("stage snapshot state: %w", err)
		}
	}

	args := jailerArgs(cfg, req, uid, gid, paths.API)
	cmd := exec.CommandContext(ctx, cfg.JailerBin, args...)
	cmd.SysProcAttr = jailerSysProcAttr()

	var outputMu sync.Mutex
	outputSeq := 0
	prepareOutput := func(hostPath string) (string, func() error, error) {
		outputMu.Lock()
		defer outputMu.Unlock()
		if hostPath == "" || !filepath.IsAbs(hostPath) {
			return "", nil, fmt.Errorf("snapshot output must be an absolute path")
		}
		outputSeq++
		name := fmt.Sprintf("output-%d", outputSeq)
		inside := filepath.Join(rootDir, "snapshots", name)
		f, err := os.OpenFile(inside, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return "", nil, err
		}
		if err := f.Close(); err != nil {
			return "", nil, err
		}
		if err := os.Chown(inside, uid, gid); err != nil {
			return "", nil, err
		}
		finalize := func() error {
			if err := os.MkdirAll(filepath.Dir(hostPath), 0750); err != nil {
				return err
			}
			if err := os.Rename(inside, hostPath); err == nil {
				return nil
			}
			if err := copyFile(inside, hostPath, 0600); err != nil {
				return err
			}
			return os.Remove(inside)
		}
		return "/snapshots/" + name, finalize, nil
	}

	return PreparedLaunch{
		Command:       cmd,
		HostAPIPath:   filepath.Join(rootDir, "run", "firecracker.socket"),
		HostUFFDPath:  filepath.Join(rootDir, "run", "uffd.socket"),
		Paths:         paths,
		PrepareOutput: prepareOutput,
		ConfigureSocket: func(hostPath string) error {
			if filepath.Clean(hostPath) != filepath.Join(rootDir, "run", "uffd.socket") {
				return fmt.Errorf("refusing to configure unexpected jail socket %s", hostPath)
			}
			if err := os.Chown(hostPath, uid, gid); err != nil {
				return err
			}
			return os.Chmod(hostPath, 0600)
		},
		OwnsValidation: true,
		ProcessPID: func() (int, error) {
			b, err := os.ReadFile(filepath.Join(rootDir, execName+".pid"))
			if err != nil {
				return 0, err
			}
			pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
			if err != nil || pid <= 0 {
				return 0, fmt.Errorf("invalid firecracker child PID %q", strings.TrimSpace(string(b)))
			}
			if err := validateJailedProcess(pid, uid, rootDir); err != nil {
				return 0, err
			}
			return pid, nil
		},
		Cleanup: cleanupJail,
	}, nil
}

func validateLaunchRequest(req LaunchRequest) error {
	if req.Mode == "" || req.FirecrackerBin == "" || req.VMID == "" || req.HostAPIPath == "" {
		return fmt.Errorf("mode, firecracker binary, VM ID, and API socket are required")
	}
	if len(req.VMID) > 64 || strings.Trim(req.VMID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
		return fmt.Errorf("VM ID %q is not jailer-safe", req.VMID)
	}
	if req.KernelImage == "" || req.RootfsPath == "" {
		return fmt.Errorf("kernel and rootfs are required for jailed launches")
	}
	if req.Vcpus <= 0 || req.MemMIB <= 0 {
		return fmt.Errorf("positive vcpus and memory are required for jailed launches")
	}
	return nil
}

func prepareJailDirs(root string) error {
	for _, dir := range []string{
		root,
		filepath.Join(root, "run"),
		filepath.Join(root, "kernel"),
		filepath.Join(root, "disks"),
		filepath.Join(root, "snapshots"),
	} {
		if err := os.MkdirAll(dir, 0711); err != nil {
			return err
		}
		if err := os.Chmod(dir, 0711); err != nil {
			return err
		}
	}
	return nil
}

func stageWritableRootfs(src, dst string, uid, gid int) error {
	if err := validateRegularInput(src); err != nil {
		return err
	}
	if err := os.Link(src, dst); err != nil {
		return fmt.Errorf("hard-link per-VM rootfs (jail base must share its filesystem): %w", err)
	}
	if err := os.Chown(dst, uid, gid); err != nil {
		return err
	}
	return os.Chmod(dst, 0600)
}

func stageReadonly(src, dst string, trustedOwner int) error {
	if err := validateRegularInput(src); err != nil {
		return err
	}
	if err := reflinkOrCopy(src, dst); err != nil {
		return err
	}
	if err := os.Chown(dst, trustedOwner, -1); err != nil {
		return err
	}
	return os.Chmod(dst, 0444)
}

func reflinkOrCopy(src, dst string) error {
	const cp = "/usr/bin/cp"
	if err := validateTrustedFile(cp, 0, true); err == nil {
		cmd := exec.Command(cp, "--reflink=auto", "--sparse=always", "--", src, dst)
		if out, err := cmd.CombinedOutput(); err == nil {
			return nil
		} else if len(out) > 0 {
			_ = os.Remove(dst)
		}
	}
	return copyFile(src, dst, 0600)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = out.Close()
		if !ok {
			_ = os.Remove(dst)
		}
	}()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	if err := out.Sync(); err != nil {
		return err
	}
	ok = true
	return out.Close()
}

func validateRegularInput(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not absolute", path)
	}
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() {
		return fmt.Errorf("%q is not a regular file", path)
	}
	return nil
}

func validateTrustedFile(path string, owner int, executable bool) error {
	if err := validateRegularInput(path); err != nil {
		return err
	}
	st, err := os.Stat(path)
	if err != nil {
		return err
	}
	if statUID(st) != owner {
		return fmt.Errorf("%s uid is %d, want %d", path, statUID(st), owner)
	}
	if st.Mode().Perm()&0022 != 0 {
		return fmt.Errorf("%s is group/other writable", path)
	}
	if executable && st.Mode().Perm()&0111 == 0 {
		return fmt.Errorf("%s is not executable", path)
	}
	return validateTrustedParents(path, owner)
}

func ensureTrustedDir(path string, owner int) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not absolute", path)
	}
	if err := os.MkdirAll(path, 0755); err != nil {
		return err
	}
	if err := os.Chmod(path, 0755); err != nil {
		return err
	}
	return validateTrustedParents(filepath.Join(path, "sentinel"), owner)
}

func validateTrustedParents(path string, owner int) error {
	dir := filepath.Dir(filepath.Clean(path))
	for {
		st, err := os.Stat(dir)
		if err != nil {
			return err
		}
		// /tmp-style sticky directories are safe parents for tests only when
		// the configured owner is the current user; production owner remains 0.
		stickySafe := st.Mode()&os.ModeSticky != 0 && owner == os.Geteuid()
		if lst, lerr := os.Lstat(dir); lerr != nil {
			return lerr
		} else if lst.Mode()&os.ModeSymlink != 0 && owner == 0 {
			return fmt.Errorf("trusted parent %s is a symlink", dir)
		}
		ownerSafe := statUID(st) == owner || (owner != 0 && statUID(st) == 0)
		if !ownerSafe && !stickySafe {
			return fmt.Errorf("parent %s uid is %d, want %d", dir, statUID(st), owner)
		}
		if st.Mode().Perm()&0022 != 0 && !stickySafe {
			return fmt.Errorf("parent %s is group/other writable", dir)
		}
		if dir == "/" {
			break
		}
		dir = filepath.Dir(dir)
	}
	return nil
}

func validateCgroupParent(cfg JailerConfig) error {
	if cfg.CgroupParent == "" || filepath.IsAbs(cfg.CgroupParent) || strings.Contains(cfg.CgroupParent, "..") {
		return fmt.Errorf("jailer cgroup parent must be a trusted relative delegated cgroup v2 path")
	}
	parent := filepath.Join(cfg.CgroupRoot, cfg.CgroupParent)
	st, err := os.Stat(parent)
	if err != nil {
		return fmt.Errorf("jailer cgroup parent: %w", err)
	}
	if !st.IsDir() {
		return fmt.Errorf("jailer cgroup parent %s is not a directory", parent)
	}
	if err := validateTrustedParents(filepath.Join(parent, "sentinel"), cfg.TrustedOwnerUID); err != nil {
		return fmt.Errorf("jailer cgroup parent: %w", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.CgroupRoot, "cgroup.controllers")); err != nil {
		return fmt.Errorf("cgroup v2 unified hierarchy unavailable: %w", err)
	}
	return nil
}

// prepareCurrentCgroupDelegation turns the current Nomad task cgroup into an
// aggregate parent: serve is moved into a control leaf, then cpu/memory/pids/io
// are delegated to sibling per-VM leaves created by jailer. This preserves the
// task's memory.max and avoids the unsafe top-level /sys/fs/cgroup/firecracker
// default. It fails closed if the task is not a bounded, exclusive leaf.
func prepareCurrentCgroupDelegation(cfg JailerConfig) (string, error) {
	delegationMu.Lock()
	defer delegationMu.Unlock()

	rel, err := currentUnifiedCgroup()
	if err != nil {
		return "", err
	}
	const controlLeaf = "sandbox-control"
	if filepath.Base(rel) == controlLeaf {
		return filepath.Dir(rel), nil
	}
	parent := filepath.Join(cfg.CgroupRoot, rel)
	limit, err := os.ReadFile(filepath.Join(parent, "memory.max"))
	if err != nil {
		return "", fmt.Errorf("read task memory.max: %w", err)
	}
	if strings.TrimSpace(string(limit)) == "max" {
		return "", fmt.Errorf("task cgroup %s has no aggregate memory.max", rel)
	}
	procs, err := os.ReadFile(filepath.Join(parent, "cgroup.procs"))
	if err != nil {
		return "", fmt.Errorf("read task cgroup.procs: %w", err)
	}
	for _, field := range strings.Fields(string(procs)) {
		if field != strconv.Itoa(os.Getpid()) {
			return "", fmt.Errorf("task cgroup %s contains foreign process %s; refusing delegation", rel, field)
		}
	}
	controllers, err := os.ReadFile(filepath.Join(parent, "cgroup.controllers"))
	if err != nil {
		return "", fmt.Errorf("read delegated controllers: %w", err)
	}
	have := make(map[string]bool)
	for _, controller := range strings.Fields(string(controllers)) {
		have[controller] = true
	}
	required := []string{"cpu", "memory", "pids", "io"}
	for _, controller := range required {
		if !have[controller] {
			return "", fmt.Errorf("task cgroup %s does not delegate %s", rel, controller)
		}
	}
	control := filepath.Join(parent, controlLeaf)
	if err := os.Mkdir(control, 0755); err != nil && !errors.Is(err, os.ErrExist) {
		return "", fmt.Errorf("create serve control cgroup: %w", err)
	}
	if err := os.WriteFile(filepath.Join(control, "cgroup.procs"), []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		return "", fmt.Errorf("move serve into control cgroup: %w", err)
	}
	var enable strings.Builder
	for _, controller := range required {
		fmt.Fprintf(&enable, "+%s ", controller)
	}
	if err := os.WriteFile(filepath.Join(parent, "cgroup.subtree_control"), []byte(strings.TrimSpace(enable.String())), 0600); err != nil {
		return "", fmt.Errorf("enable per-VM cgroup controllers: %w", err)
	}
	return rel, nil
}

func currentUnifiedCgroup() (string, error) {
	b, err := os.ReadFile("/proc/self/cgroup")
	if err != nil {
		return "", fmt.Errorf("read current cgroup: %w", err)
	}
	for _, line := range strings.Split(string(b), "\n") {
		if !strings.HasPrefix(line, "0::") {
			continue
		}
		rel := strings.TrimPrefix(filepath.Clean(strings.TrimPrefix(line, "0::")), "/")
		if rel == "" || rel == "." {
			break
		}
		return rel, nil
	}
	return "", fmt.Errorf("serve must run in a non-root delegated cgroup v2 task")
}

// jailerCgroupLeaf matches Firecracker v1.15's CgroupV2::new layout:
// <unified mount>/<--parent-cgroup>/<--id>. Supplying --parent-cgroup replaces
// the default "firecracker" parent; it does not add another component.
func jailerCgroupLeaf(cfg JailerConfig, vmID string) string {
	return filepath.Join(cfg.CgroupRoot, cfg.CgroupParent, vmID)
}

func reserveJailerIdentity(cfg JailerConfig, vmID string) (int, int, func(), error) {
	if cfg.IdentityCount <= 0 || cfg.UIDStart <= 0 || cfg.GIDStart <= 0 {
		return 0, 0, nil, fmt.Errorf("invalid jailer identity pool")
	}
	dir := filepath.Join(cfg.ChrootBaseDir, ".allocations")
	if err := os.MkdirAll(dir, 0700); err != nil {
		return 0, 0, nil, err
	}
	start := stableSlot(vmID, cfg.IdentityCount)
	for i := 0; i < cfg.IdentityCount; i++ {
		slot := (start + i) % cfg.IdentityCount
		uid, gid := cfg.UIDStart+slot, cfg.GIDStart+slot
		path := filepath.Join(dir, strconv.Itoa(uid))
		f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if errors.Is(err, os.ErrExist) {
			continue
		}
		if err != nil {
			return 0, 0, nil, err
		}
		_, writeErr := io.WriteString(f, vmID+"\n")
		closeErr := f.Close()
		if writeErr != nil || closeErr != nil {
			_ = os.Remove(path)
			return 0, 0, nil, errors.Join(writeErr, closeErr)
		}
		var once sync.Once
		return uid, gid, func() { once.Do(func() { _ = os.Remove(path) }) }, nil
	}
	return 0, 0, nil, fmt.Errorf("jailer identity pool exhausted (%d identities)", cfg.IdentityCount)
}

func stableSlot(id string, n int) int {
	var h uint32 = 2166136261
	for i := 0; i < len(id); i++ {
		h ^= uint32(id[i])
		h *= 16777619
	}
	return int(h % uint32(n))
}

func jailerArgs(cfg JailerConfig, req LaunchRequest, uid, gid int, apiPath string) []string {
	args := []string{
		"--id", req.VMID,
		"--exec-file", req.FirecrackerBin,
		"--uid", strconv.Itoa(uid),
		"--gid", strconv.Itoa(gid),
		"--chroot-base-dir", cfg.ChrootBaseDir,
		"--new-pid-ns",
		"--cgroup-version", "2",
		// With no --cgroup arguments, Firecracker 1.15 moves the process into
		// this existing cgroup-v2 leaf. The controller prepares the leaf first
		// because jailer's key=value parser cannot represent io.max values
		// containing the required rbps=/wbps= tokens.
		"--parent-cgroup", filepath.Join(cfg.CgroupParent, req.VMID),
	}
	args = append(args,
		"--resource-limit", fmt.Sprintf("no-file=%d", cfg.NoFile),
		"--resource-limit", fmt.Sprintf("fsize=%d", cfg.FileSize),
		"--",
		"--api-sock", apiPath,
	)
	if req.DisableSeccomp {
		args = append(args, "--no-seccomp")
	}
	return args
}

func prepareVMMCgroup(cfg JailerConfig, req LaunchRequest) (retErr error) {
	leaf := jailerCgroupLeaf(cfg, req.VMID)
	if err := os.Mkdir(leaf, 0755); err != nil {
		if errors.Is(err, os.ErrExist) {
			return fmt.Errorf("refusing to reuse pre-existing VM cgroup %s", leaf)
		}
		return fmt.Errorf("create VM cgroup: %w", err)
	}
	defer func() {
		if retErr != nil {
			_ = os.Remove(leaf)
		}
	}()

	memoryMax := (req.MemMIB + cfg.MemoryOverheadMIB) << 20
	cpuMax := req.Vcpus * cfg.CPUPeriodUS
	settings := []struct {
		file  string
		value string
	}{
		{"memory.max", strconv.FormatInt(memoryMax, 10)},
		{"memory.swap.max", "0"},
		{"pids.max", strconv.FormatInt(cfg.PIDsMax, 10)},
		{"cpu.max", fmt.Sprintf("%d %d", cpuMax, cfg.CPUPeriodUS)},
		{"cpu.weight", strconv.FormatInt(cfg.CPUWeight, 10)},
	}
	if cfg.IODevice != "" && (cfg.IOReadBPS > 0 || cfg.IOWriteBPS > 0) {
		value := cfg.IODevice
		if cfg.IOReadBPS > 0 {
			value += fmt.Sprintf(" rbps=%d", cfg.IOReadBPS)
		}
		if cfg.IOWriteBPS > 0 {
			value += fmt.Sprintf(" wbps=%d", cfg.IOWriteBPS)
		}
		settings = append(settings, struct {
			file  string
			value string
		}{"io.max", value})
	}
	for _, setting := range settings {
		if err := os.WriteFile(filepath.Join(leaf, setting.file), []byte(setting.value), 0600); err != nil {
			return fmt.Errorf("configure VM cgroup %s: %w", setting.file, err)
		}
	}
	return nil
}
