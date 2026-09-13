//go:build linux

package vm

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
)

// JailerReconcileResult summarizes abandoned isolation state removed before
// serve admits new VMs.
type JailerReconcileResult struct {
	ProcessesTerminated int
	JailsRemoved        int
	IdentitiesReleased  int
	CgroupsRemoved      int
	// SharedArtifactsRemoved counts shared staged inputs (kernel, snapshot
	// mem/state) reclaimed because no jail links them any more.
	SharedArtifactsRemoved int
}

// ReconcileJailer removes VMMs and reservations owned by a previous serve
// process. The server cannot safely adopt those SDK/raw process handles.
// Process identity is checked against the trusted jail PID file, comm, and the
// configured UID pool before a signal is sent.
func ReconcileJailer(cfg JailerConfig) (JailerReconcileResult, error) {
	cfg.defaults()
	var result JailerReconcileResult
	if cfg.CgroupParent == "" {
		if rel, err := currentUnifiedCgroup(); err == nil {
			if filepath.Base(rel) == "sandbox-control" {
				rel = filepath.Dir(rel)
			}
			cfg.CgroupParent = rel
		}
	}
	if err := ensureTrustedDir(cfg.ChrootBaseDir, cfg.TrustedOwnerUID); err != nil {
		return result, err
	}
	execDir := filepath.Join(cfg.ChrootBaseDir, "firecracker")
	entries, err := os.ReadDir(execDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		vmID := entry.Name()
		if len(vmID) > 64 || strings.Trim(vmID, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789-") != "" {
			continue
		}
		jailDir := filepath.Join(execDir, vmID)
		pidFile := filepath.Join(jailDir, "root", "firecracker.pid")
		if pid, err := readPIDFile(pidFile); err == nil && processAlive(pid) {
			uid := jailedProcessUID(pid)
			if uid < cfg.UIDStart || uid >= cfg.UIDStart+cfg.IdentityCount {
				return result, fmt.Errorf("refusing to release live unverified PID %d from jail %s", pid, vmID)
			}
			if err := validateJailedProcess(pid, uid, filepath.Join(jailDir, "root")); err != nil {
				return result, fmt.Errorf("refusing to release live unverified jail %s: %w", vmID, err)
			}
			parent := processParentPID(pid)
			terminatePID(pid, 2*time.Second)
			if parent > 0 && processAlive(parent) && isJailerProcess(parent) {
				_ = syscall.Kill(parent, syscall.SIGTERM)
				waitPIDExit(parent, 2*time.Second)
			}
			result.ProcessesTerminated++
		}
		if cfg.CgroupParent != "" {
			leaf := jailerCgroupLeaf(cfg, vmID)
			if _, err := os.Lstat(leaf); err == nil {
				if err := removeVMMCgroup(leaf); err != nil {
					return result, err
				}
				result.CgroupsRemoved++
			} else if !errors.Is(err, os.ErrNotExist) {
				return result, err
			}
		}
		if err := os.RemoveAll(jailDir); err != nil {
			return result, fmt.Errorf("remove stale jail %s: %w", jailDir, err)
		}
		result.JailsRemoved++
	}
	// Older cleanup deleted the jail before a failed rmdir, leaving no jail
	// entry to discover. Production VM cgroup names are generated UUIDs.
	if cfg.CgroupParent != "" {
		parent := filepath.Join(cfg.CgroupRoot, cfg.CgroupParent)
		groups, err := os.ReadDir(parent)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		for _, group := range groups {
			id, err := uuid.Parse(group.Name())
			if !group.IsDir() || err != nil || id.String() != group.Name() {
				continue
			}
			leaf := filepath.Join(parent, group.Name())
			events, err := os.ReadFile(filepath.Join(leaf, "cgroup.events"))
			if err != nil {
				return result, fmt.Errorf("inspect orphan cgroup %s: %w", leaf, err)
			}
			if !strings.Contains("\n"+string(events), "\npopulated 0\n") {
				return result, fmt.Errorf("refusing to remove populated or unverifiable orphan cgroup %s", leaf)
			}
			if err := removeVMMCgroup(leaf); err != nil {
				return result, err
			}
			result.CgroupsRemoved++
		}
	}
	allocDir := filepath.Join(cfg.ChrootBaseDir, ".allocations")
	allocs, err := os.ReadDir(allocDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	for _, allocation := range allocs {
		if allocation.IsDir() {
			continue
		}
		if _, err := strconv.Atoi(allocation.Name()); err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(allocDir, allocation.Name())); err != nil && !errors.Is(err, os.ErrNotExist) {
			return result, err
		}
		result.IdentitiesReleased++
	}
	result.SharedArtifactsRemoved = sweepSharedStage(cfg)
	return result, nil
}

// sweepSharedStage drops shared staged artifacts (see stageSharedReadonly) that
// no jail references any more — link count 1 means this directory holds the only
// name for the inode. Every jail has just been removed above, so in practice
// this reclaims the whole set; they are re-staged on demand by the next launch
// at reflink cost (~1 ms), so nothing is lost by being aggressive.
//
// Entries keyed on a stale source mtime (a rebuilt kernel or golden) would
// otherwise linger forever. Best-effort: a shared artifact that cannot be
// removed is a wasted inode, never a correctness problem, and unlinking one a
// live jail still links is harmless anyway — the inode survives until its last
// link goes.
func sweepSharedStage(cfg JailerConfig) int {
	dir := filepath.Join(cfg.ChrootBaseDir, sharedStageDir)
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		info, err := entry.Info()
		if err != nil {
			continue
		}
		st, ok := info.Sys().(*syscall.Stat_t)
		if !ok || st.Nlink != 1 {
			continue
		}
		if err := os.Remove(path); err == nil {
			removed++
		}
	}
	return removed
}

func readPIDFile(path string) (int, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(b)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid PID file %s", path)
	}
	return pid, nil
}

func jailedProcessUID(pid int) int {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return -1
	}
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return -1
		}
		uid, err := strconv.Atoi(fields[1])
		if err == nil {
			return uid
		}
		return -1
	}
	return -1
}

func terminatePID(pid int, grace time.Duration) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if syscall.Kill(pid, 0) != nil {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	waitPIDExit(pid, time.Second)
}

func waitPIDExit(pid int, grace time.Duration) {
	deadline := time.Now().Add(grace)
	for time.Now().Before(deadline) {
		if !processAlive(pid) {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
}
