package vm

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
)

const (
	defaultJailerUIDStart      = 200000
	defaultJailerIdentityCount = 4096
	// defaultJailerMemoryOverhead is the per-VM cgroup headroom above the
	// guest's mem_mib. It is NOT a free safety margin: the server's memory
	// admission must charge at least this much per VM (see CheckMemoryAdmission),
	// so every MiB here is a MiB of admitted density on the host. Measured
	// per-VM footprint on the production fleet is ~91 MiB total for a 1 GiB
	// guest (docs/benchmarks.md, memory density), so 156 — the value
	// deploy-job.sh has always folded into MEM_PER_SLOT_MIB (1024+156=1180) —
	// both covers the VMM and keeps admission and the cgroups consistent.
	defaultJailerMemoryOverhead = int64(156)
	defaultJailerPIDsMax        = int64(64)
	defaultJailerIOBPS          = int64(256 << 20)
	defaultJailerNoFile         = uint64(256)
	defaultJailerFileSize       = uint64(64 << 30)
)

var delegationMu sync.Mutex

// JailerConfig defines the host isolation policy shared by all launch modes.
// The zero values of limits select conservative production defaults.
type JailerConfig struct {
	JailerBin     string
	ChrootBaseDir string
	UIDStart      int
	GIDStart      int
	IdentityCount int
	CgroupParent  string
	CgroupRoot    string // defaults to /sys/fs/cgroup; injectable for probes/tests
	// MemoryOverheadMIB is the per-VM cgroup allowance above the guest's
	// mem_mib. It is the single source of truth for per-VM overhead: the
	// server's memory admission charges at least this much (see
	// CheckMemoryAdmission), so raising it lowers admitted density instead of
	// letting the per-VM allowances overcommit the parent task cgroup.
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
	task, rel, err := taskCgroupPath(cfg)
	if err != nil {
		return "", err
	}
	if _, err := taskMemoryMaxMIB(task); err != nil {
		return "", err
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

// MemoryAdmission is the server's memory-accounting side of the per-VM memory
// contract, handed to CheckMemoryAdmission so the two halves can be validated
// against each other and against the parent cgroup.
type MemoryAdmission struct {
	// BudgetMIB is the RESOLVED mem_budget_mib: the ceiling on the sum of
	// per-VM charges the registry will admit. <=0 means admission is off (or
	// was derived from /proc/meminfo), which under a finite parent cgroup is
	// itself the inconsistency.
	BudgetMIB int64
	// ChargedOverheadMIB is what admission charges per VM on top of its
	// mem_mib. Must be >= the jailer's per-VM cgroup allowance.
	ChargedOverheadMIB int64
	// TemplateMemMIB is the template guest size; used only to make the error
	// message actionable (it names the per-VM charge an operator must divide
	// the budget by).
	TemplateMemMIB int64
}

// CheckMemoryAdmission fails closed when the server's memory admission and the
// jailer's per-VM cgroup allowances are mutually inconsistent, i.e. when a
// FULLY ADMITTED host would install per-VM memory.max values summing above the
// parent (serve task) cgroup's memory.max. The per-VM caps stop one guest from
// ballooning, but nothing stops the aggregate from exceeding the parent — and
// the parent contains serve, so the parent OOMing kills every VM on the host at
// once instead of failing one create. Two independent numbers used to drift
// here (admission charged 156 MiB, the cgroups allowed 512), which is why the
// allowance is now the single source of truth and this gate refuses to serve
// when the pair cannot hold.
func CheckMemoryAdmission(cfg JailerConfig, adm MemoryAdmission) error {
	cfg.defaults()
	task, _, err := taskCgroupPath(cfg)
	if err != nil {
		return err
	}
	taskMaxMIB, err := taskMemoryMaxMIB(task)
	if err != nil {
		return err
	}
	return checkMemoryAdmission(cfg.EffectiveMemoryOverheadMIB(), adm, taskMaxMIB, task)
}

// checkMemoryAdmission is CheckMemoryAdmission's pure core: it takes the
// numbers rather than reading them, so the failure modes are unit-testable
// without a delegated cgroup.
func checkMemoryAdmission(allowanceMIB int64, adm MemoryAdmission, taskMaxMIB int64, taskPath string) error {
	if adm.ChargedOverheadMIB < allowanceMIB {
		return fmt.Errorf("memory admission charges %d MiB per-VM overhead but each VM's cgroup allows %d MiB (jailer_memory_overhead_mib): at full occupancy the per-VM memory.max values sum above mem_budget_mib and OOM the parent cgroup %s, killing serve and every VM with it — charge at least the allowance or lower jailer_memory_overhead_mib",
			adm.ChargedOverheadMIB, allowanceMIB, taskPath)
	}
	if adm.BudgetMIB <= 0 {
		return fmt.Errorf("mem_budget_mib must be set positive under the jailer: with admission off (or derived from /proc/meminfo, which reports the machine total, not this task's limit) nothing bounds the summed per-VM allowances below the %d MiB memory.max of %s — set mem_budget_mib to at most that minus serve's own footprint (deploy-job.sh: SLOTS_PER_HOST × MEM_PER_SLOT_MIB)",
			taskMaxMIB, taskPath)
	}
	if adm.BudgetMIB > taskMaxMIB {
		return fmt.Errorf("mem_budget_mib %d MiB exceeds the serve task cgroup memory.max %d MiB (%s): a fully admitted host would overcommit the parent cgroup and OOM serve along with every VM — lower SLOTS_PER_HOST × MEM_PER_SLOT_MIB (per-VM charge is %d guest + %d overhead MiB) or raise the task memory",
			adm.BudgetMIB, taskMaxMIB, taskPath, adm.TemplateMemMIB, adm.ChargedOverheadMIB)
	}
	return nil
}

// taskCgroupPath resolves serve's own cgroup — the parent every per-VM leaf is
// charged against — returning its absolute path and the relative name used in
// operator-facing messages. The "sandbox-control" leaf is stripped: serve moves
// itself into that sibling so the VMM leaves are not its own children, but the
// aggregate limit lives one level up.
func taskCgroupPath(cfg JailerConfig) (string, string, error) {
	rel, err := currentUnifiedCgroup()
	if err != nil {
		return "", "", err
	}
	if filepath.Base(rel) == "sandbox-control" {
		rel = filepath.Dir(rel)
	}
	return filepath.Join(cfg.CgroupRoot, rel), rel, nil
}

// taskMemoryMaxMIB reads the parent task cgroup's finite aggregate limit. An
// absent or "max" limit is fatal: without it neither the per-VM caps nor the
// admission budget bound anything the kernel will enforce.
func taskMemoryMaxMIB(task string) (int64, error) {
	raw, err := os.ReadFile(filepath.Join(task, "memory.max"))
	value := strings.TrimSpace(string(raw))
	if err != nil || value == "max" {
		return 0, fmt.Errorf("serve task cgroup must have a finite aggregate memory.max")
	}
	limit, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse task cgroup memory.max %q: %w", value, err)
	}
	return limit >> 20, nil
}

// EffectiveMemoryOverheadMIB is the per-VM memory the jailer's cgroup allows on
// top of the guest's mem_mib, with the zero value resolved to the default. A
// nil config is an unjailed (development) launch, which installs no cgroup and
// therefore allows nothing extra.
func (c *JailerConfig) EffectiveMemoryOverheadMIB() int64 {
	if c == nil {
		return 0
	}
	if c.MemoryOverheadMIB == 0 {
		return defaultJailerMemoryOverhead
	}
	return c.MemoryOverheadMIB
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
	beginSnapshotWrite := snapshotWriteWindow(cfg, req.VMID, req.MemMIB)

	return PreparedLaunch{
		Command:            cmd,
		HostAPIPath:        filepath.Join(rootDir, "run", "firecracker.socket"),
		HostUFFDPath:       filepath.Join(rootDir, "run", "uffd.socket"),
		Paths:              paths,
		PrepareOutput:      prepareOutput,
		BeginSnapshotWrite: beginSnapshotWrite,
		CgroupLeaf:         jailerCgroupLeaf(cfg, req.VMID),
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

// snapshotWriteWindow returns a hook that removes only wbps from a jailed
// VMM's io.max while Firecracker writes a host-controlled snapshot. Snapshot
// callers pause the guest before entering this window, so tenant disk I/O
// cannot escape its normal limit. rbps remains capped throughout.
// snapshotWriteWindow relaxes the jailed VMM's write constraints for the
// duration of one host-requested snapshot, and restores them afterwards.
//
// It covers BOTH sides of the same burst:
//
//   - io.max wbps, so writeback is not bandwidth-throttled. A throttled write
//     makes dirty pages accumulate faster than they drain, which is the memory
//     problem below arriving from the I/O side.
//   - memory.high, so the page cache the write creates is reclaimed under
//     pressure instead of charging up to the hard memory.max. Without this a
//     FULL snapshot kills the VMM: Firecracker writes the guest's whole memory
//     into a file, and cgroup v2 charges that page cache to the same leaf that
//     already holds the guest — a leaf sized at mem_mib + jailer overhead. The
//     kernel's answer at memory.max is the OOM killer, so the VMM died and the
//     sandbox was lost (docs/cgroup-memory-model.md). memory.high makes the
//     kernel throttle and reclaim instead, which is what it exists for.
//
// Diff snapshots never hit this (a few MiB of dirty pages), which is why only
// cold-booted sandboxes — the ones with no golden base to diff against — were
// affected.
//
// It FAILS CLOSED: if the memory guard cannot be installed, the window returns
// an error and vm.Snapshot aborts before issuing /snapshot/create. The VMM is
// still alive and paused at that point, so the caller's resume succeeds and the
// sandbox survives — an error instead of silent destruction.
func snapshotWriteWindow(cfg JailerConfig, vmID string, guestMemMIB int64) func(bool) (func() error, error) {
	leaf := jailerCgroupLeaf(cfg, vmID)
	ioPath := filepath.Join(leaf, "io.max")
	relaxIO := cfg.IODevice != "" && cfg.IOWriteBPS > 0
	limited := ioMaxValue(cfg, false)
	unlimitedWrite := ioMaxValue(cfg, true)
	// A full snapshot writes the whole guest, so that is the page-cache burst to
	// make room for. Captured here because only the launcher knows the guest's
	// size; Snapshot only has to say which kind of snapshot it is taking.
	burst := guestMemMIB << 20

	return func(full bool) (func() error, error) {
		var (
			unlock     func()
			restoreMax = func() error { return nil }
		)
		if full {
			// Serialize full-snapshot windows: each reserves parent-cgroup
			// headroom, and two concurrent windows would each see the same
			// headroom as free. Diff snapshots skip this entirely, so the
			// 8-way-parallel shutdown freeze of golden clones is unaffected.
			snapshotFullWindowMu.Lock()
			unlock = snapshotFullWindowMu.Unlock
			var err error
			restoreMax, err = reserveSnapshotMemory(cfg, leaf, burst)
			if err != nil {
				unlock()
				return nil, err
			}
		}
		restoreHigh, err := armSnapshotMemoryHigh(leaf)
		if err != nil {
			_ = restoreMax()
			if unlock != nil {
				unlock()
			}
			return nil, err
		}
		if relaxIO {
			if err := os.WriteFile(ioPath, []byte(unlimitedWrite), 0600); err != nil {
				_ = restoreHigh()
				_ = restoreMax()
				if unlock != nil {
					unlock()
				}
				return nil, fmt.Errorf("remove snapshot write throttle: %w", err)
			}
		}
		var once sync.Once
		var restoreErr error
		return func() error {
			once.Do(func() {
				var errs []error
				if relaxIO {
					if err := os.WriteFile(ioPath, []byte(limited), 0600); err != nil {
						errs = append(errs, fmt.Errorf("restore snapshot write throttle: %w", err))
					}
				}
				// memory.max comes back BEFORE memory.high is lifted: the
				// ceiling has kept actual usage low throughout, so lowering the
				// hard limit here reclaims nothing and cannot OOM. Lifting the
				// ceiling first would remove that guarantee.
				if err := restoreMax(); err != nil {
					errs = append(errs, err)
				}
				if err := restoreHigh(); err != nil {
					errs = append(errs, err)
				}
				if unlock != nil {
					unlock()
				}
				restoreErr = errors.Join(errs...)
			})
			return restoreErr
		}, nil
	}
}

// snapshotFullWindowMu serializes full-snapshot memory reservations per process.
var snapshotFullWindowMu sync.Mutex

// serveMemoryReserve is what a host keeps for serve itself, mirroring the +2 GiB
// deploy-job.sh adds to TASK_MEMORY on top of SLOTS × MEM_PER_SLOT_MIB. serve
// runs in a sandbox-control leaf with no memory.max of its own, so its footprint
// is not reserved by any leaf limit and must be subtracted explicitly.
const serveMemoryReserve = int64(2 << 30)

// reserveSnapshotMemory raises the VM's memory.max far enough that the whole
// snapshot write could be dirty at once, and returns the restore callback.
//
// memory.high keeps real usage far below this, so the raise is insurance rather
// than consumption — but it is a real reservation against the parent task
// cgroup, so it is bounded by what is genuinely unreserved there. Overcommitting
// the parent is the one failure mode worth refusing service over: it OOMs serve
// and every VM on the host at once.
func reserveSnapshotMemory(cfg JailerConfig, leaf string, burst int64) (func() error, error) {
	limit, err := cgroupLimitBytes(filepath.Join(leaf, "memory.max"))
	if err != nil {
		return nil, fmt.Errorf("read VM memory limit: %w", err)
	}
	if limit <= 0 {
		return func() error { return nil }, nil // unlimited: nothing to reserve
	}
	current, err := cgroupLimitBytes(filepath.Join(leaf, "memory.current"))
	if err != nil {
		return nil, fmt.Errorf("read VM memory usage: %w", err)
	}

	// Room for the guest's footprint plus a fully dirty copy of the write, plus
	// one margin so memory.high has a band to work in above the footprint.
	want := current + burst + snapshotMemoryHighMarginMax
	if want <= limit {
		return func() error { return nil }, nil // the leaf is already big enough
	}
	extra := want - limit

	headroom, err := parentReservableBytes(cfg)
	if err != nil {
		return nil, err
	}
	if extra > headroom {
		return nil, fmt.Errorf("snapshot needs %d MiB beyond this VM's %d MiB limit but only %d MiB is unreserved in the host task cgroup: refusing rather than risking an OOM of serve and every VM (see docs/cgroup-memory-model.md)",
			extra>>20, limit>>20, headroom>>20)
	}

	maxPath := filepath.Join(leaf, "memory.max")
	if err := os.WriteFile(maxPath, []byte(strconv.FormatInt(want, 10)), 0600); err != nil {
		return nil, fmt.Errorf("raise VM memory limit for snapshot: %w", err)
	}
	return func() error {
		if err := os.WriteFile(maxPath, []byte(strconv.FormatInt(limit, 10)), 0600); err != nil {
			return fmt.Errorf("restore VM memory limit after snapshot: %w", err)
		}
		return nil
	}, nil
}

// parentReservableBytes reports how much of the task cgroup is committed to
// nothing yet: its own memory.max, minus every per-VM leaf's memory.max, minus
// serve's reserve.
//
// It deliberately subtracts the leaves' LIMITS rather than their current usage.
// Admitted-but-idle VMs are entitled to their full allowance at any moment, so
// counting only what they happen to be using today would hand out headroom that
// is already promised — exactly the overcommitment CheckMemoryAdmission exists
// to prevent.
//
// A sibling leaf with no finite limit cannot be accounted for and is skipped;
// serveMemoryReserve is what covers serve's own unlimited control leaf.
func parentReservableBytes(cfg JailerConfig) (int64, error) {
	parent := filepath.Join(cfg.CgroupRoot, cfg.CgroupParent)
	parentMax, err := cgroupLimitBytes(filepath.Join(parent, "memory.max"))
	if err != nil {
		return 0, fmt.Errorf("read task cgroup memory.max: %w", err)
	}
	if parentMax <= 0 {
		// No aggregate limit: nothing to overcommit. Production refuses to start
		// in this state (prepareCurrentCgroupDelegation), so this is a
		// development or test host.
		return math.MaxInt64, nil
	}
	entries, err := os.ReadDir(parent)
	if err != nil {
		return 0, fmt.Errorf("list task cgroup children: %w", err)
	}
	var committed int64
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		leafMax, err := cgroupLimitBytes(filepath.Join(parent, entry.Name(), "memory.max"))
		if err != nil || leafMax <= 0 {
			continue
		}
		committed += leafMax
	}
	free := parentMax - committed - serveMemoryReserve
	if free < 0 {
		return 0, nil
	}
	return free, nil
}

// The ceiling sits a MARGIN above the VM's footprint, and the write then
// proceeds as a sawtooth: fill the margin, get throttled, writeback drains,
// reclaim frees, continue.
//
// The margin is a FRACTION of the available slack, not a fixed size, because it
// trades two things off against each other:
//
//   - too small and the write thrashes against continuous reclaim;
//   - too large and throttling starts late, leaving little room between the
//     ceiling and memory.max to absorb the overshoot while writeback catches up.
//
// The second effect is what matters for a tight leaf. A guest that fills most of
// its configured RAM has little slack in total, so most of that slack must stay
// available as absorption room. A fixed 64 MiB margin got 512 and 1024 MiB
// sandboxes working but still lost 256 MiB ones on the fleet, because at that
// size the guest fills its RAM and the margin ate the buffer.
const (
	snapshotMemoryHighSlackDivisor = 4
	snapshotMemoryHighMarginMin    = int64(4 << 20)
	snapshotMemoryHighMarginMax    = int64(64 << 20)
)

// snapshotMemoryHighReserve keeps the ceiling strictly below memory.max when
// even the minimum margin does not fit, so throttling still engages before the
// kernel's hard limit does.
const snapshotMemoryHighReserve = int64(1 << 20)

// snapshotMemoryHighMargin sizes the throttling band for a leaf with `slack`
// bytes between the VM's current footprint and its hard limit.
func snapshotMemoryHighMargin(slack int64) int64 {
	margin := slack / snapshotMemoryHighSlackDivisor
	if margin > snapshotMemoryHighMarginMax {
		margin = snapshotMemoryHighMarginMax
	}
	if margin < snapshotMemoryHighMarginMin {
		margin = snapshotMemoryHighMarginMin
	}
	return margin
}

// armSnapshotMemoryHigh installs a reclaim ceiling just above the VM's current
// footprint and returns the restore callback.
//
// The ceiling is derived from memory.current rather than from mem_mib because
// what matters is what the guest has actually TOUCHED: untouched guest pages map
// to the shared zero page and are never charged, so a configured size tells us
// nothing about the real headroom. Deriving it also means this needs no
// knowledge of the guest's configuration.
//
// It must land ABOVE the current footprint: memory.swap.max is 0, so guest
// anonymous pages cannot be reclaimed at all. A ceiling below them would throttle
// against memory that can never be freed — a hard stall, then the OOM this is
// meant to avoid. Reclaim can only target the snapshot file's page cache, which
// is precisely the intent.
func armSnapshotMemoryHigh(leaf string) (func() error, error) {
	highPath := filepath.Join(leaf, "memory.high")
	previous, err := os.ReadFile(highPath)
	if err != nil {
		return nil, fmt.Errorf("read %s (snapshot would risk OOM-killing the VMM): %w", highPath, err)
	}
	restore := func() error {
		if err := os.WriteFile(highPath, previous, 0600); err != nil {
			return fmt.Errorf("restore snapshot memory ceiling: %w", err)
		}
		return nil
	}

	limit, err := cgroupLimitBytes(filepath.Join(leaf, "memory.max"))
	if err != nil {
		return nil, fmt.Errorf("read VM memory limit: %w", err)
	}
	if limit <= 0 {
		// Unlimited memory.max: nothing can OOM-kill the VMM here, so adding a
		// throttle would only slow the snapshot down for no benefit.
		return func() error { return nil }, nil
	}
	current, err := cgroupLimitBytes(filepath.Join(leaf, "memory.current"))
	if err != nil {
		return nil, fmt.Errorf("read VM memory usage: %w", err)
	}

	if current >= limit {
		// Already at the fence: there is nowhere to put a ceiling, so the
		// kernel is the only thing standing between this write and an OOM kill.
		// Refuse while the VMM is alive and resumable.
		return nil, fmt.Errorf("snapshot cannot proceed safely: the VM is already using %d of its %d MiB limit (see docs/cgroup-memory-model.md)",
			current>>20, limit>>20)
	}

	// Size the band from the slack actually available, then clamp under the
	// fence rather than refusing when even the minimum does not fit: a narrow
	// band still makes the kernel reclaim and throttle BEFORE memory.max, which
	// is strictly better than the unprotected write that used to happen here.
	// Refusing instead would break small sandboxes that already worked — a
	// 128 MiB guest sits close to its own limit because that limit is small.
	high := current + snapshotMemoryHighMargin(limit-current)
	if high >= limit {
		high = limit - snapshotMemoryHighReserve
	}
	if high < current {
		// Never place the ceiling below the current footprint: with no swap the
		// guest's anonymous pages cannot be reclaimed, so that would throttle
		// against memory which can never be freed.
		high = current
	}
	if err := os.WriteFile(highPath, []byte(strconv.FormatInt(high, 10)), 0600); err != nil {
		return nil, fmt.Errorf("arm snapshot memory ceiling (snapshot would risk OOM-killing the VMM): %w", err)
	}
	return restore, nil
}

// cgroupLimitBytes reads a cgroup v2 byte value, returning 0 for "max".
func cgroupLimitBytes(path string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(raw))
	if value == "max" {
		return 0, nil
	}
	return strconv.ParseInt(value, 10, 64)
}

func ioMaxValue(cfg JailerConfig, unlimitedWrite bool) string {
	value := cfg.IODevice
	if cfg.IOReadBPS > 0 {
		value += fmt.Sprintf(" rbps=%d", cfg.IOReadBPS)
	}
	if unlimitedWrite {
		value += " wbps=max"
	} else if cfg.IOWriteBPS > 0 {
		value += fmt.Sprintf(" wbps=%d", cfg.IOWriteBPS)
	}
	return value
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
	allowed := currentProcessFamily()
	var taskPIDs []int
	for _, field := range strings.Fields(string(procs)) {
		pid, parseErr := strconv.Atoi(field)
		if parseErr != nil || !allowed[pid] {
			return "", fmt.Errorf("task cgroup %s contains foreign process %s; refusing delegation", rel, field)
		}
		taskPIDs = append(taskPIDs, pid)
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
	// The production Nomad task may contain a tiny shell supervisor in the
	// serve process's direct ancestor chain. Move that trusted process family
	// together so the aggregate task cgroup is empty before enabling
	// controllers. Any peer or unrelated process still fails closed above.
	self := os.Getpid()
	for _, pid := range taskPIDs {
		if pid == self {
			continue
		}
		if err := os.WriteFile(filepath.Join(control, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0600); err != nil {
			return "", fmt.Errorf("move serve supervisor %d into control cgroup: %w", pid, err)
		}
	}
	if err := os.WriteFile(filepath.Join(control, "cgroup.procs"), []byte(strconv.Itoa(self)), 0600); err != nil {
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

func currentProcessFamily() map[int]bool {
	return processFamily(os.Getpid(), processParentPID)
}

func processFamily(pid int, parent func(int) int) map[int]bool {
	const maxAncestors = 64
	family := make(map[int]bool)
	for range maxAncestors {
		if pid <= 0 || family[pid] {
			break
		}
		family[pid] = true
		pid = parent(pid)
	}
	return family
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

	// Same allowance the server's admission charges for this VM — see
	// CheckMemoryAdmission for why the two must not drift.
	memoryMax := (req.MemMIB + cfg.EffectiveMemoryOverheadMIB()) << 20
	cpuMax := req.Vcpus * cfg.CPUPeriodUS
	settings := []struct {
		file  string
		value string
	}{
		{"memory.max", strconv.FormatInt(memoryMax, 10)},
		{"memory.swap.max", "0"},
		// A running VM is deliberately NOT throttled: it may legitimately use
		// all of mem_mib. This states the baseline explicitly so the snapshot
		// window has a known value to restore, and so the absence of the file
		// is a real error rather than a silently skipped guard — see
		// armSnapshotMemoryHigh.
		{"memory.high", "max"},
		{"pids.max", strconv.FormatInt(cfg.PIDsMax, 10)},
		{"cpu.max", fmt.Sprintf("%d %d", cpuMax, cfg.CPUPeriodUS)},
		{"cpu.weight", strconv.FormatInt(cfg.CPUWeight, 10)},
	}
	if cfg.IODevice != "" && (cfg.IOReadBPS > 0 || cfg.IOWriteBPS > 0) {
		settings = append(settings, struct {
			file  string
			value string
		}{"io.max", ioMaxValue(cfg, false)})
	}
	for _, setting := range settings {
		if err := os.WriteFile(filepath.Join(leaf, setting.file), []byte(setting.value), 0600); err != nil {
			return fmt.Errorf("configure VM cgroup %s: %w", setting.file, err)
		}
	}
	return nil
}
