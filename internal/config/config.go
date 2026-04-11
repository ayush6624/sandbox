package config

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
)

// Config is JSON configuration for a devbox microVM.
type Config struct {
	FirecrackerBin string `json:"firecracker_bin"`
	KernelImage    string `json:"kernel_image"`
	RootfsPath     string `json:"rootfs_path"`
	KernelArgs     string `json:"kernel_args"`

	Vcpus  int64 `json:"vcpus"`
	MemMIB int64 `json:"mem_mib"`

	SocketPath string `json:"socket_path"`
	StatePath  string `json:"state_path"`

	// Networking
	TapDevice   string `json:"tap_device"`
	MacAddress  string `json:"mac_address"`
	GuestCIDR   string `json:"guest_cidr"`
	GatewayIP   string `json:"gateway_ip"`
	Nameservers string `json:"nameservers"`
}

// Defaults fills zero values with sensible defaults for a devbox.
func (c *Config) Defaults() {
	if c.KernelArgs == "" {
		c.KernelArgs = "reboot=k panic=1 pci=off root=/dev/vda rw console=ttyS0"
	}
	if c.Vcpus == 0 {
		c.Vcpus = 2
	}
	if c.MemMIB == 0 {
		c.MemMIB = 1024
	}
	if c.StatePath == "" {
		c.StatePath = "/tmp/websandbox-state.json"
	}
}

// Load reads and decodes path as JSON.
func Load(path string) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode %s: %w", path, err)
	}
	c.Defaults()
	return &c, nil
}
