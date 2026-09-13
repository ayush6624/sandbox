//go:build linux

package vm

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/google/uuid"
)

func kernelCgroupRoot(t *testing.T) string {
	t.Helper()
	root := os.Getenv("SANDBOX_TEST_CGROUP_ROOT")
	if root == "" {
		t.Skip("set SANDBOX_TEST_CGROUP_ROOT to a writable cgroup-v2 directory for kernel tests")
	}
	dir, err := os.MkdirTemp(root, "sandbox-cleanup-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Remove(dir); err != nil {
			t.Errorf("remove test cgroup: %v", err)
		}
	})
	return dir
}

func kernelCgroup(t *testing.T, parent, name string) string {
	t.Helper()
	leaf := filepath.Join(parent, name)
	if err := os.Mkdir(leaf, 0755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := removeVMMCgroup(leaf); err != nil {
			t.Error(err)
		}
	})
	return leaf
}

func populateTestCgroup(t *testing.T, leaf string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command("sleep", "60")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	if err := os.WriteFile(filepath.Join(leaf, "cgroup.procs"), []byte(strconv.Itoa(cmd.Process.Pid)), 0600); err != nil {
		t.Fatal(err)
	}
	return cmd
}

func TestCgroupCleanupKernelBusyRetry(t *testing.T) {
	leaf := kernelCgroup(t, kernelCgroupRoot(t), uuid.NewString())
	cmd := populateTestCgroup(t, leaf)
	if err := os.Remove(leaf); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("populated cgroup removal = %v, want EBUSY", err)
	}
	timer := time.AfterFunc(50*time.Millisecond, func() { _ = cmd.Process.Kill() })
	defer timer.Stop()
	if err := removeVMMCgroup(leaf); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(leaf); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup reservation survived cleanup: %v", err)
	}
	if err := removeVMMCgroup(leaf); err != nil {
		t.Fatalf("repeated cleanup: %v", err)
	}
}

func TestCgroupCleanupKernelBusyTimeoutDoesNotKill(t *testing.T) {
	leaf := kernelCgroup(t, kernelCgroupRoot(t), uuid.NewString())
	cmd := populateTestCgroup(t, leaf)
	if err := removeVMMCgroup(leaf); !errors.Is(err, syscall.EBUSY) {
		t.Fatalf("persistent busy removal = %v, want EBUSY", err)
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("cleanup killed a live process: %v", err)
	}
}

func TestCgroupCleanupKernelReconcileOrphans(t *testing.T) {
	root := kernelCgroupRoot(t)
	parent := kernelCgroup(t, root, "task")
	orphan := kernelCgroup(t, parent, uuid.NewString())
	unrelated := kernelCgroup(t, parent, "sandbox-control")
	cfg := JailerConfig{CgroupRoot: root, CgroupParent: "task", ChrootBaseDir: t.TempDir(), TrustedOwnerUID: os.Geteuid()}
	result, err := ReconcileJailer(cfg)
	if err != nil || result.CgroupsRemoved != 1 {
		t.Fatalf("orphan reconciliation = %+v, %v", result, err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan survived: %v", err)
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated cgroup removed: %v", err)
	}
	result, err = ReconcileJailer(cfg)
	if err != nil || result.CgroupsRemoved != 0 {
		t.Fatalf("repeated reconciliation = %+v, %v", result, err)
	}
}

func TestCgroupCleanupKernelReconcileRefusesLiveOrphan(t *testing.T) {
	root := kernelCgroupRoot(t)
	parent := kernelCgroup(t, root, "task")
	leaf := kernelCgroup(t, parent, uuid.NewString())
	cmd := populateTestCgroup(t, leaf)
	cfg := JailerConfig{CgroupRoot: root, CgroupParent: "task", ChrootBaseDir: t.TempDir(), TrustedOwnerUID: os.Geteuid()}
	if _, err := ReconcileJailer(cfg); err == nil {
		t.Fatal("live orphan was accepted")
	}
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("reconciliation killed an unverified process: %v", err)
	}
}
