package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/ayush6624/web-sandbox/internal/provisioner"
	"github.com/ayush6624/web-sandbox/internal/registry"
	"github.com/ayush6624/web-sandbox/internal/vm"
)

// Config bundles everything the server needs at startup.
type Config struct {
	SocketPath  string
	Provisioner *provisioner.Provisioner
	GatewayIP   string         // bridge IP; used as the guest's default gateway
	VMTemplate  vm.RunOptions  // base options (firecracker bin, kernel, args, vcpus, mem, dns)
}

// Server holds runtime state for the sandbox API.
type Server struct {
	cfg      Config
	reg      *registry.Registry
	machines sync.Map // map[string]*vm.Machine
	vmCtx    context.Context // long-lived; tied to Serve's ctx, NOT request ctx
}

func New(cfg Config, reg *registry.Registry) *Server {
	return &Server{cfg: cfg, reg: reg}
}

// Serve listens on the configured Unix socket until ctx is cancelled.
// On shutdown, all running sandboxes are torn down.
func (s *Server) Serve(ctx context.Context) error {
	s.vmCtx = ctx

	if err := os.MkdirAll(filepath.Dir(s.cfg.SocketPath), 0o755); err != nil {
		return err
	}
	_ = os.Remove(s.cfg.SocketPath) // clear stale socket

	ln, err := net.Listen("unix", s.cfg.SocketPath)
	if err != nil {
		return err
	}
	_ = os.Chmod(s.cfg.SocketPath, 0o600)

	mux := http.NewServeMux()
	mux.HandleFunc("POST /sandboxes", s.handleCreate)
	mux.HandleFunc("GET /sandboxes", s.handleList)
	mux.HandleFunc("GET /sandboxes/{id}", s.handleGet)
	mux.HandleFunc("DELETE /sandboxes/{id}", s.handleDestroy)

	httpSrv := &http.Server{Handler: mux}

	srvErr := make(chan error, 1)
	go func() { srvErr <- httpSrv.Serve(ln) }()

	select {
	case <-ctx.Done():
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = httpSrv.Shutdown(shCtx)
		cancel()
		s.shutdownAll()
		return nil
	case err := <-srvErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

// shutdownAll tears down every tracked sandbox on server stop.
func (s *Server) shutdownAll() {
	var wg sync.WaitGroup
	s.machines.Range(func(k, _ any) bool {
		id := k.(string)
		wg.Add(1)
		go func() {
			defer wg.Done()
			s.destroy(context.Background(), id)
		}()
		return true
	})
	wg.Wait()
}

// --- HTTP handlers ---

func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := uuid.NewString()

	rootfsPath, err := s.cfg.Provisioner.PrepareRootfs(id)
	if err != nil {
		httpError(w, 500, fmt.Errorf("prepare rootfs: %w", err))
		return
	}

	sb, err := s.reg.Create(ctx, id, rootfsPath)
	if err != nil {
		_ = s.cfg.Provisioner.CleanupRootfs(id)
		httpError(w, 500, fmt.Errorf("registry create: %w", err))
		return
	}

	if err := s.cfg.Provisioner.CreateTap(sb.TapDevice); err != nil {
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("create tap: %w", err))
		return
	}

	opts := s.cfg.VMTemplate
	opts.RootfsPath = rootfsPath
	opts.TapDevice = sb.TapDevice
	opts.GuestCIDR = sb.GuestIP + "/24"
	opts.GatewayIP = s.cfg.GatewayIP
	opts.MacAddress = randomMAC()
	opts.SocketPath = "" // auto-generate per VM

	m, rt, err := vm.NewMachine(s.vmCtx, opts, false)
	if err != nil {
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("new machine: %w", err))
		return
	}
	if err := vm.Start(s.vmCtx, m); err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("start: %w", err))
		return
	}
	pid, err := vm.PID(m)
	if err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("pid: %w", err))
		return
	}

	if err := s.cfg.Provisioner.AddPortForward(sb.HostPort, sb.GuestIP); err != nil {
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("port forward: %w", err))
		return
	}

	if err := s.reg.FinishStart(ctx, id, pid, rt.VMID, rt.SocketPath); err != nil {
		s.cfg.Provisioner.RemovePortForward(sb.HostPort, sb.GuestIP)
		_ = vm.StopForce(m)
		s.rollbackPreVM(id, sb)
		httpError(w, 500, fmt.Errorf("finish start: %w", err))
		return
	}

	s.machines.Store(id, m)

	// Watch for early death so we don't silently leak rows.
	go func(id string) {
		err := vm.Wait(context.Background(), m)
		fmt.Fprintf(os.Stderr, "[%s] VM exited: %v\n", id, err)
	}(id)

	sb.PID = pid
	sb.VMID = rt.VMID
	sb.SocketPath = rt.SocketPath
	writeJSON(w, 201, sb)
}

func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	sandboxes, err := s.reg.List(r.Context())
	if err != nil {
		httpError(w, 500, err)
		return
	}
	if sandboxes == nil {
		sandboxes = []registry.Sandbox{}
	}
	writeJSON(w, 200, sandboxes)
}

func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	sb, err := s.reg.Get(r.Context(), id)
	if err != nil {
		httpError(w, 404, err)
		return
	}
	writeJSON(w, 200, sb)
}

func (s *Server) handleDestroy(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.destroy(r.Context(), id); err != nil {
		httpError(w, 500, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- internals ---

// rollbackPreVM cleans up rootfs + tap + registry row when the VM never came up.
func (s *Server) rollbackPreVM(id string, sb registry.Sandbox) {
	ctx := context.Background()
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	_ = s.cfg.Provisioner.CleanupRootfs(id)
	_ = s.reg.Destroy(ctx, id)
}

// destroy is the inverse of handleCreate: graceful guest shutdown, then resource cleanup.
func (s *Server) destroy(ctx context.Context, id string) error {
	sb, err := s.reg.Get(ctx, id)
	if err != nil {
		return fmt.Errorf("get sandbox: %w", err)
	}

	if v, ok := s.machines.LoadAndDelete(id); ok {
		m := v.(*vm.Machine)
		shCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		_ = vm.ShutdownGuest(shCtx, m)
		cancel()
		waitDone := make(chan struct{})
		go func() {
			_ = vm.Wait(context.Background(), m)
			close(waitDone)
		}()
		select {
		case <-waitDone:
		case <-time.After(2 * time.Minute):
			_ = vm.StopForce(m)
			<-waitDone
		}
	}

	s.cfg.Provisioner.RemovePortForward(sb.HostPort, sb.GuestIP)
	_ = s.cfg.Provisioner.DeleteTap(sb.TapDevice)
	_ = s.cfg.Provisioner.CleanupRootfs(id)
	return s.reg.Destroy(ctx, id)
}

// --- helpers ---

func httpError(w http.ResponseWriter, code int, err error) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": err.Error()})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

// randomMAC returns a locally-administered unicast MAC.
func randomMAC() string {
	var b [6]byte
	_, _ = rand.Read(b[:])
	b[0] = (b[0] | 0x02) & 0xfe // locally administered, unicast
	return fmt.Sprintf("%02X:%02X:%02X:%02X:%02X:%02X", b[0], b[1], b[2], b[3], b[4], b[5])
}
