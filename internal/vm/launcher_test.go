package vm

import (
	"context"
	"os/exec"
	"reflect"
	"testing"
)

func TestDirectLauncherBuildsSecureCommandForEveryMode(t *testing.T) {
	modes := []LaunchMode{
		LaunchColdBoot,
		LaunchSnapshotRestore,
		LaunchHotClone,
		LaunchUFFDRestore,
	}
	for _, mode := range modes {
		t.Run(string(mode), func(t *testing.T) {
			got, err := (directProcessLauncher{}).Prepare(context.Background(), LaunchRequest{
				Mode:           mode,
				FirecrackerBin: "/usr/local/bin/firecracker",
				VMID:           "vm-1",
				HostAPIPath:    "/run/vm-1.sock",
			})
			if err != nil {
				t.Fatal(err)
			}
			want := []string{
				"/usr/local/bin/firecracker",
				"--api-sock", "/run/vm-1.sock",
				"--id", "vm-1",
			}
			if !reflect.DeepEqual(got.Command.Args, want) {
				t.Fatalf("args = %v, want %v", got.Command.Args, want)
			}
			if got.HostAPIPath != "/run/vm-1.sock" {
				t.Fatalf("HostAPIPath = %q", got.HostAPIPath)
			}
		})
	}
}

type recordingLauncher struct {
	requests []LaunchRequest
}

func (l *recordingLauncher) Prepare(ctx context.Context, req LaunchRequest) (PreparedLaunch, error) {
	l.requests = append(l.requests, req)
	return PreparedLaunch{
		Command:     exec.CommandContext(ctx, "true"),
		HostAPIPath: "/jails/" + req.VMID + "/root/run/firecracker.socket",
		Cleanup:     func() {},
	}, nil
}

func TestPrepareLaunchUsesInjectedLauncherAndHostSocket(t *testing.T) {
	launcher := &recordingLauncher{}
	got, err := prepareLaunch(context.Background(), RunOptions{
		FirecrackerBin: "/usr/local/bin/firecracker",
		Launcher:       launcher,
	}, LaunchHotClone, "vm-2", "/run/requested.sock")
	if err != nil {
		t.Fatal(err)
	}
	if len(launcher.requests) != 1 {
		t.Fatalf("requests = %d, want 1", len(launcher.requests))
	}
	req := launcher.requests[0]
	if req.Mode != LaunchHotClone || req.VMID != "vm-2" || req.HostAPIPath != "/run/requested.sock" {
		t.Fatalf("request = %+v", req)
	}
	if got.HostAPIPath != "/jails/vm-2/root/run/firecracker.socket" {
		t.Fatalf("prepared host socket = %q", got.HostAPIPath)
	}
}

func TestPrepareLaunchRejectsIncompletePreparedLaunch(t *testing.T) {
	launcher := ProcessLauncherFunc(func(context.Context, LaunchRequest) (PreparedLaunch, error) {
		return PreparedLaunch{}, nil
	})
	_, err := prepareLaunch(context.Background(), RunOptions{
		FirecrackerBin: "/usr/local/bin/firecracker",
		Launcher:       launcher,
	}, LaunchColdBoot, "vm-3", "/run/vm-3.sock")
	if err == nil {
		t.Fatal("prepareLaunch succeeded with a nil command")
	}
}

// ProcessLauncherFunc keeps launcher tests and future policy decorators small.
type ProcessLauncherFunc func(context.Context, LaunchRequest) (PreparedLaunch, error)

func (f ProcessLauncherFunc) Prepare(ctx context.Context, req LaunchRequest) (PreparedLaunch, error) {
	return f(ctx, req)
}
