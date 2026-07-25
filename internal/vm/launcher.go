package vm

import (
	"context"
	"fmt"
	"os/exec"
)

// LaunchMode identifies the lifecycle path requesting a Firecracker process.
// Jailer preparation depends on the mode because the staged inputs differ.
type LaunchMode string

const (
	LaunchColdBoot        LaunchMode = "cold_boot"
	LaunchSnapshotRestore LaunchMode = "snapshot_restore"
	LaunchHotClone        LaunchMode = "hot_clone"
	LaunchUFFDRestore     LaunchMode = "uffd_restore"
)

// LaunchRequest is the process-level portion of a VMM launch. The jailer
// implementation will grow this request with staged assets and resource
// policy; keeping it mode-aware from the start prevents raw launch paths from
// bypassing future isolation.
type LaunchRequest struct {
	Mode           LaunchMode
	FirecrackerBin string
	VMID           string
	HostAPIPath    string
	DisableSeccomp bool
}

// PreparedLaunch is a process ready to start plus the host-visible API socket
// the controller must dial. Cleanup must be idempotent and is called only
// after the process has exited, or when preparation/startup fails.
type PreparedLaunch struct {
	Command     *exec.Cmd
	HostAPIPath string
	Cleanup     func()
}

// ProcessLauncher prepares the Firecracker process for every lifecycle mode.
// Production jailer support belongs behind this interface; callers must not
// construct Firecracker or jailer commands themselves.
type ProcessLauncher interface {
	Prepare(context.Context, LaunchRequest) (PreparedLaunch, error)
}

type directProcessLauncher struct{}

func (directProcessLauncher) Prepare(ctx context.Context, req LaunchRequest) (PreparedLaunch, error) {
	if req.Mode == "" {
		return PreparedLaunch{}, fmt.Errorf("launch mode is required")
	}
	if req.FirecrackerBin == "" {
		return PreparedLaunch{}, fmt.Errorf("firecracker binary is required")
	}
	if req.VMID == "" {
		return PreparedLaunch{}, fmt.Errorf("VM ID is required")
	}
	if req.HostAPIPath == "" {
		return PreparedLaunch{}, fmt.Errorf("API socket path is required")
	}
	cmd := exec.CommandContext(ctx, req.FirecrackerBin,
		processArgs(req.HostAPIPath, req.VMID, req.DisableSeccomp)...)
	return PreparedLaunch{
		Command:     cmd,
		HostAPIPath: req.HostAPIPath,
		Cleanup:     func() {},
	}, nil
}

func prepareLaunch(ctx context.Context, opts RunOptions, mode LaunchMode, vmID, socketPath string) (PreparedLaunch, error) {
	launcher := opts.Launcher
	if launcher == nil {
		launcher = directProcessLauncher{}
	}
	prepared, err := launcher.Prepare(ctx, LaunchRequest{
		Mode:           mode,
		FirecrackerBin: opts.FirecrackerBin,
		VMID:           vmID,
		HostAPIPath:    socketPath,
		DisableSeccomp: opts.DisableSeccomp,
	})
	if err != nil {
		return PreparedLaunch{}, fmt.Errorf("prepare %s launch: %w", mode, err)
	}
	if prepared.Command == nil {
		prepared.cleanup()
		return PreparedLaunch{}, fmt.Errorf("prepare %s launch: launcher returned nil command", mode)
	}
	if prepared.HostAPIPath == "" {
		prepared.cleanup()
		return PreparedLaunch{}, fmt.Errorf("prepare %s launch: launcher returned empty API path", mode)
	}
	if prepared.Cleanup == nil {
		prepared.Cleanup = func() {}
	}
	return prepared, nil
}

func (p PreparedLaunch) cleanup() {
	if p.Cleanup != nil {
		p.Cleanup()
	}
}
