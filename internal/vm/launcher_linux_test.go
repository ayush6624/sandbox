//go:build linux

package vm

import (
	"context"
	"os/exec"
	"testing"
)

func TestSDKConstructorsUseModeAwareLauncher(t *testing.T) {
	tests := []struct {
		name string
		mode LaunchMode
		new  func(context.Context, RunOptions) (*Machine, error)
	}{
		{
			name: "cold boot",
			mode: LaunchColdBoot,
			new: func(ctx context.Context, opts RunOptions) (*Machine, error) {
				m, _, err := NewMachine(ctx, opts, true)
				return m, err
			},
		},
		{
			name: "snapshot restore",
			mode: LaunchSnapshotRestore,
			new: func(ctx context.Context, opts RunOptions) (*Machine, error) {
				m, _, err := NewMachineFromSnapshot(ctx, opts, "/snapshot.mem", "/snapshot.state", true)
				return m, err
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			launcher := &recordingLauncher{}
			m, err := tt.new(context.Background(), RunOptions{
				FirecrackerBin: "/usr/local/bin/firecracker",
				KernelImage:    "/kernel",
				RootfsPath:     "/rootfs",
				LogDir:         t.TempDir(),
				Launcher:       launcher,
				Vcpus:          2,
				MemMIB:         1024,
			})
			if err != nil {
				t.Fatal(err)
			}
			defer m.cleanupLaunch()
			if len(launcher.requests) != 1 || launcher.requests[0].Mode != tt.mode {
				t.Fatalf("requests = %+v, want one %s launch", launcher.requests, tt.mode)
			}
			req := launcher.requests[0]
			if req.KernelImage != "/kernel" || req.RootfsPath != "/rootfs" || req.Vcpus == 0 || req.MemMIB == 0 {
				t.Fatalf("launcher did not receive complete assets/resources: %+v", req)
			}
			if tt.mode == LaunchSnapshotRestore && (req.SnapshotMem != "/snapshot.mem" || req.SnapshotState != "/snapshot.state") {
				t.Fatalf("snapshot assets not carried through launcher: %+v", req)
			}
		})
	}
}

func TestJailedSDKPathsUseLauncherValidation(t *testing.T) {
	launcher := ProcessLauncherFunc(func(ctx context.Context, req LaunchRequest) (PreparedLaunch, error) {
		return PreparedLaunch{
			Command:     exec.CommandContext(ctx, "true"),
			HostAPIPath: "/tmp/host-firecracker.sock",
			Paths: LaunchPaths{
				API: "/run/firecracker.socket", Kernel: "/kernel/vmlinux",
				Rootfs: "/disks/rootfs.ext4",
			},
			OwnsValidation: true,
			Cleanup:        func() {},
		}, nil
	})
	m, _, err := NewMachine(context.Background(), RunOptions{
		FirecrackerBin: "/usr/local/bin/firecracker",
		KernelImage:    "/host/kernel-that-was-validated-before-staging",
		RootfsPath:     "/host/rootfs-that-was-validated-before-staging",
		LogDir:         t.TempDir(),
		Launcher:       launcher,
		Vcpus:          1,
		MemMIB:         128,
	}, false)
	if err != nil {
		t.Fatal(err)
	}
	defer m.cleanupLaunch()
	if !m.Cfg.DisableValidation {
		t.Fatal("SDK host validation remained enabled for jail-visible paths")
	}
	if m.Cfg.KernelImagePath != "/kernel/vmlinux" || *m.Cfg.Drives[0].PathOnHost != "/disks/rootfs.ext4" {
		t.Fatalf("SDK paths were not translated: kernel=%q rootfs=%q", m.Cfg.KernelImagePath, *m.Cfg.Drives[0].PathOnHost)
	}
}
