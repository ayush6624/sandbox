//go:build linux

package vm

import (
	"context"
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
			})
			if err != nil {
				t.Fatal(err)
			}
			defer m.cleanupLaunch()
			if len(launcher.requests) != 1 || launcher.requests[0].Mode != tt.mode {
				t.Fatalf("requests = %+v, want one %s launch", launcher.requests, tt.mode)
			}
		})
	}
}
