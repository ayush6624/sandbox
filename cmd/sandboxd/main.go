// sandboxd is the in-guest agent: a small HTTP server that lets the host
// run commands and read/write files inside the sandbox. It is baked into the
// base rootfs and started by systemd on boot.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/ayush6624/web-sandbox/internal/agentapi"
)

const defaultCwd = "/home/sandbox/app"

func main() {
	addr := flag.String("addr", fmt.Sprintf(":%d", agentapi.Port), "listen address")
	flag.Parse()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", handleHealth)
	mux.HandleFunc("POST /exec", handleExec)
	mux.HandleFunc("POST /exec/stream", handleExecStream)
	mux.HandleFunc("GET /files", handleReadFile)
	mux.HandleFunc("PUT /files", handleWriteFile)
	mux.HandleFunc("GET /dir", handleListDir)

	log.Printf("sandboxd listening on %s", *addr)
	log.Fatal(http.ListenAndServe(*addr, mux))
}

func handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]string{"status": "ok"})
}

func handleExec(w http.ResponseWriter, r *http.Request) {
	var req agentapi.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Cmd == "" {
		httpError(w, 400, errors.New("cmd is required"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), execTimeout(req))
	defer cancel()
	cmd := buildCmd(ctx, req)

	var stdout, stderr cappedBuffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	start := time.Now()
	err := cmd.Run()
	res := agentapi.ExecResult{
		Stdout:     stdout.String(),
		Stderr:     stderr.String(),
		DurationMS: time.Since(start).Milliseconds(),
		TimedOut:   errors.Is(ctx.Err(), context.DeadlineExceeded),
	}
	switch {
	case err == nil:
		res.ExitCode = 0
	case cmd.ProcessState != nil:
		res.ExitCode = cmd.ProcessState.ExitCode()
	default:
		httpError(w, 500, fmt.Errorf("start command: %w", err))
		return
	}
	writeJSON(w, 200, res)
}

// handleExecStream runs a command like handleExec but streams output as it
// arrives: NDJSON lines of agentapi.ExecEvent, flushed per event, ending with
// exactly one exit event.
func handleExecStream(w http.ResponseWriter, r *http.Request) {
	var req agentapi.ExecRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, 400, fmt.Errorf("decode body: %w", err))
		return
	}
	if req.Cmd == "" {
		httpError(w, 400, errors.New("cmd is required"))
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		httpError(w, 500, errors.New("response writer does not support streaming"))
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), execTimeout(req))
	defer cancel()
	cmd := buildCmd(ctx, req)

	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		httpError(w, 500, err)
		return
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		httpError(w, 500, err)
		return
	}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		httpError(w, 500, fmt.Errorf("start command: %w", err))
		return
	}

	w.Header().Set("Content-Type", "application/x-ndjson")
	w.WriteHeader(200)
	flusher.Flush()

	// One encoder shared by both reader goroutines; the mutex keeps event
	// lines from interleaving mid-object.
	var mu sync.Mutex
	enc := json.NewEncoder(w)
	emit := func(ev agentapi.ExecEvent) {
		mu.Lock()
		defer mu.Unlock()
		_ = enc.Encode(ev)
		flusher.Flush()
	}

	var wg sync.WaitGroup
	stream := func(rd io.Reader, typ string) {
		defer wg.Done()
		buf := make([]byte, 32*1024)
		for {
			n, err := rd.Read(buf)
			if n > 0 {
				emit(agentapi.ExecEvent{Type: typ, Data: string(buf[:n])})
			}
			if err != nil {
				return
			}
		}
	}
	wg.Add(2)
	go stream(stdoutPipe, agentapi.EventStdout)
	go stream(stderrPipe, agentapi.EventStderr)
	wg.Wait() // drain the pipes before Wait closes them

	err = cmd.Wait()
	exit := agentapi.ExecEvent{
		Type:       agentapi.EventExit,
		TimedOut:   errors.Is(ctx.Err(), context.DeadlineExceeded),
		DurationMS: time.Since(start).Milliseconds(),
	}
	switch {
	case err == nil:
		exit.ExitCode = 0
	case cmd.ProcessState != nil:
		exit.ExitCode = cmd.ProcessState.ExitCode()
	default:
		exit.ExitCode = -1
	}
	emit(exit)
}

// execTimeout returns the command time budget for req.
func execTimeout(req agentapi.ExecRequest) time.Duration {
	if req.TimeoutSec > 0 {
		return time.Duration(req.TimeoutSec) * time.Second
	}
	return agentapi.DefaultTimeout
}

// buildCmd constructs the bash invocation shared by /exec and /exec/stream.
// The command runs in its own process group and the whole group is killed on
// timeout, so children spawned by the shell don't outlive the request.
func buildCmd(ctx context.Context, req agentapi.ExecRequest) *exec.Cmd {
	cmd := exec.CommandContext(ctx, "/bin/bash", "-lc", req.Cmd)
	cmd.Dir = workingDir(req.Cwd)
	cmd.Env = os.Environ()
	for k, v := range req.Env {
		cmd.Env = append(cmd.Env, k+"="+v)
	}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
	}
	return cmd
}

func handleReadFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpError(w, 400, errors.New("path query param is required"))
		return
	}
	f, err := os.Open(path)
	if err != nil {
		httpError(w, statusForFSError(err), err)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		httpError(w, 500, err)
		return
	}
	if st.IsDir() {
		httpError(w, 400, fmt.Errorf("%s is a directory (use /dir)", path))
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(st.Size()))
	_, _ = io.Copy(w, f)
}

func handleWriteFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		httpError(w, 400, errors.New("path query param is required"))
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		httpError(w, 500, err)
		return
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		httpError(w, statusForFSError(err), err)
		return
	}
	n, err := io.Copy(f, r.Body)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		httpError(w, 500, err)
		return
	}
	writeJSON(w, 201, map[string]any{"path": path, "bytes": n})
}

func handleListDir(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	if path == "" {
		path = defaultCwd
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		httpError(w, statusForFSError(err), err)
		return
	}
	out := make([]agentapi.DirEntry, 0, len(entries))
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		out = append(out, agentapi.DirEntry{
			Name:  e.Name(),
			Size:  info.Size(),
			Mode:  info.Mode().String(),
			IsDir: e.IsDir(),
			MTime: info.ModTime(),
		})
	}
	writeJSON(w, 200, out)
}

// workingDir picks the exec cwd: explicit request value, else the app dir if
// it exists, else /.
func workingDir(requested string) string {
	if requested != "" {
		return requested
	}
	if _, err := os.Stat(defaultCwd); err == nil {
		return defaultCwd
	}
	return "/"
}

func statusForFSError(err error) int {
	switch {
	case errors.Is(err, os.ErrNotExist):
		return 404
	case errors.Is(err, os.ErrPermission):
		return 403
	default:
		return 500
	}
}

func httpError(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// cappedBuffer keeps at most agentapi.MaxOutputBytes and drops the rest.
type cappedBuffer struct {
	b       []byte
	dropped int64
}

func (c *cappedBuffer) Write(p []byte) (int, error) {
	room := agentapi.MaxOutputBytes - len(c.b)
	if room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.b = append(c.b, p[:room]...)
		c.dropped += int64(len(p) - room)
	} else {
		c.dropped += int64(len(p))
	}
	return len(p), nil
}

func (c *cappedBuffer) String() string {
	if c.dropped > 0 {
		return string(c.b) + fmt.Sprintf("\n... [%d bytes truncated]", c.dropped)
	}
	return string(c.b)
}
