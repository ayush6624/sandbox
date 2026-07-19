package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/config"
	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/server"
	"github.com/ayush6624/sandbox/internal/vm"
)

var (
	listenAddr    string
	apiToken      string
	gatewayURL    string
	gatewayToken  string
	advertiseAddr string
	hostID        string
)

func serveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the sandbox API server (root required)",
		RunE:  runServe,
	}
	cmd.Flags().StringVar(&cfgPath, "config", "configs/devbox.json", "path to JSON config")
	cmd.Flags().StringVar(&listenAddr, "listen", "", "also serve the API on this TCP address (requires --token); overrides config listen_addr")
	cmd.Flags().StringVar(&apiToken, "token", "", "bearer token for the TCP listener; overrides config api_token")
	cmd.Flags().StringVar(&gatewayURL, "gateway", "", "register with this gateway URL and heartbeat (requires --listen); overrides config gateway_url")
	cmd.Flags().StringVar(&gatewayToken, "gateway-token", "", "bearer token presented to the gateway; overrides config gateway_token")
	cmd.Flags().StringVar(&advertiseAddr, "advertise", "", "address the gateway should dial back; defaults to --listen")
	cmd.Flags().StringVar(&hostID, "host-id", "", "stable host identity reported to the gateway; defaults to hostname")
	return cmd
}

func runServe(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if listenAddr != "" {
		cfg.ListenAddr = listenAddr
	}
	if apiToken != "" {
		cfg.APIToken = apiToken
	}
	if gatewayURL != "" {
		cfg.GatewayURL = gatewayURL
	}
	if gatewayToken != "" {
		cfg.GatewayToken = gatewayToken
	}
	if advertiseAddr != "" {
		cfg.AdvertiseAddr = advertiseAddr
	}
	if hostID != "" {
		cfg.HostID = hostID
	}
	if cfg.GatewayURL != "" && cfg.ListenAddr == "" {
		return fmt.Errorf("--gateway requires --listen (the gateway dials back over TCP)")
	}

	reg, err := registry.Open(cfg.DBPath, cfg.Pools)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	prov := &provisioner.Provisioner{
		Network: provisioner.Network{
			Bridge:      cfg.Bridge,
			GatewayCIDR: cfg.GatewayIP + "/24",
			GuestPort:   cfg.GuestPort,
		},
		RootfsBase:  cfg.RootfsBase,
		RootfsDir:   cfg.RootfsDir,
		SnapshotDir: cfg.SnapshotDir,
	}

	if err := prov.EnsureNetwork(); err != nil {
		return fmt.Errorf("ensure network: %w", err)
	}

	tmpl := vm.RunOptions{
		FirecrackerBin: cfg.FirecrackerBin,
		KernelImage:    cfg.KernelImage,
		KernelArgs:     cfg.KernelArgs,
		Vcpus:          cfg.Vcpus,
		MemMIB:         cfg.MemMIB,
		Nameservers:    cfg.Nameservers,
	}

	srv := server.New(server.Config{
		SocketPath:        cfg.SocketPath,
		ListenAddr:        cfg.ListenAddr,
		APIToken:          cfg.APIToken,
		Provisioner:       prov,
		GatewayIP:         cfg.GatewayIP,
		VMTemplate:        tmpl,
		HotCreate:         !cfg.DisableHotCreate,
		CreateConcurrency: cfg.CreateConcurrency,
		MemBudgetMIB:      cfg.MemBudgetMIB,
		HibernateAfter:    time.Duration(cfg.HibernateAfterSec) * time.Second,
		SnapshotBucket:    cfg.SnapshotBucket,
		GatewayURL:        cfg.GatewayURL,
		GatewayToken:      cfg.GatewayToken,
		AdvertiseAddr:     cfg.AdvertiseAddr,
		HostID:            cfg.HostID,
	}, reg)

	// Every running sandbox costs a handful of fds (firecracker socket, log,
	// FIFO, port-proxy listener + connections); the default 1024 soft limit
	// falls over well before the pools do.
	raiseNoFileLimit()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("sandbox server listening on %s\n", cfg.SocketPath)
	return srv.Serve(ctx)
}
