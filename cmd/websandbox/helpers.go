package main

import (
	"github.com/spf13/cobra"

	"github.com/ayush6624/web-sandbox/internal/config"
	"github.com/ayush6624/web-sandbox/internal/vm"
)

// Shared persistent flags across commands that need a config file.
var (
	cfgPath string
	fcBin   string
	kernel  string
	rootfs  string
	socket  string
	vcpus   int64
	memMIB  int64
)

// addVMFlags registers the shared flags for commands that create a VM.
func addVMFlags(cmd *cobra.Command) {
	cmd.Flags().StringVar(&cfgPath, "config", "", "path to JSON config (required)")
	_ = cmd.MarkFlagRequired("config")
	cmd.Flags().StringVar(&fcBin, "firecracker", "", "override firecracker binary")
	cmd.Flags().StringVar(&kernel, "kernel", "", "override guest kernel")
	cmd.Flags().StringVar(&rootfs, "rootfs", "", "override rootfs disk path")
	cmd.Flags().StringVar(&socket, "socket", "", "override API socket path")
	cmd.Flags().Int64Var(&vcpus, "vcpus", 0, "override vCPU count")
	cmd.Flags().Int64Var(&memMIB, "mem-mib", 0, "override memory (MiB)")
}

// loadAndMerge loads the config file and merges CLI flag overrides.
func loadAndMerge() (*config.Config, vm.RunOptions, error) {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return nil, vm.RunOptions{}, err
	}
	opts := vm.RunOptions{
		FirecrackerBin: cfg.FirecrackerBin,
		KernelImage:    cfg.KernelImage,
		RootfsPath:     cfg.RootfsPath,
		KernelArgs:     cfg.KernelArgs,
		Vcpus:          cfg.Vcpus,
		MemMIB:         cfg.MemMIB,
		SocketPath:     cfg.SocketPath,

		// Networking
		TapDevice:   cfg.TapDevice,
		MacAddress:  cfg.MacAddress,
		GuestCIDR:   cfg.GuestCIDR,
		GatewayIP:   cfg.GatewayIP,
		Nameservers: cfg.Nameservers,
	}
	if fcBin != "" {
		opts.FirecrackerBin = fcBin
	}
	if kernel != "" {
		opts.KernelImage = kernel
	}
	if rootfs != "" {
		opts.RootfsPath = rootfs
	}
	if socket != "" {
		opts.SocketPath = socket
	}
	if vcpus > 0 {
		opts.Vcpus = vcpus
	}
	if memMIB > 0 {
		opts.MemMIB = memMIB
	}
	return cfg, opts, nil
}
