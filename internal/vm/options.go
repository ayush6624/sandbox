package vm

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/google/uuid"
)

// RunOptions configures a microVM run.
type RunOptions struct {
	FirecrackerBin string
	SocketPath     string
	KernelImage    string
	RootfsPath     string
	KernelArgs     string
	Vcpus          int64
	MemMIB         int64
	LogDir         string

	// Networking (optional — if TapDevice is empty, no networking)
	TapDevice   string
	MacAddress  string
	GuestCIDR   string // e.g. "172.16.0.2/24"
	GatewayIP   string // e.g. "172.16.0.1"
	Nameservers string // e.g. "8.8.8.8"
}

// RuntimeConfig captures identifiers after the SDK config is built.
type RuntimeConfig struct {
	SocketPath string
	VMID       string
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
		o.SocketPath = filepath.Join(os.TempDir(), fmt.Sprintf("websandbox-%s.sock", id.String()))
	}
	if o.LogDir == "" {
		o.LogDir = os.TempDir()
	}
	return nil
}
