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
	KernelImage    string
	RootfsPath     string
	SnapshotMem    string
	SnapshotState  string
	UFFDHostPath   string
	Vcpus          int64
	MemMIB         int64
}

// LaunchPaths are the paths visible to Firecracker after preparation. HostAPI
// is deliberately separate: the controller dials it outside the jail.
type LaunchPaths struct {
	API           string
	Kernel        string
	Rootfs        string
	SnapshotMem   string
	SnapshotState string
	UFFD          string
}

// PreparedLaunch is a process ready to start plus the host-visible API socket
// the controller must dial. Cleanup must be idempotent and is called only
// after the process has exited, or when preparation/startup fails.
type PreparedLaunch struct {
	Command      *exec.Cmd
	HostAPIPath  string
	HostUFFDPath string
	Paths        LaunchPaths
	// PrepareOutput creates a jail-visible file for Firecracker to write and
	// returns a finalize callback that publishes it at the requested host path.
	// Direct launches leave this nil and write to the host path directly.
	PrepareOutput func(hostPath string) (guestPath string, finalize func() error, err error)
	// ConfigureSocket applies the jailed VMM identity to a controller-created
	// Unix socket (currently UFFD) after it has been bound.
	ConfigureSocket func(hostPath string) error
	// OwnsValidation means preparation validated and staged every filesystem
	// input; SDK host-path validation must be skipped for jail-visible paths.
	OwnsValidation bool
	// ProcessPID returns the Firecracker child PID (not the jailer parent).
	ProcessPID func() (int, error)
	// BeginSnapshotWrite temporarily removes only the jailed VMM's write
	// bandwidth cap while Firecracker writes a host-requested snapshot. The
	// caller must have paused the guest first and must invoke the returned
	// restore callback before resuming it. Direct launches leave this nil.
	BeginSnapshotWrite func() (restore func() error, err error)
	Cleanup            func()
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
	prepared := PreparedLaunch{
		Command:      cmd,
		HostAPIPath:  req.HostAPIPath,
		HostUFFDPath: req.UFFDHostPath,
		Paths: LaunchPaths{
			API:           req.HostAPIPath,
			Kernel:        req.KernelImage,
			Rootfs:        req.RootfsPath,
			SnapshotMem:   req.SnapshotMem,
			SnapshotState: req.SnapshotState,
			UFFD:          req.UFFDHostPath,
		},
		Cleanup: func() {},
	}
	prepared.ProcessPID = func() (int, error) {
		if cmd.Process == nil {
			return 0, fmt.Errorf("firecracker process not started")
		}
		return cmd.Process.Pid, nil
	}
	return prepared, nil
}

func prepareLaunch(ctx context.Context, opts RunOptions, mode LaunchMode, vmID, socketPath string) (PreparedLaunch, error) {
	launcher := opts.Launcher
	if launcher != nil && opts.Jailer != nil {
		return PreparedLaunch{}, fmt.Errorf("Launcher and Jailer are mutually exclusive")
	}
	if launcher == nil && opts.Jailer != nil {
		launcher = newJailerProcessLauncher(*opts.Jailer)
	}
	if launcher == nil {
		launcher = directProcessLauncher{}
	}
	prepared, err := launcher.Prepare(ctx, LaunchRequest{
		Mode:           mode,
		FirecrackerBin: opts.FirecrackerBin,
		VMID:           vmID,
		HostAPIPath:    socketPath,
		DisableSeccomp: opts.DisableSeccomp,
		KernelImage:    opts.KernelImage,
		RootfsPath:     opts.RootfsPath,
		SnapshotMem:    opts.SnapshotMemPath,
		SnapshotState:  opts.SnapshotStatePath,
		UFFDHostPath:   socketPath + ".uffd",
		Vcpus:          opts.Vcpus,
		MemMIB:         opts.MemMIB,
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
	if prepared.Paths.API == "" {
		prepared.Paths.API = prepared.HostAPIPath
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
