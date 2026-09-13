//go:build linux

package vm

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCloneBackendSequenceAndCleanup(t *testing.T) {
	for _, backend := range []cloneBackend{fileClone, uffdClone} {
		for _, failAt := range []string{"", "/snapshot/load", "/drives/rootfs", "/mmds", "/vm", "configure", "start"} {
			if backend == fileClone && failAt == "configure" {
				continue
			}
			t.Run(string(rune('0'+backend))+failAt, func(t *testing.T) {
				dir := t.TempDir()
				sock := filepath.Join(dir, "api")
				ln, err := net.Listen("unix", sock)
				if err != nil {
					t.Fatal(err)
				}
				var mu sync.Mutex
				var sequence []string
				var bodies []map[string]any
				var resumed atomic.Bool
				prewarmed := make(chan struct{}, 1)
				api := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path == "/version" {
						w.WriteHeader(200)
						return
					}
					var body map[string]any
					if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
						t.Error(err)
					}
					mu.Lock()
					sequence = append(sequence, r.Method+" "+r.URL.Path)
					bodies = append(bodies, body)
					mu.Unlock()
					if r.URL.Path == failAt {
						http.Error(w, "injected", 500)
						return
					}
					if r.URL.Path == "/vm" {
						resumed.Store(true)
					}
					w.WriteHeader(204)
				})}
				go api.Serve(ln)
				defer api.Close()
				var cleaned atomic.Int32
				cleanup := make(chan struct{})
				var request LaunchRequest
				launcher := ProcessLauncherFunc(func(ctx context.Context, req LaunchRequest) (PreparedLaunch, error) {
					request = req
					cmd := exec.CommandContext(ctx, "/bin/sleep", "60")
					if failAt == "start" {
						cmd = exec.CommandContext(ctx, filepath.Join(dir, "missing"))
					}
					return PreparedLaunch{
						Command: cmd, HostAPIPath: sock, HostUFFDPath: sock + ".uffd",
						Paths: LaunchPaths{SnapshotMem: "/snapshots/memory", SnapshotState: "/snapshots/state", Rootfs: "/disks/rootfs", UFFD: "/run/uffd.socket"},
						ConfigureSocket: func(path string) error {
							if _, err := os.Stat(path); err != nil {
								t.Error(err)
							}
							if failAt == "configure" {
								return errors.New("injected permissions")
							}
							return nil
						},
						Cleanup: func() {
							if cleaned.Add(1) == 1 {
								close(cleanup)
							}
						},
					}, nil
				})
				opts := RunOptions{Launcher: launcher, SocketPath: sock, LogDir: dir, UFFDChunks: &UFFDChunkSource{Total: 8192, ChunkSize: 4096, Prefetch: 1, Prewarm: []uint64{0}, Load: func(uint64) ([]byte, error) {
					if !resumed.Load() {
						t.Error("prewarm began before resume succeeded")
					}
					prewarmed <- struct{}{}
					return make([]byte, 4096), nil
				}}}
				params := CloneParams{MemPath: "must-not-stage-for-remote", StatePath: "host-state", CloneRootfsPath: "host-rootfs", TapDevice: "tap-new", GuestIP: "10.0.0.2", MacAddress: "02:00:00:00:00:02", GatewayIP: "10.0.0.1", Prefix: 24, Gen: "fresh"}
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				start := StartClone
				if backend == uffdClone {
					start = StartCloneUFFD
				}
				m, rt, err := start(ctx, opts, params)
				if failAt == "" {
					if err != nil {
						t.Fatal(err)
					}
					defer StopForce(m)
					if DiffCapable(m) != (backend == fileClone) {
						t.Fatal("incorrect diff capability")
					}
					if rt.LaunchTimings.SnapshotLoad <= 0 || rt.LaunchTimings.Resume <= 0 {
						t.Fatal("missing launch timings")
					}
					if backend == uffdClone {
						select {
						case <-prewarmed:
						case <-time.After(time.Second):
							t.Fatal("prewarm did not start after resume")
						}
					}
					if err := StopForce(m); err != nil {
						t.Fatal(err)
					}
				} else if err == nil {
					StopForce(m)
					t.Fatal("expected injected failure")
				}
				if failAt != "" {
					if m != nil || rt != (RuntimeConfig{}) {
						t.Fatalf("confirmed failed launch retained machine or runtime: %v, %+v", m, rt)
					}
					if errors.Is(err, ErrLaunchExitUnconfirmed) {
						t.Fatalf("failed launch was not joined: %v", err)
					}
					select {
					case <-cleanup:
					default:
						t.Fatal("failed launch returned before process exit and cleanup")
					}
				}
				select {
				case <-cleanup:
				case <-time.After(3 * time.Second):
					t.Fatal("launch cleanup did not run")
				}
				if cleaned.Load() != 1 {
					t.Fatalf("cleanup count %d", cleaned.Load())
				}
				if backend == uffdClone {
					if request.Mode != LaunchUFFDRestore || request.SnapshotMem != "" {
						t.Fatalf("remote UFFD staging: %+v", request)
					}
					deadline := time.Now().Add(time.Second)
					for {
						_, err := os.Stat(sock + ".uffd")
						if errors.Is(err, os.ErrNotExist) {
							break
						}
						if time.Now().After(deadline) {
							t.Fatal("UFFD socket leaked")
						}
						time.Sleep(time.Millisecond)
					}
				} else if request.SnapshotMem != params.MemPath || request.Mode != LaunchHotClone {
					t.Fatalf("file staging: %+v", request)
				}
				mu.Lock()
				defer mu.Unlock()
				want := []string{"PUT /snapshot/load", "PATCH /drives/rootfs", "PUT /mmds", "PATCH /vm"}
				if failAt == "configure" || failAt == "start" {
					want = nil
				} else if failAt != "" {
					for i, v := range want {
						if strings.HasSuffix(v, failAt) {
							want = want[:i+1]
							break
						}
					}
				}
				if !reflect.DeepEqual(sequence, want) {
					t.Fatalf("sequence %v, want %v", sequence, want)
				}
				if len(bodies) == 0 {
					return
				}
				load := bodies[0]
				if load["resume_vm"] != false || load["snapshot_path"] != "/snapshots/state" {
					t.Fatalf("load %+v", load)
				}
				overrides := load["network_overrides"].([]any)[0].(map[string]any)
				if overrides["iface_id"] != "1" || overrides["host_dev_name"] != "tap-new" {
					t.Fatalf("overrides %+v", overrides)
				}
				mem := load["mem_backend"].(map[string]any)
				if backend == uffdClone {
					if mem["backend_type"] != "Uffd" || mem["backend_path"] != "/run/uffd.socket" || load["enable_diff_snapshots"] != nil {
						t.Fatalf("UFFD load %+v", load)
					}
				} else if mem["backend_type"] != "File" || mem["backend_path"] != "/snapshots/memory" || load["enable_diff_snapshots"] != true {
					t.Fatalf("file load %+v", load)
				}
				if len(bodies) > 1 && bodies[1]["path_on_host"] != "/disks/rootfs" {
					t.Fatalf("rootfs %+v", bodies[1])
				}
				if len(bodies) > 2 && (bodies[2]["ip"] != params.GuestIP || bodies[2]["gen"] != "fresh" || bodies[2]["epoch_ms"] == "") {
					t.Fatalf("identity %+v", bodies[2])
				}
			})
		}
	}
}
