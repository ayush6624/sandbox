//go:build linux

package vm

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// Machine wraps a Firecracker VM. Cold-booted and 1:1-restored VMs use the
// embedded SDK machine; fan-out clones (StartClone) use raw, because the clone
// restore sequence — snapshot load with network_overrides, then a drive
// relocation, before resume — isn't expressible through the SDK v1.0.0
// WithSnapshot helper (which loads+resumes atomically). Exactly one of the two
// is set; the lifecycle functions branch on which.
type Machine struct {
	*fcsdk.Machine
	raw *rawMachine
	// diffCapable is set when the VM was loaded with dirty-page tracking
	// enabled (StartClone), making Diff snapshots valid against the snapshot
	// it was loaded from.
	diffCapable bool
}

// DiffCapable reports whether Snapshot(..., SnapshotDiff) is valid for m —
// i.e. the VM has tracked dirty pages since it was loaded from its base
// snapshot.
func DiffCapable(m *Machine) bool { return m != nil && m.diffCapable }

// SealUFFDRecording stops working-set recording on a UFFD-restored machine's
// page source. Call it just before a hibernate snapshot: the snapshot reads the
// whole guest, faulting every not-yet-present chunk through the handler, which
// would otherwise record the entire guest as "working set" (roadmap B3). No-op
// for non-UFFD machines or non-recording sources.
func SealUFFDRecording(m *Machine) {
	if m != nil && m.raw != nil && m.raw.uffd != nil {
		m.raw.uffd.seal()
	}
}

// UFFDWorkingSet returns the chunk indices the guest faulted since wake (up to
// the seal), for the server to persist and prewarm the next wake. nil for
// non-UFFD machines or non-recording sources.
func UFFDWorkingSet(m *Machine) []uint64 {
	if m != nil && m.raw != nil && m.raw.uffd != nil {
		return m.raw.uffd.workingSet()
	}
	return nil
}

// rawMachine is a Firecracker process we manage directly (clone path, and the
// UFFD restore path), driving its API over the unix socket instead of through
// the SDK.
type rawMachine struct {
	cmd     *exec.Cmd
	sock    string
	doneCh  chan struct{} // closed when the process exits
	waitErr error         // exit error, valid once doneCh is closed
	// uffd is the page-fault handler backing a UFFD-restored VM's memory; nil
	// for cold boots and clones. It must outlive the VM (the guest faults
	// throughout its run) and be torn down when the VM exits.
	uffd *uffdHandler
}

func (o *RunOptions) applyDefaults() error {
	if o.FirecrackerBin == "" {
		o.FirecrackerBin = "firecracker"
	}
	if o.SocketPath == "" {
		id, err := uuid.NewRandom()
		if err != nil {
			return err
		}
		o.SocketPath = filepath.Join(os.TempDir(), fmt.Sprintf("sandbox-%s.sock", id.String()))
	}
	if o.LogDir == "" {
		o.LogDir = os.TempDir()
	}
	return nil
}

func (o RunOptions) fcConfig() (fcsdk.Config, error) {
	if err := o.applyDefaults(); err != nil {
		return fcsdk.Config{}, err
	}

	uid, err := uuid.NewRandom()
	if err != nil {
		return fcsdk.Config{}, err
	}
	logFIFO := filepath.Join(o.LogDir, fmt.Sprintf("sandbox-log-%s.fifo", uid.String()))

	vmID, err := uuid.NewRandom()
	if err != nil {
		return fcsdk.Config{}, err
	}

	drives := []models.Drive{
		{
			DriveID:      fcsdk.String("rootfs"),
			PathOnHost:   fcsdk.String(o.RootfsPath),
			IsRootDevice: fcsdk.Bool(true),
			IsReadOnly:   fcsdk.Bool(false),
		},
	}

	cfg := fcsdk.Config{
		VMID:            vmID.String(),
		SocketPath:      o.SocketPath,
		KernelImagePath: o.KernelImage,
		KernelArgs:      o.KernelArgs,
		Drives:          drives,
		MachineCfg: models.MachineConfiguration{
			VcpuCount:  fcsdk.Int64(o.Vcpus),
			MemSizeMib: fcsdk.Int64(o.MemMIB),
		},
		LogFifo:  logFIFO,
		LogLevel: "Warn",
		Seccomp:  fcsdk.SeccompConfig{Enabled: false},
	}

	if o.TapDevice != "" {
		iface, err := buildNetworkInterface(o)
		if err != nil {
			return fcsdk.Config{}, fmt.Errorf("network config: %w", err)
		}
		// Enable MMDS on the boot NIC so snapshots carry an MMDS device: a clone
		// restored from this snapshot reads its fresh IP/MAC from MMDS and
		// reconfigures eth0 (see StartClone + the sandboxd thaw agent). Harmless
		// for sandboxes that are never snapshotted — the guest simply never reads it.
		iface.AllowMMDS = true
		cfg.NetworkInterfaces = fcsdk.NetworkInterfaces{iface}
		cfg.MmdsVersion = fcsdk.MMDSv2
	}

	return cfg, nil
}

func buildNetworkInterface(o RunOptions) (fcsdk.NetworkInterface, error) {
	ip, ipNet, err := net.ParseCIDR(o.GuestCIDR)
	if err != nil {
		return fcsdk.NetworkInterface{}, fmt.Errorf("parse guest CIDR %q: %w", o.GuestCIDR, err)
	}
	ipNet.IP = ip

	gateway := net.ParseIP(o.GatewayIP)
	if gateway == nil {
		return fcsdk.NetworkInterface{}, fmt.Errorf("invalid gateway IP %q", o.GatewayIP)
	}

	var nameservers []string
	if o.Nameservers != "" {
		nameservers = strings.Split(o.Nameservers, ",")
	}

	return fcsdk.NetworkInterface{
		StaticConfiguration: &fcsdk.StaticNetworkConfiguration{
			MacAddress:  o.MacAddress,
			HostDevName: o.TapDevice,
			IPConfiguration: &fcsdk.IPConfiguration{
				IPAddr:      *ipNet,
				Gateway:     gateway,
				Nameservers: nameservers,
				IfName:      "eth0",
			},
		},
	}, nil
}

func buildCommand(ctx context.Context, fcCfg fcsdk.Config, fcBin, logDir string) *exec.Cmd {
	builder := fcsdk.VMCommandBuilder{}.
		WithBin(fcBin).
		WithSocketPath(fcCfg.SocketPath).
		AddArgs("--id", fcCfg.VMID)
	if !fcCfg.Seccomp.Enabled {
		builder = builder.AddArgs("--no-seccomp")
	} else if len(fcCfg.Seccomp.Filter) > 0 {
		builder = builder.AddArgs("--seccomp-filter", fcCfg.Seccomp.Filter)
	}
	cmd := builder.Build(ctx)
	// Capture firecracker's stdout/stderr so we can debug early-exit crashes.
	logPath := filepath.Join(logDir, fmt.Sprintf("firecracker-%s.log", fcCfg.VMID))
	if f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	return cmd
}

func silentLog() *logrus.Entry {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return logrus.NewEntry(l)
}

// NewMachine builds a Machine from RunOptions.
// Pass disableValidation=true to skip SDK path validation (e.g. for dry runs).
func NewMachine(ctx context.Context, opts RunOptions, disableValidation bool) (*Machine, RuntimeConfig, error) {
	fcCfg, err := opts.fcConfig()
	if err != nil {
		return nil, RuntimeConfig{}, err
	}
	fcCfg.DisableValidation = disableValidation

	cmd := buildCommand(ctx, fcCfg, opts.FirecrackerBin, opts.LogDir)
	m, err := fcsdk.NewMachine(ctx, fcCfg, fcsdk.WithProcessRunner(cmd), fcsdk.WithLogger(silentLog()))
	if err != nil {
		return nil, RuntimeConfig{}, err
	}
	rt := RuntimeConfig{SocketPath: fcCfg.SocketPath, VMID: fcCfg.VMID}
	return &Machine{Machine: m}, rt, nil
}

// NewMachineFromSnapshot builds a Machine that loads memPath/statePath and
// resumes, instead of cold booting. The network device is restored from the
// snapshot (the SDK's load-snapshot handler list skips network-interface
// creation), so we omit NetworkInterfaces here; the caller must recreate the
// tap under the name baked into the snapshot before Start. The rootfs drive is
// kept only so the SDK's load-snapshot validation can stat it — its contents
// must already match the snapshot's view of the disk.
func NewMachineFromSnapshot(ctx context.Context, opts RunOptions, memPath, statePath string, disableValidation bool) (*Machine, RuntimeConfig, error) {
	opts.TapDevice = "" // device comes from the snapshot; don't add a fresh iface
	fcCfg, err := opts.fcConfig()
	if err != nil {
		return nil, RuntimeConfig{}, err
	}
	fcCfg.DisableValidation = disableValidation

	cmd := buildCommand(ctx, fcCfg, opts.FirecrackerBin, opts.LogDir)
	m, err := fcsdk.NewMachine(ctx, fcCfg,
		fcsdk.WithProcessRunner(cmd),
		fcsdk.WithLogger(silentLog()),
		fcsdk.WithSnapshot(memPath, statePath, func(c *fcsdk.SnapshotConfig) {
			c.ResumeVM = true
		}),
	)
	if err != nil {
		return nil, RuntimeConfig{}, err
	}
	rt := RuntimeConfig{SocketPath: fcCfg.SocketPath, VMID: fcCfg.VMID}
	return &Machine{Machine: m}, rt, nil
}

// Start boots the VMM and sends InstanceStart — or, for a snapshot-backed
// machine, loads the snapshot and resumes (the SDK no-ops InstanceStart then).
func Start(ctx context.Context, m *Machine) error {
	if m == nil || m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	return m.Machine.Start(ctx)
}

// StopForce sends SIGTERM to the Firecracker process (fast teardown).
func StopForce(m *Machine) error {
	if m == nil {
		return nil
	}
	if m.raw != nil {
		if m.raw.cmd.Process != nil {
			err := m.raw.cmd.Process.Signal(syscall.SIGTERM)
			// Close the UFFD handler's socket (if any); its mem mapping is
			// released by the fault goroutine once Firecracker actually exits.
			m.raw.uffd.close()
			return err
		}
		m.raw.uffd.close()
		return nil
	}
	if m.Machine == nil {
		return nil
	}
	return m.Machine.StopVMM()
}

// ShutdownGuest requests ACPI-style shutdown via CtrlAltDel. Clones have no SDK
// machine to drive ACPI, so we SIGTERM the VMM (prompt exit) instead — good
// enough for a disposable sandbox and keeps destroy() from blocking on Wait.
func ShutdownGuest(ctx context.Context, m *Machine) error {
	if m == nil {
		return fmt.Errorf("nil machine")
	}
	if m.raw != nil {
		return StopForce(m)
	}
	if m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	return m.Machine.Shutdown(ctx)
}

// Wait blocks until the Firecracker process exits.
func Wait(ctx context.Context, m *Machine) error {
	if m == nil {
		return fmt.Errorf("nil machine")
	}
	if m.raw != nil {
		select {
		case <-m.raw.doneCh:
			return m.raw.waitErr
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	return m.Machine.Wait(ctx)
}

// PID returns the Firecracker process PID.
func PID(m *Machine) (int, error) {
	if m == nil {
		return 0, fmt.Errorf("nil machine")
	}
	if m.raw != nil {
		if m.raw.cmd.Process == nil {
			return 0, fmt.Errorf("clone process not started")
		}
		return m.raw.cmd.Process.Pid, nil
	}
	if m.Machine == nil {
		return 0, fmt.Errorf("nil machine")
	}
	return m.Machine.PID()
}

// Pause freezes the guest's vCPUs (required before CreateSnapshot). Raw clone
// machines — which every hot-created sandbox is — are driven over the FC
// socket directly; the SDK path covers cold boots and 1:1 restores.
func Pause(ctx context.Context, m *Machine) error {
	if m == nil {
		return fmt.Errorf("nil machine")
	}
	if m.raw != nil {
		return fcAPI(ctx, unixClient(m.raw.sock), "PATCH", "/vm", map[string]any{"state": "Paused"})
	}
	if m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	return m.Machine.PauseVM(ctx)
}

// Resume unfreezes the guest's vCPUs after a snapshot.
func Resume(ctx context.Context, m *Machine) error {
	if m == nil {
		return fmt.Errorf("nil machine")
	}
	if m.raw != nil {
		return fcAPI(ctx, unixClient(m.raw.sock), "PATCH", "/vm", map[string]any{"state": "Resumed"})
	}
	if m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	return m.Machine.ResumeVM(ctx)
}

// Snapshot writes a VM snapshot (memory + device state) to the given paths.
// The VM must be paused first. snapType is SnapshotFull or SnapshotDiff; Diff
// writes only pages dirtied since load and requires DiffCapable(m) — the
// resulting sparse mem file must be merged onto its base before a restore.
// For a raw clone machine the snapshot bakes the clone's CURRENT config — its
// own CoW rootfs path, tap, and reidentified IP/MAC — which is exactly what
// the registry records for it.
func Snapshot(ctx context.Context, m *Machine, memPath, statePath, snapType string) error {
	if m == nil {
		return fmt.Errorf("nil machine")
	}
	if snapType == SnapshotDiff && !m.diffCapable {
		return fmt.Errorf("diff snapshot requested but VM was not loaded with dirty-page tracking")
	}
	if m.raw != nil {
		return fcAPI(ctx, unixClient(m.raw.sock), "PUT", "/snapshot/create", map[string]any{
			"snapshot_type": snapType,
			"snapshot_path": statePath,
			"mem_file_path": memPath,
		})
	}
	if m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	// SDK machines (cold boots, 1:1 restores) are never diffCapable, so this
	// is always a Full snapshot — CreateSnapshot's default.
	return m.Machine.CreateSnapshot(ctx, memPath, statePath)
}

// StartClone launches an identity-neutral clone from a snapshot. Because the
// SDK v1.0.0 WithSnapshot helper loads+resumes atomically and exposes neither
// network_overrides nor a load→PATCH→resume window, we manage the firecracker
// process ourselves and drive its API over the unix socket:
//
//  1. load snapshot (resume_vm=false) with network_overrides remapping eth0's
//     host tap to the clone's fresh tap;
//  2. PATCH /drives/rootfs to the clone's copy-on-write rootfs;
//  3. PUT /mmds with the clone's fresh IP/MAC/gw (the in-guest thaw agent reads
//     this and reconfigures eth0);
//  4. resume.
//
// The caller must have created c.TapDevice (unbridged) beforehand, and should
// attach it to the bridge only after the guest has reidentified.
func StartClone(ctx context.Context, opts RunOptions, c CloneParams) (mm *Machine, rt RuntimeConfig, err error) {
	if err = opts.applyDefaults(); err != nil {
		return nil, RuntimeConfig{}, err
	}
	vmID := uuid.NewString()

	cmd := exec.CommandContext(ctx, opts.FirecrackerBin, "--api-sock", opts.SocketPath, "--id", vmID, "--no-seccomp")
	logPath := filepath.Join(opts.LogDir, fmt.Sprintf("firecracker-%s.log", vmID))
	if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); ferr == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err = cmd.Start(); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("start firecracker: %w", err)
	}
	rm := &rawMachine{cmd: cmd, sock: opts.SocketPath, doneCh: make(chan struct{})}
	go func() { rm.waitErr = cmd.Wait(); close(rm.doneCh) }()
	// Kill the process on any error below so we don't leak a firecracker.
	defer func() {
		if err != nil {
			_ = cmd.Process.Kill()
		}
	}()

	client := unixClient(opts.SocketPath)
	if err = waitAPI(ctx, client); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("firecracker API never came up: %w", err)
	}

	// 1. Load the snapshot without resuming, remapping the host tap. The iface_id
	// must match what the source VM registered: the SDK names interfaces by index
	// but 1-based (createNetworkInterfaces calls createNetworkInterface with id+1),
	// so our single boot NIC is "1" — NOT "0" and NOT the guest name "eth0".
	// enable_diff_snapshots turns on dirty-page tracking from the moment of
	// load, so a later PUT /snapshot/create with snapshot_type=Diff captures
	// exactly this sandbox's delta over the snapshot it was cloned from.
	load := map[string]any{
		"snapshot_path":         c.StatePath,
		"mem_backend":           map[string]any{"backend_type": "File", "backend_path": c.MemPath},
		"enable_diff_snapshots": true,
		"resume_vm":             false,
		"network_overrides":     []map[string]any{{"iface_id": "1", "host_dev_name": c.TapDevice}},
	}
	if err = fcAPI(ctx, client, "PUT", "/snapshot/load", load); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("load snapshot: %w", err)
	}
	// 2. Relocate the rootfs to this clone's CoW copy.
	if err = fcAPI(ctx, client, "PATCH", "/drives/rootfs", map[string]any{"drive_id": "rootfs", "path_on_host": c.CloneRootfsPath}); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("relocate rootfs: %w", err)
	}
	// 3. Push the clone's fresh identity into MMDS for the guest thaw agent.
	// epoch_ms lets the guest step its snapshot-stale wall clock at thaw,
	// instead of NTP stepping it minutes forward later, mid-exec.
	mmds := map[string]any{
		"ip":       c.GuestIP,
		"mac":      c.MacAddress,
		"gw":       c.GatewayIP,
		"prefix":   strconv.Itoa(c.Prefix),
		"gen":      c.Gen,
		"epoch_ms": strconv.FormatInt(time.Now().UnixMilli(), 10),
	}
	if err = fcAPI(ctx, client, "PUT", "/mmds", mmds); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("set mmds: %w", err)
	}
	// 4. Resume.
	if err = fcAPI(ctx, client, "PATCH", "/vm", map[string]any{"state": "Resumed"}); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("resume: %w", err)
	}

	return &Machine{raw: rm, diffCapable: true}, RuntimeConfig{SocketPath: opts.SocketPath, VMID: vmID}, nil
}

// RestoreUFFD restores a snapshot on its ORIGINAL identity (same-identity wake)
// with the UFFD memory backend, so the guest resumes before its RAM is paged in
// and faults its working set from memPath on demand (see uffd_linux.go). Like
// StartClone it drives Firecracker over the raw socket, because SDK v1.0.0's
// WithSnapshot exposes no mem_backend field. The caller must have recreated the
// baked tap and staged the rootfs at its baked path first; there is no
// network_overrides / drive relocation, so this is a plain load+resume.
func RestoreUFFD(ctx context.Context, opts RunOptions, memPath, statePath string) (mm *Machine, rt RuntimeConfig, err error) {
	if err = opts.applyDefaults(); err != nil {
		return nil, RuntimeConfig{}, err
	}
	vmID := uuid.NewString()
	uffdSock := opts.SocketPath + ".uffd"

	// Build the page source (local mmap, local chunks, or an injected GCS chunk
	// source), then start the handler listening before the load call — Firecracker
	// dials it during LoadSnapshot to hand over the uffd.
	src, err := buildUFFDSource(opts, memPath)
	if err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("build uffd source: %w", err)
	}
	h, err := startUffdHandler(uffdSock, src)
	if err != nil {
		_ = src.close()
		return nil, RuntimeConfig{}, fmt.Errorf("start uffd handler: %w", err)
	}
	defer func() {
		if err != nil {
			h.close()
		}
	}()

	cmd := exec.CommandContext(ctx, opts.FirecrackerBin, "--api-sock", opts.SocketPath, "--id", vmID, "--no-seccomp")
	logPath := filepath.Join(opts.LogDir, fmt.Sprintf("firecracker-%s.log", vmID))
	if f, ferr := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644); ferr == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err = cmd.Start(); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("start firecracker: %w", err)
	}
	rm := &rawMachine{cmd: cmd, sock: opts.SocketPath, doneCh: make(chan struct{}), uffd: h}
	// If the page source can't serve a fault (e.g. a GCS chunk fetch fails after
	// retries), Firecracker would hang forever on the unserved page. Kill it
	// instead: the wake fails cleanly and the sandbox stays hibernated for a
	// retry. Set before LoadSnapshot (which is when FC connects and the fault
	// goroutine starts), so the fault path always observes the callback.
	h.fatal.set(func(error) { _ = cmd.Process.Kill() })
	// When Firecracker exits, the uffd read fails and faultLoop returns on its
	// own — but tear the handler down explicitly too, to unmap the mem file and
	// remove the socket.
	go func() { rm.waitErr = cmd.Wait(); h.close(); close(rm.doneCh) }()
	defer func() {
		if err != nil {
			_ = cmd.Process.Kill()
		}
	}()

	client := unixClient(opts.SocketPath)
	if err = waitAPI(ctx, client); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("firecracker API never came up: %w", err)
	}

	// Load with the UFFD backend and resume in one call. resume_vm=true is safe
	// here (unlike the clone path) because there's no drive/identity fixup to do
	// between load and resume. Firecracker connects to uffdSock during this call.
	load := map[string]any{
		"snapshot_path": statePath,
		"mem_backend":   map[string]any{"backend_type": "Uffd", "backend_path": uffdSock},
		"resume_vm":     true,
	}
	if err = fcAPI(ctx, client, "PUT", "/snapshot/load", load); err != nil {
		return nil, RuntimeConfig{}, fmt.Errorf("load snapshot (uffd): %w", err)
	}
	// Resume is done (the load call above resumed and kicked devices). Only NOW is
	// it safe to launch working-set prewarm: doing it earlier put concurrent chunk
	// fetches in flight during FC's resume-time virtio-ring reads and panicked FC
	// at high concurrency (roadmap B3). Post-resume it races only the guest's own
	// faults, exactly like fault-ahead prefetch (which is safe at high concurrency).
	h.startPrewarm()
	return &Machine{raw: rm, diffCapable: false}, RuntimeConfig{SocketPath: opts.SocketPath, VMID: vmID}, nil
}

// PushEpoch writes the host's current wall clock into a VM's MMDS store so
// the in-guest thaw agent can step the guest's snapshot-stale clock. Used by
// the 1:1 restore path — clones get epoch_ms in their identity doc from
// StartClone. Replaces the MMDS store, which is empty on a restored VM.
func PushEpoch(ctx context.Context, sockPath string) error {
	return fcAPI(ctx, unixClient(sockPath), "PUT", "/mmds", map[string]any{
		"epoch_ms": strconv.FormatInt(time.Now().UnixMilli(), 10),
	})
}

// unixClient builds an HTTP client that talks to a Firecracker API unix socket.
func unixClient(sock string) *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, "unix", sock)
			},
		},
	}
}

// waitAPI polls the Firecracker API socket until it answers (or ctx/timeout).
func waitAPI(ctx context.Context, client *http.Client) error {
	deadline := time.Now().Add(5 * time.Second)
	for {
		req, _ := http.NewRequestWithContext(ctx, "GET", "http://localhost/version", nil)
		if resp, err := client.Do(req); err == nil {
			resp.Body.Close()
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// fcAPI sends a JSON request to the Firecracker API over the unix socket.
func fcAPI(ctx context.Context, client *http.Client, method, path string, body any) error {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://localhost"+path, rdr)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		msg, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("%s %s -> HTTP %d: %s", method, path, resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	return nil
}
