//go:build linux

package vm

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

func validateJailedProcess(pid, uid int, rootDir string) error {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return err
	}
	if strings.TrimSpace(string(comm)) != "firecracker" {
		return fmt.Errorf("PID %d is not firecracker", pid)
	}
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return err
	}
	foundUID := false
	for _, line := range strings.Split(string(status), "\n") {
		if !strings.HasPrefix(line, "Uid:") {
			continue
		}
		fields := strings.Fields(line)
		actual, parseErr := strconv.Atoi(fields[1])
		if parseErr != nil || actual != uid {
			return fmt.Errorf("firecracker PID %d uid does not match reserved uid %d", pid, uid)
		}
		foundUID = true
		break
	}
	if !foundUID {
		return fmt.Errorf("firecracker PID %d has no readable uid", pid)
	}
	procRoot, err := os.Stat(fmt.Sprintf("/proc/%d/root", pid))
	if err != nil {
		return err
	}
	wantRoot, err := os.Stat(rootDir)
	if err != nil {
		return err
	}
	if !os.SameFile(procRoot, wantRoot) {
		return fmt.Errorf("firecracker PID %d root does not match jail %s", pid, rootDir)
	}
	return nil
}

func processAlive(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func processParentPID(pid int) int {
	status, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(status), "\n") {
		if strings.HasPrefix(line, "PPid:") {
			fields := strings.Fields(line)
			parent, _ := strconv.Atoi(fields[1])
			return parent
		}
	}
	return 0
}

func isJailerProcess(pid int) bool {
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	return err == nil && strings.TrimSpace(string(comm)) == "jailer" && jailedProcessUID(pid) == 0
}
