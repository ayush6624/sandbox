//go:build linux

package vm

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"golang.org/x/sys/unix"
)

func TestFailedLaunchJoinsDistinctFirecrackerChild(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", "sleep 60 & echo $!; wait")
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer cmd.Process.Kill()
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(line))
	if err != nil || pid == cmd.Process.Pid {
		t.Fatalf("child PID %q: %v", line, err)
	}
	defer unix.Kill(pid, unix.SIGKILL)
	childFD, err := unix.PidfdOpen(pid, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(childFD)
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); close(done) }()
	if err := terminateFailedLaunch(cmd, func() (int, error) { return pid, nil }, "", done, time.Second); err != nil {
		t.Fatal(err)
	}
	fds := []unix.PollFd{{Fd: int32(childFD), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 0); err != nil || n != 1 {
		t.Fatalf("returned while Firecracker child could execute: poll=%d err=%v", n, err)
	}
	select {
	case <-done:
	default:
		t.Fatal("launcher was not joined")
	}
}

func TestFailedLaunchRetainsClaimWhenExitCannotBeConfirmed(t *testing.T) {
	for _, reason := range []string{"waiter unfinished", "cgroup occupied", "cgroup unreadable"} {
		t.Run(reason, func(t *testing.T) {
			cmd := exec.Command("/bin/sleep", "60")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			defer cmd.Process.Kill()
			reaped := make(chan struct{})
			go func() { _ = cmd.Wait(); close(reaped) }()
			done := (<-chan struct{})(reaped)
			var cgroup string
			if reason == "waiter unfinished" {
				done = make(chan struct{})
			} else {
				cgroup = t.TempDir()
				path := filepath.Join(cgroup, "cgroup.events")
				if reason == "cgroup occupied" {
					if err := os.WriteFile(path, []byte("populated 1\nfrozen 0\n"), 0600); err != nil {
						t.Fatal(err)
					}
				} else if err := os.Mkdir(path, 0700); err != nil {
					t.Fatal(err)
				}
			}
			if err := terminateFailedLaunch(cmd, nil, cgroup, done, 50*time.Millisecond); !errors.Is(err, ErrLaunchExitUnconfirmed) {
				t.Fatalf("unsafe launch failure should retain its execution claim: %v", err)
			}
			<-reaped
		})
	}
}

func TestSDKFailedStartJoinsProcessAndPreservesConsole(t *testing.T) {
	dir := t.TempDir()
	sock := filepath.Join(dir, "api.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	api := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"test","state":"Not started","vmm_version":"1.0.0","app_name":"Firecracker"}`)
	})}
	go api.Serve(ln)
	defer api.Close()
	var cmd *exec.Cmd
	cleaned := make(chan struct{})
	launcher := ProcessLauncherFunc(func(ctx context.Context, req LaunchRequest) (PreparedLaunch, error) {
		cmd = exec.Command("/bin/sh", "-c", "echo failed-start-console; exec sleep 60")
		return PreparedLaunch{Command: cmd, HostAPIPath: sock, OwnsValidation: true, Cleanup: func() { close(cleaned) }}, nil
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m, _, err := NewMachineFromSnapshot(ctx, RunOptions{Launcher: launcher, LogDir: dir}, "/mem", "/state", true)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("response lost after resume")
	m.Handlers = fcsdk.Handlers{FcInit: (fcsdk.HandlerList{}).Append(fcsdk.StartVMMHandler, fcsdk.Handler{
		Name: "injected-after-process-start",
		Fn:   func(context.Context, *fcsdk.Machine) error { return injected },
	})}
	if err := Start(ctx, m); !errors.Is(err, injected) || errors.Is(err, ErrLaunchExitUnconfirmed) {
		t.Fatalf("failed start: %v", err)
	}
	select {
	case <-cleaned:
	default:
		t.Fatal("SDK failed start returned before cleanup")
	}
	if cmd.ProcessState == nil {
		t.Fatal("SDK process has not been reaped")
	}
	b, err := os.ReadFile(PreserveFailureLog(m))
	if err != nil || !strings.Contains(string(b), "failed-start-console") {
		t.Fatalf("startup console lost: %q, %v", b, err)
	}
}

func TestSDKSocketStartupFailureConfirmsKernelExit(t *testing.T) {
	t.Setenv("FIRECRACKER_GO_SDK_INIT_TIMEOUT_SECONDS", "1")
	dir := t.TempDir()
	var cmd *exec.Cmd
	launcher := ProcessLauncherFunc(func(ctx context.Context, req LaunchRequest) (PreparedLaunch, error) {
		cmd = exec.Command("/bin/sleep", "60")
		return PreparedLaunch{Command: cmd, HostAPIPath: filepath.Join(dir, "missing.sock"), OwnsValidation: true}, nil
	})
	m, _, err := NewMachine(context.Background(), RunOptions{Launcher: launcher, LogDir: dir}, true)
	if err != nil {
		t.Fatal(err)
	}
	m.Handlers = fcsdk.Handlers{FcInit: (fcsdk.HandlerList{}).Append(fcsdk.StartVMMHandler)}
	if err := Start(context.Background(), m); err == nil || errors.Is(err, ErrLaunchExitUnconfirmed) {
		t.Fatalf("socket startup failure was not safely joined: %v", err)
	}
	// In this SDK failure branch, exitCh closes before cmd.Wait completes.
	// Check the kernel rather than treating the SDK's error as an exit proof.
	fd, err := unix.PidfdOpen(cmd.Process.Pid, 0)
	if errors.Is(err, unix.ESRCH) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	fds := []unix.PollFd{{Fd: int32(fd), Events: unix.POLLIN}}
	if n, err := unix.Poll(fds, 0); n != 1 || err != nil {
		t.Fatalf("SDK reported failure while child could still execute: poll=%d err=%v", n, err)
	}
}
