package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const acceptanceJSON = `{"id":"op-1","type":"sandbox_create","status":"pending","requested":1,"succeeded":0,"failed":0,"created_at":"2026-01-01T00:00:00Z"}`
const runningJSON = `{"id":"op-1","type":"sandbox_create","status":"running","requested":1,"succeeded":0,"failed":0,"results":[{"index":0,"progress":{"coordination":{"phase":"assigned"},"worker":{"attempt":1,"sequence":4,"condition":"succeeded","current":{"stage":"ready"}}}}]}`
const successJSON = `{"id":"op-1","type":"sandbox_create","status":"succeeded","completed_at":"2026-01-01T00:00:01Z","requested":1,"succeeded":1,"failed":0,"results":[{"index":0,"sandbox":{"id":"sb-1"}}]}`

func executeOperationTest(ctx context.Context, args ...string) (string, string, error) {
	cmd := rootCmd()
	var stdout, stderr bytes.Buffer
	cmd.SetOut(&stdout)
	cmd.SetErr(&stderr)
	cmd.SetArgs(args)
	err := cmd.ExecuteContext(ctx)
	return stdout.String(), stderr.String(), err
}

func TestDurableUpCommandsHTTPAndUnix(t *testing.T) {
	for _, unix := range []bool{false, true} {
		t.Run(fmt.Sprint("unix=", unix), func(t *testing.T) {
			var posts, polls, gets atomic.Int32
			handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/v1/sandbox-creations":
					posts.Add(1)
					if r.Method != "POST" || r.Header.Get("Idempotency-Key") != "stable-key" {
						t.Errorf("request: %s %v", r.Method, r.Header)
					}
					if !unix && r.Header.Get("Authorization") != "Bearer test-key" {
						t.Error("missing bearer")
					}
					var body map[string]any
					json.NewDecoder(r.Body).Decode(&body)
					if body["name"] != "example" {
						t.Errorf("body=%v", body)
					}
					w.WriteHeader(202)
					fmt.Fprint(w, acceptanceJSON)
				case "/v1/operations/op-1":
					if polls.Add(1) <= 2 {
						fmt.Fprint(w, runningJSON)
					} else {
						fmt.Fprint(w, successJSON)
					}
				case "/sandboxes/sb-1":
					gets.Add(1)
					fmt.Fprint(w, `{"id":"sb-1","status":"running","guest_ip":"10.0.0.2"}`)
				default:
					t.Errorf("unexpected path %s", r.URL.Path)
					w.WriteHeader(404)
				}
			})
			flags := operationTestServer(t, unix, handler)
			out, stderr, err := executeOperationTest(context.Background(), append([]string{"up", "--name", "example", "--idempotency-key", "stable-key"}, flags...)...)
			if err != nil {
				t.Fatal(err, stderr)
			}
			var ready map[string]any
			if json.Unmarshal([]byte(out), &ready) != nil || ready["id"] != "sb-1" || ready["guest_ip"] != "10.0.0.2" {
				t.Fatalf("stdout=%s", out)
			}
			if strings.Count(stderr, "attempt 1 ready succeeded") != 1 || !strings.Contains(stderr, "sandbox operation wait op-1") {
				t.Fatalf("stderr=%s", stderr)
			}
			if posts.Load() != 1 || polls.Load() != 3 || gets.Load() != 1 {
				t.Fatalf("requests=%d/%d/%d", posts.Load(), polls.Load(), gets.Load())
			}
		})
	}
}
func TestDurableUpValidationBeforePOST(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { requests.Add(1) }))
	defer server.Close()
	for _, args := range [][]string{
		{"--async", "--progress"}, {"--hibernate-after=-2"}, {"--ttl=-1"},
		{"--vcpus=-1"}, {"--mem=-1"}, {"--mem=1"}, {"--mem=127"}, {"--async", "--mem=127"}, {"--legacy", "--mem=127"}, {"--idempotency-key="}, {"--idempotency-key= "},
		{"--legacy", "--async"}, {"--legacy", "--progress"}, {"--legacy", "--idempotency-key=key"},
		{"--legacy", "--async=false"}, {"--legacy", "--progress=false"},
		{"--legacy", "--vcpus=-1"}, {"--legacy", "--mem=-1"}, {"--legacy", "--hibernate-after=-2"},
	} {
		_, _, err := executeOperationTest(context.Background(), append(append([]string{"up"}, args...), "--api-url", server.URL)...)
		if err == nil {
			t.Fatalf("accepted %v", args)
		}
	}
	if requests.Load() != 0 {
		t.Fatalf("made %d requests", requests.Load())
	}
}
func TestDurableUpLostResponseReplayAndAsync(t *testing.T) {
	var posts atomic.Int32
	keys := make(chan string, 2)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/sandbox-creations" {
			t.Errorf("fallback %s", r.URL.Path)
		}
		keys <- r.Header.Get("Idempotency-Key")
		if posts.Add(1) == 1 {
			conn, _, _ := w.(http.Hijacker).Hijack()
			conn.Close()
			return
		}
		w.WriteHeader(202)
		fmt.Fprint(w, acceptanceJSON)
	}))
	defer server.Close()
	out, stderr, err := executeOperationTest(context.Background(), "up", "--async", "--api-url", server.URL)
	if err == nil || out != "" {
		t.Fatalf("out=%q err=%v", out, err)
	}
	key := <-keys
	if key == "" || !strings.Contains(stderr, key) {
		t.Fatalf("recovery missing %s", stderr)
	}
	out, _, err = executeOperationTest(context.Background(), "up", "--async", "--api-url", server.URL, "--idempotency-key", key)
	if err != nil || !strings.Contains(out, `"id": "op-1"`) || posts.Load() != 2 || <-keys != key {
		t.Fatalf("out=%s err=%v posts=%v", out, err, posts.Load())
	}
}
func TestOperationWaitFailureAndGet(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != "GET" {
			t.Error("mutation while waiting")
		}
		fmt.Fprint(w, `{"id":"op-1","type":"sandbox_create","status":"failed","completed_at":"2026-01-01T00:00:01Z","requested":1,"failed":1,"results":[{"index":0,"error":{"code":"capacity","detail":"full"}}]}`)
	}))
	defer server.Close()
	for _, command := range []string{"get", "wait"} {
		out, stderr, err := executeOperationTest(context.Background(), "operation", command, "op-1", "--api-url", server.URL)
		if (command == "wait") != (err != nil) || !strings.Contains(out, `"code": "capacity"`) {
			t.Fatalf("%s out=%s err=%v", command, out, err)
		}
		if command == "get" && stderr != "" {
			t.Fatal(stderr)
		}
	}
}
func TestDurableUpFinalGetFailure(t *testing.T) {
	var posts atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/sandbox-creations":
			posts.Add(1)
			w.WriteHeader(202)
			fmt.Fprint(w, acceptanceJSON)
		case "/v1/operations/op-1":
			fmt.Fprint(w, successJSON)
		case "/sandboxes/sb-1":
			w.WriteHeader(503)
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
		}
	}))
	defer server.Close()
	out, _, err := executeOperationTest(context.Background(), "up", "--progress", "--api-url", server.URL)
	if out != "" || err == nil || !strings.Contains(err.Error(), "sb-1") || !strings.Contains(err.Error(), "op-1") || posts.Load() != 1 {
		t.Fatalf("out=%q err=%v posts=%d", out, err, posts.Load())
	}
}
func TestOperationCancellationDuringBody(t *testing.T) {
	for _, command := range [][]string{{"up"}, {"up", "--progress"}, {"operation", "wait", "op-1"}} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == "POST" {
				w.WriteHeader(202)
				fmt.Fprint(w, acceptanceJSON)
				return
			}
			fmt.Fprint(w, `{"id":`)
			w.(http.Flusher).Flush()
			<-r.Context().Done()
		}))
		ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
		out, _, err := executeOperationTest(ctx, append(command, "--api-url", server.URL)...)
		cancel()
		server.Close()
		if out != "" || err == nil || !strings.Contains(err.Error(), "op-1") || !strings.Contains(err.Error(), "accepted work continues") {
			t.Fatalf("out=%q err=%v", out, err)
		}
	}
}

func TestDurableUpCancellationBeforeAcceptance(t *testing.T) {
	keys := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		keys <- r.Header.Get("Idempotency-Key")
		w.WriteHeader(202)
		fmt.Fprint(w, `{"id":`)
		w.(http.Flusher).Flush()
		<-r.Context().Done()
	}))
	defer server.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	out, stderr, err := executeOperationTest(ctx, "up", "--async", "--api-url", server.URL)
	key := <-keys
	if out != "" || err == nil || key == "" || !strings.Contains(stderr, key) || !strings.Contains(stderr, "accepted work continues") || !strings.Contains(err.Error(), key) {
		t.Fatalf("out=%q stderr=%q err=%v", out, stderr, err)
	}
}

func TestCLISignalHelper(t *testing.T) {
	if os.Getenv("SANDBOX_SIGNAL_HELPER") != "1" {
		return
	}
	for index, arg := range os.Args {
		if arg == "--" {
			os.Args = append([]string{"sandbox"}, os.Args[index+1:]...)
			main()
			os.Exit(0)
		}
	}
	os.Exit(2)
}

func TestCLISignalsRemainScoped(t *testing.T) {
	for _, durable := range []bool{false, true} {
		t.Run(fmt.Sprint("durable=", durable), func(t *testing.T) {
			requested := make(chan struct{}, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requested <- struct{}{}
				w.WriteHeader(200)
				w.(http.Flusher).Flush()
				<-r.Context().Done()
			}))
			defer server.Close()
			args := []string{"exec", "--api-url", server.URL, "sb-1", "--", "sleep", "60"}
			if durable {
				args = []string{"operation", "wait", "op-1", "--api-url", server.URL}
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			process := exec.CommandContext(ctx, os.Args[0], append([]string{"-test.run=^TestCLISignalHelper$", "--"}, args...)...)
			process.Env = append(os.Environ(), "SANDBOX_SIGNAL_HELPER=1")
			var output bytes.Buffer
			process.Stdout = &output
			process.Stderr = &output
			if err := process.Start(); err != nil {
				t.Fatal(err)
			}
			select {
			case <-requested:
			case <-ctx.Done():
				process.Wait()
				t.Fatalf("command never requested API: %s", output.String())
			}
			if err := process.Process.Signal(os.Interrupt); err != nil {
				t.Fatal(err)
			}
			err := process.Wait()
			if ctx.Err() != nil {
				t.Fatalf("Ctrl-C did not stop command: %s", output.String())
			}
			if err == nil {
				t.Fatal("interrupted command returned success")
			}
			if durable && (!strings.Contains(output.String(), "op-1") || !strings.Contains(output.String(), "accepted work continues")) {
				t.Fatalf("missing recovery after Ctrl-C: %s", output.String())
			}
		})
	}
}

func operationTestServer(t *testing.T, unix bool, handler http.Handler) []string {
	t.Helper()
	var flags []string
	if unix {
		dir, err := os.MkdirTemp("/tmp", "single-cli-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(dir) })
		path := filepath.Join(dir, "s")
		listener, err := net.Listen("unix", path)
		if err != nil {
			t.Fatal(err)
		}
		server := &http.Server{Handler: handler}
		go server.Serve(listener)
		t.Cleanup(func() { server.Close() })
		config := filepath.Join(dir, "config.json")
		os.WriteFile(config, []byte("{}"), 0600)
		flags = []string{"--api-url=", "--socket", path, "--config", config}
	} else {
		server := httptest.NewServer(handler)
		t.Cleanup(func() { server.Close() })
		flags = []string{"--api-url", server.URL, "--api-key", "test-key"}
	}
	return flags
}

func TestUpCreateOptionsHTTPAndUnix(t *testing.T) {
	for _, unix := range []bool{false, true} {
		for _, tc := range []struct {
			name     string
			args     []string
			cpu, mem int64
			idle     int
		}{
			{name: "defaults"},
			{name: "zero", args: []string{"--vcpus=0", "--mem=0"}},
			{name: "cpu", args: []string{"--vcpus=2"}, cpu: 2},
			{name: "memory", args: []string{"--mem=1024"}, mem: 1024},
			{name: "both", args: []string{"--vcpus=2", "--mem=1024"}, cpu: 2, mem: 1024},
			{name: "disabled idle", args: []string{"--hibernate-after=-1"}, idle: -1},
			{name: "progress synonym", args: []string{"--progress"}},
		} {
			t.Run(fmt.Sprintf("unix=%v/%s", unix, tc.name), func(t *testing.T) {
				var posts, gets atomic.Int32
				flags := operationTestServer(t, unix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					switch r.URL.Path {
					case "/v1/sandbox-creations":
						posts.Add(1)
						if r.Method != "POST" || r.Header.Get("Idempotency-Key") == "" {
							t.Errorf("request: %s %v", r.Method, r.Header)
						}
						var body struct {
							Resources *struct {
								VCPU      int64 `json:"vcpu"`
								MemoryMIB int64 `json:"memory_mib"`
							} `json:"resources"`
							Lifecycle struct {
								Idle int `json:"idle_timeout_seconds"`
							} `json:"lifecycle"`
						}
						if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
							t.Error(err)
						}
						if tc.cpu == 0 && tc.mem == 0 {
							if body.Resources != nil {
								t.Errorf("defaults have resources: %+v", body.Resources)
							}
						} else if body.Resources == nil || body.Resources.VCPU != tc.cpu || body.Resources.MemoryMIB != tc.mem {
							t.Errorf("resources: %+v", body.Resources)
						}
						if body.Lifecycle.Idle != tc.idle {
							t.Errorf("idle=%d", body.Lifecycle.Idle)
						}
						w.WriteHeader(202)
						fmt.Fprint(w, acceptanceJSON)
					case "/v1/operations/op-1":
						fmt.Fprint(w, successJSON)
					case "/sandboxes/sb-1":
						gets.Add(1)
						fmt.Fprint(w, `{"id":"sb-1","guest_ip":"10.0.0.2"}`)
					default:
						t.Errorf("unexpected route: %s", r.URL.Path)
						w.WriteHeader(404)
					}
				}))
				args := append([]string{"up"}, tc.args...)
				out, stderr, err := executeOperationTest(context.Background(), append(args, flags...)...)
				if err != nil || !strings.Contains(out, `"guest_ip": "10.0.0.2"`) || !strings.Contains(stderr, "sandbox sb-1 ready") || posts.Load() != 1 || gets.Load() != 1 {
					t.Fatalf("stdout=%s stderr=%s err=%v posts=%d gets=%d", out, stderr, err, posts.Load(), gets.Load())
				}
			})
		}
	}
}

func TestUpExplicitLegacyHTTPAndUnix(t *testing.T) {
	for _, unix := range []bool{false, true} {
		t.Run(fmt.Sprint("unix=", unix), func(t *testing.T) {
			var posts atomic.Int32
			flags := operationTestServer(t, unix, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				posts.Add(1)
				if r.Method != "POST" || r.URL.Path != "/sandboxes" || r.Header.Get("Idempotency-Key") != "" {
					t.Errorf("request: %s %s %v", r.Method, r.URL.Path, r.Header)
				}
				var body map[string]any
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Error(err)
				}
				if body["vcpus"] != float64(2) || body["hibernate_after_sec"] != float64(-1) {
					t.Errorf("body=%v", body)
				}
				fmt.Fprint(w, `{"id":"sb-legacy","guest_ip":"10.0.0.3"}`)
			}))
			out, stderr, err := executeOperationTest(context.Background(), append([]string{"up", "--legacy", "--vcpus=2", "--hibernate-after=-1"}, flags...)...)
			if err != nil || posts.Load() != 1 || !strings.Contains(out, `"guest_ip": "10.0.0.3"`) || stderr != "sandbox sb-legacy ready\n" {
				t.Fatalf("out=%s stderr=%s err=%v posts=%d", out, stderr, err, posts.Load())
			}
		})
	}
}

func TestUpDoesNotFallbackAfterAPIError(t *testing.T) {
	for _, status := range []int{400, 404, 405, 409, 500} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				if r.Method != "POST" || r.URL.Path != "/v1/sandbox-creations" {
					t.Errorf("unexpected request: %s %s", r.Method, r.URL.Path)
				}
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"code":"test","detail":"rejected"}}`)
			}))
			defer server.Close()
			out, _, err := executeOperationTest(context.Background(), "up", "--api-url", server.URL)
			if err == nil || out != "" || requests.Load() != 1 {
				t.Fatalf("out=%s err=%v requests=%d", out, err, requests.Load())
			}
			if (status == 404 || status == 405) && (!strings.Contains(err.Error(), "--legacy") || !strings.Contains(err.Error(), "upgrade the server")) {
				t.Fatalf("missing compatibility guidance: %v", err)
			}
		})
	}
}
