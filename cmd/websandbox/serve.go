package main

import (
	"context"
	"fmt"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ayush6624/web-sandbox/internal/config"
	"github.com/ayush6624/web-sandbox/internal/provisioner"
	"github.com/ayush6624/web-sandbox/internal/registry"
	"github.com/ayush6624/web-sandbox/internal/server"
	"github.com/ayush6624/web-sandbox/internal/vm"
)

func serveCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "serve",
		Short: "Run the sandbox API server (root required)",
		RunE:  runServe,
	}
	cmd.Flags().StringVar(&cfgPath, "config", "configs/devbox.json", "path to JSON config")
	return cmd
}

func runServe(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}

	reg, err := registry.Open(cfg.DBPath, cfg.Pools)
	if err != nil {
		return fmt.Errorf("open registry: %w", err)
	}
	defer reg.Close()

	prov := &provisioner.Provisioner{
		Network: provisioner.Network{
			Bridge:    cfg.Bridge,
			GuestPort: cfg.GuestPort,
		},
		RootfsBase: cfg.RootfsBase,
		RootfsDir:  cfg.RootfsDir,
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
		SocketPath:  cfg.SocketPath,
		Provisioner: prov,
		GatewayIP:   cfg.GatewayIP,
		VMTemplate:  tmpl,
	}, reg)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	fmt.Printf("websandbox server listening on %s\n", cfg.SocketPath)
	return srv.Serve(ctx)
}
