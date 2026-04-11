//go:build linux

package vm

import (
	"context"
	"fmt"
	"io"
	"net"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/google/uuid"
	"github.com/sirupsen/logrus"

	fcsdk "github.com/firecracker-microvm/firecracker-go-sdk"
	"github.com/firecracker-microvm/firecracker-go-sdk/client/models"
)

// Machine wraps the Firecracker SDK machine (Linux only).
type Machine struct {
	*fcsdk.Machine
}

func (o RunOptions) fcConfig() (fcsdk.Config, error) {
	if err := o.applyDefaults(); err != nil {
		return fcsdk.Config{}, err
	}

	uid, err := uuid.NewRandom()
	if err != nil {
		return fcsdk.Config{}, err
	}
	logFIFO := filepath.Join(o.LogDir, fmt.Sprintf("websandbox-log-%s.fifo", uid.String()))

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

	// Configure networking if a tap device is specified.
	if o.TapDevice != "" {
		iface, err := buildNetworkInterface(o)
		if err != nil {
			return fcsdk.Config{}, fmt.Errorf("network config: %w", err)
		}
		cfg.NetworkInterfaces = fcsdk.NetworkInterfaces{iface}
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

func buildCommand(ctx context.Context, fcCfg fcsdk.Config, fcBin string) *exec.Cmd {
	builder := fcsdk.VMCommandBuilder{}.
		WithBin(fcBin).
		WithSocketPath(fcCfg.SocketPath).
		AddArgs("--id", fcCfg.VMID)
	if !fcCfg.Seccomp.Enabled {
		builder = builder.AddArgs("--no-seccomp")
	} else if len(fcCfg.Seccomp.Filter) > 0 {
		builder = builder.AddArgs("--seccomp-filter", fcCfg.Seccomp.Filter)
	}
	return builder.Build(ctx)
}

func silentLog() *logrus.Entry {
	l := logrus.New()
	l.SetOutput(io.Discard)
	return logrus.NewEntry(l)
}

// NewMachine builds a Machine from RunOptions.
func NewMachine(ctx context.Context, opts RunOptions) (*Machine, RuntimeConfig, error) {
	fcCfg, err := opts.fcConfig()
	if err != nil {
		return nil, RuntimeConfig{}, err
	}

	cmd := buildCommand(ctx, fcCfg, opts.FirecrackerBin)
	m, err := fcsdk.NewMachine(ctx, fcCfg, fcsdk.WithProcessRunner(cmd), fcsdk.WithLogger(silentLog()))
	if err != nil {
		return nil, RuntimeConfig{}, err
	}
	rt := RuntimeConfig{SocketPath: fcCfg.SocketPath, VMID: fcCfg.VMID}
	return &Machine{m}, rt, nil
}

// Start boots the VMM and sends InstanceStart.
func Start(ctx context.Context, m *Machine) error {
	if m == nil || m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	return m.Machine.Start(ctx)
}

// StopForce sends SIGTERM to the Firecracker process.
func StopForce(m *Machine) error {
	if m == nil || m.Machine == nil {
		return nil
	}
	return m.Machine.StopVMM()
}

// Wait blocks until the Firecracker process exits.
func Wait(ctx context.Context, m *Machine) error {
	if m == nil || m.Machine == nil {
		return fmt.Errorf("nil machine")
	}
	return m.Machine.Wait(ctx)
}
