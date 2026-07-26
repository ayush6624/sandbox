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
	listenAddr       string
	apiToken         string
	apiTokenFile     string
	workerToken      string
	workerTokenFile  string
	gatewayURL       string
	gatewayToken     string
	gatewayTokenFile string
	transportMode    string
	tlsCertFile      string
	tlsKeyFile       string
	advertiseAddr    string
	hostID           string
	workerRelease    string
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
	cmd.Flags().StringVar(&apiTokenFile, "token-file", "", "reloadable newline-delimited client API credentials")
	cmd.Flags().StringVar(&workerToken, "worker-token", "", "gateway-to-worker bearer credential (must differ from the client token in production)")
	cmd.Flags().StringVar(&workerTokenFile, "worker-token-file", "", "reloadable newline-delimited gateway-to-worker credentials")
	cmd.Flags().StringVar(&gatewayURL, "gateway", "", "register with this gateway URL and heartbeat (requires --listen); overrides config gateway_url")
	cmd.Flags().StringVar(&gatewayToken, "gateway-token", "", "worker-control bearer credential presented to the gateway")
	cmd.Flags().StringVar(&gatewayTokenFile, "gateway-token-file", "", "reloadable worker-control credentials presented to the gateway")
	cmd.Flags().StringVar(&transportMode, "management-transport", "", "TCP security mode: tls, private_proxy, or explicit development")
	cmd.Flags().StringVar(&tlsCertFile, "tls-cert", "", "TLS certificate file (atomically replace to rotate)")
	cmd.Flags().StringVar(&tlsKeyFile, "tls-key", "", "TLS private-key file (atomically replace to rotate)")
	cmd.Flags().StringVar(&advertiseAddr, "advertise", "", "address the gateway should dial back; defaults to --listen")
	cmd.Flags().StringVar(&hostID, "host-id", "", "stable host identity reported to the gateway; defaults to hostname")
	cmd.Flags().StringVar(&workerRelease, "worker-release", "", "deployed worker generation reported to the gateway")
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
	if apiTokenFile != "" {
		cfg.APITokenFile = apiTokenFile
	}
	if workerToken != "" {
		cfg.WorkerToken = workerToken
	}
	if workerTokenFile != "" {
		cfg.WorkerTokenFile = workerTokenFile
	}
	if gatewayURL != "" {
		cfg.GatewayURL = gatewayURL
	}
	if gatewayToken != "" {
		cfg.GatewayControlToken = gatewayToken
	}
	if gatewayTokenFile != "" {
		cfg.GatewayControlTokenFile = gatewayTokenFile
	}
	if transportMode != "" {
		cfg.ManagementTransport = transportMode
	}
	if tlsCertFile != "" {
		cfg.TLSCertFile = tlsCertFile
	}
	if tlsKeyFile != "" {
		cfg.TLSKeyFile = tlsKeyFile
	}
	if advertiseAddr != "" {
		cfg.AdvertiseAddr = advertiseAddr
	}
	if hostID != "" {
		cfg.HostID = hostID
	}
	if workerRelease != "" {
		cfg.WorkerRelease = workerRelease
	}
	if cfg.GatewayURL != "" && cfg.ListenAddr == "" {
		return fmt.Errorf("--gateway requires --listen (the gateway dials back over TCP)")
	}
	jailerCfg := jailerConfigFrom(cfg)
	if jailerCfg != nil {
		if _, err := checkJailerPrerequisites(cfg); err != nil {
			return fmt.Errorf("jailer prerequisites: %w", err)
		}
		reconciled, err := vm.ReconcileJailer(*jailerCfg)
		if err != nil {
			return fmt.Errorf("reconcile jailed VMM state: %w", err)
		}
		fmt.Printf("jailer reconciliation: processes=%d jails=%d identities=%d cgroups=%d\n",
			reconciled.ProcessesTerminated, reconciled.JailsRemoved,
			reconciled.IdentitiesReleased, reconciled.CgroupsRemoved)
	}

	reg, err := registry.Open(cfg.DBPath, cfg.Pools)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	prov := &provisioner.Provisioner{
		Network: provisioner.Network{
			Bridge:                 cfg.Bridge,
			GatewayCIDR:            fmt.Sprintf("%s/%d", cfg.GatewayIP, cfg.GuestSubnetBits),
			AllowInterGuestTraffic: cfg.AllowInterGuestNetwork,
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
		DisableSeccomp: cfg.DisableSeccomp,
		LogMaxBytes:    cfg.FirecrackerLogMaxBytes,
		LogRetention:   time.Duration(cfg.FirecrackerLogRetentionHours) * time.Hour,
		LogMaxFiles:    cfg.FirecrackerLogMaxFiles,
		Jailer:         jailerCfg,
	}

	srv := server.New(server.Config{
		SocketPath:          cfg.SocketPath,
		ListenAddr:          cfg.ListenAddr,
		APIToken:            cfg.APIToken,
		APITokens:           cfg.APITokens,
		APITokenFile:        cfg.APITokenFile,
		WorkerToken:         cfg.WorkerToken,
		WorkerTokens:        cfg.WorkerTokens,
		WorkerTokenFile:     cfg.WorkerTokenFile,
		ManagementTransport: cfg.ManagementTransport,
		TLSCertFile:         cfg.TLSCertFile,
		TLSKeyFile:          cfg.TLSKeyFile,
		Provisioner:         prov,
		GatewayIP:           cfg.GatewayIP,
		GuestSubnetBits:     cfg.GuestSubnetBits,
		VMTemplate:          tmpl,
		HotCreate:           !cfg.DisableHotCreate,
		CreateConcurrency:   cfg.CreateConcurrency,
		PlacementDelay:      time.Duration(cfg.PlacementDelaySec) * time.Second,
		MemBudgetMIB:        cfg.MemBudgetMIB,
		HibernateAfter:      time.Duration(cfg.HibernateAfterSec) * time.Second,
		UFFDRestore:         cfg.UFFDRestore,
		UFFDChunkBytes:      uint64(cfg.UFFDChunkKiB) * 1024,
		UFFDChunkGCS:        cfg.UFFDChunkGCS,
		UFFDChunkPrefetch:   cfg.UFFDChunkPrefetch,
		SnapshotBucket:      cfg.SnapshotBucket,
		GatewayURL:          cfg.GatewayURL,
		GatewayToken:        firstNonEmpty(cfg.GatewayControlToken, cfg.GatewayToken),
		GatewayTokens:       cfg.GatewayControlTokens,
		GatewayTokenFile:    cfg.GatewayControlTokenFile,
		AdvertiseAddr:       cfg.AdvertiseAddr,
		HostID:              cfg.HostID,
		WorkerRelease:       cfg.WorkerRelease,
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

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}
	return ""
}
