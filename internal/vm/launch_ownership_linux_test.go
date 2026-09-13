//go:build linux

package vm

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestRawFailedLaunchReturnsOwnershipUntilExitConfirmed(t *testing.T) {
	for _, launch := range []string{"clone-file", "clone-uffd", "restore-uffd"} {
		failures := []string{"api", "/snapshot/load"}
		if launch != "restore-uffd" {
			failures = append(failures, "/drives/rootfs", "/mmds", "/vm")
		}
		for _, failAt := range failures {
			for _, unconfirmed := range []bool{false, true} {
				name := launch + "/" + failAt + "/confirmed"
				if unconfirmed {
					name = launch + "/" + failAt + "/unconfirmed"
				}
				t.Run(name, func(t *testing.T) {
					dir := t.TempDir()
					sock := filepath.Join(dir, "api")
					ln, err := net.Listen("unix", sock)
					if err != nil {
						t.Fatal(err)
					}
					api := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						if r.URL.Path == failAt {
							http.Error(w, "injected launch failure", http.StatusInternalServerError)
							return
						}
						w.WriteHeader(http.StatusNoContent)
					})}
					go api.Serve(ln)
					defer api.Close()

					// Model a jailer child outside the launcher's process group.
					child := exec.Command("/bin/sleep", "60")
					if err := child.Start(); err != nil {
						t.Fatal(err)
					}
					childDone := make(chan struct{})
					go func() { _ = child.Wait(); close(childDone) }()
					defer func() { _ = child.Process.Kill(); <-childDone }()
					childFD, err := unix.PidfdOpen(child.Process.Pid, 0)
					if err != nil {
						t.Fatal(err)
					}
					defer unix.Close(childFD)
					var pidAvailable atomic.Bool
					pidAvailable.Store(!unconfirmed)
					var cleaned atomic.Int32
					var request LaunchRequest
					var cmd *exec.Cmd
					launcher := ProcessLauncherFunc(func(_ context.Context, req LaunchRequest) (PreparedLaunch, error) {
						request = req
						cmd = exec.Command("/bin/sleep", "60")
						return PreparedLaunch{
							Command: cmd, HostAPIPath: sock, HostUFFDPath: sock + ".uffd",
							Paths: LaunchPaths{SnapshotMem: "/memory", SnapshotState: "/state", Rootfs: "/rootfs", UFFD: "/uffd"},
							ProcessPID: func() (int, error) {
								if !pidAvailable.Load() {
									return 0, errors.New("injected unavailable child PID")
								}
								return child.Process.Pid, nil
							},
							Cleanup: func() { cleaned.Add(1) },
						}, nil
					})
					opts := RunOptions{Launcher: launcher, LogDir: dir, UFFDChunks: &UFFDChunkSource{
						Total: 4096, ChunkSize: 4096, Load: func(uint64) ([]byte, error) { return make([]byte, 4096), nil },
					}}
					ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
					defer cancel()
					if failAt == "api" {
						cancel()
					}
					var m *Machine
					var rt RuntimeConfig
					switch launch {
					case "clone-file":
						m, rt, err = StartClone(ctx, opts, CloneParams{})
					case "clone-uffd":
						m, rt, err = StartCloneUFFD(ctx, opts, CloneParams{})
					case "restore-uffd":
						m, rt, err = RestoreUFFD(ctx, opts, "", "")
					}
					if err == nil || errors.Is(err, ErrLaunchExitUnconfirmed) != unconfirmed {
						t.Fatalf("launch error = %v; unconfirmed = %v", err, unconfirmed)
					}
					if failAt == "api" {
						if !errors.Is(err, context.Canceled) {
							t.Fatalf("lost request cancellation: %v", err)
						}
					} else if !strings.Contains(err.Error(), "injected launch failure") {
						t.Fatalf("lost launch failure: %v", err)
					}
					if unconfirmed {
						if m == nil || m.raw == nil || m.raw.cmd != cmd {
							t.Fatal("unconfirmed launch lost its process handle")
						}
						if rt.VMID == "" || rt.VMID != request.VMID || rt.SocketPath != sock || m.raw.sock != sock {
							t.Fatalf("unconfirmed launch lost its runtime identity: %+v", rt)
						}
						fds := []unix.PollFd{{Fd: int32(childFD), Events: unix.POLLIN}}
						if n, err := unix.Poll(fds, 0); n != 0 || err != nil {
							t.Fatalf("expected surviving child: poll=%d err=%v", n, err)
						}
						select {
						case <-m.raw.processDone:
						case <-time.After(time.Second):
							t.Fatal("launcher did not exit")
						}
						proofCtx, proofCancel := context.WithTimeout(context.Background(), time.Second)
						defer proofCancel()
						if err := Wait(proofCtx, m); !errors.Is(err, ErrLaunchExitUnconfirmed) {
							t.Fatalf("missing child PID was treated as exit: %v", err)
						}
						pidAvailable.Store(true)
						liveCtx, liveCancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
						defer liveCancel()
						if err := Wait(liveCtx, m); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrLaunchExitUnconfirmed) {
							t.Fatalf("dead launcher was treated as child exit: %v", err)
						}
						if n, err := unix.Poll(fds, 0); n != 0 || err != nil {
							t.Fatalf("Wait killed the child: poll=%d err=%v", n, err)
						}
						if cleaned.Load() != 0 {
							t.Fatal("launch cleanup ran while child was alive")
						}
						select {
						case <-m.raw.doneCh:
							t.Fatal("machine reported exit while child was alive")
						default:
						}
						pid, err := PID(m)
						if err != nil || pid != child.Process.Pid {
							t.Fatalf("returned handle cannot locate child: pid=%d err=%v", pid, err)
						}
						if err := StopForce(m); err != nil {
							t.Fatalf("returned handle cannot terminate child: %v", err)
						}
						waitCtx, waitCancel := context.WithTimeout(context.Background(), time.Second)
						defer waitCancel()
						if err := Wait(waitCtx, m); errors.Is(err, context.DeadlineExceeded) || errors.Is(err, ErrLaunchExitUnconfirmed) {
							t.Fatalf("returned handle did not prove exit: %v", err)
						}
					} else if m != nil || rt != (RuntimeConfig{}) {
						t.Fatalf("confirmed failed launch retained ownership: %v, %+v", m, rt)
					}
					select {
					case <-childDone:
					case <-time.After(time.Second):
						t.Fatal("Firecracker child still running")
					}
					if cleaned.Load() != 1 {
						t.Fatalf("launch cleanup count = %d", cleaned.Load())
					}
				})
			}
		}
	}
}

func TestUnconfirmedLaunchWaitRequiresEmptyCgroupWhenChildPIDUnavailable(t *testing.T) {
	for _, state := range []string{"occupied", "unreadable"} {
		t.Run(state, func(t *testing.T) {
			cmd := exec.Command("/bin/sleep", "60")
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			if err := cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			waitErr := cmd.Wait()
			cgroup := t.TempDir()
			events := filepath.Join(cgroup, "cgroup.events")
			if state == "occupied" {
				if err := os.WriteFile(events, []byte("populated 1\n"), 0600); err != nil {
					t.Fatal(err)
				}
			} else if err := os.Mkdir(events, 0700); err != nil {
				t.Fatal(err)
			}
			var cleaned atomic.Int32
			rm := &rawMachine{
				cmd: cmd, processDone: make(chan struct{}), doneCh: make(chan struct{}), waitErr: waitErr,
				launchExitProofRequired: true, cgroupLeaf: cgroup,
				processPID:    func() (int, error) { return 0, errors.New("child PID unavailable") },
				launchCleanup: func() { cleaned.Add(1) },
			}
			close(rm.processDone)
			m := &Machine{raw: rm, cgroupLeaf: cgroup}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
			defer cancel()
			if err := Wait(ctx, m); !errors.Is(err, context.DeadlineExceeded) || !errors.Is(err, ErrLaunchExitUnconfirmed) {
				t.Fatalf("cgroup %s treated as confirmed exit: %v", state, err)
			}
			if cleaned.Load() != 0 {
				t.Fatal("cleaned launch without proof of empty cgroup")
			}
			if err := os.Remove(events); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(events, []byte("populated 0\n"), 0600); err != nil {
				t.Fatal(err)
			}
			proofCtx, proofCancel := context.WithTimeout(context.Background(), time.Second)
			defer proofCancel()
			if err := Wait(proofCtx, m); err != waitErr {
				t.Fatalf("empty cgroup did not prove exit: %v", err)
			}
			if err := Wait(proofCtx, m); err != waitErr || cleaned.Load() != 1 {
				t.Fatalf("repeated Wait: err=%v cleanup=%d", err, cleaned.Load())
			}
		})
	}
}
