package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/google/uuid"
	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/client"
)

func upCmd() *cobra.Command {
	var name, key string
	var progress, async, legacy bool
	var ttl, hibernateAfter int
	var vcpus, memMIB int64
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create a new sandbox via the API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, stop := signal.NotifyContext(cmd.Context(), os.Interrupt, syscall.SIGTERM)
			defer stop()
			cmd.SetContext(ctx)
			cmd.SilenceUsage = true
			if legacy && (cmd.Flags().Changed("async") || cmd.Flags().Changed("progress") || cmd.Flags().Changed("idempotency-key")) {
				return fmt.Errorf("--legacy cannot be combined with --async, --progress, or --idempotency-key")
			}
			if progress && async {
				return fmt.Errorf("--progress and --async are mutually exclusive")
			}
			if ttl < 0 {
				return fmt.Errorf("--ttl must be non-negative")
			}
			if hibernateAfter < -1 {
				return fmt.Errorf("--hibernate-after must be -1 or non-negative")
			}
			if vcpus < 0 || memMIB < 0 {
				return fmt.Errorf("--vcpus and --mem must be non-negative")
			}
			if memMIB > 0 && memMIB < 128 {
				return fmt.Errorf("--mem must be 0 or at least 128 MiB")
			}
			if legacy {
				return runUpWithContext(cmd.Context(), cmd.OutOrStdout(), cmd.ErrOrStderr(), name, ttl, hibernateAfter, vcpus, memMIB)
			}
			if cmd.Flags().Changed("idempotency-key") && strings.TrimSpace(key) == "" {
				return fmt.Errorf("--idempotency-key must be nonempty")
			}
			if key == "" {
				key = uuid.NewString()
			}
			req := client.CreateRequest{Name: name, Source: client.CreateSource{Type: "default"}, Lifecycle: client.CreateLifecycle{TTLSeconds: ttl, IdleTimeoutSeconds: hibernateAfter}}
			if vcpus != 0 || memMIB != 0 {
				req.Resources = &client.CreateResources{VCPU: vcpus, MemoryMIB: memMIB}
			}
			return runDurableUp(cmd, key, req, async)
		},
	}
	addClientFlags(cmd)
	cmd.Flags().BoolVar(&progress, "progress", false, "wait for durable creation with progress on stderr (the default)")
	cmd.Flags().BoolVar(&legacy, "legacy", false, "use synchronous creation for older servers")
	cmd.Flags().BoolVar(&async, "async", false, "print a durable operation receipt without waiting")
	cmd.Flags().StringVar(&key, "idempotency-key", "", "replay a durable create with the same key and options")
	cmd.Flags().StringVar(&name, "name", "", "display name for the sandbox")
	cmd.Flags().IntVar(&ttl, "ttl", 0, "auto-destroy the sandbox after this many seconds (0 = never)")
	cmd.Flags().IntVar(&hibernateAfter, "hibernate-after", 0, "freeze the sandbox after this many idle seconds (-1 = never, 0 = host default)")
	cmd.Flags().Int64Var(&vcpus, "vcpus", 0, "vCPU override for this sandbox (0 = host template default; forces a cold boot)")
	cmd.Flags().Int64Var(&memMIB, "mem", 0, "memory override in MiB for this sandbox (0 = host template default; forces a cold boot)")
	return cmd
}

func runUpWithContext(ctx context.Context, stdout, stderr io.Writer, name string, ttl, hibernateAfter int, vcpus, memMIB int64) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	sb, err := c.Create(ctx, client.CreateOpts{
		Name:              name,
		TimeoutSec:        ttl,
		HibernateAfterSec: hibernateAfter,
		Vcpus:             vcpus,
		MemMIB:            memMIB,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(stderr, "sandbox %s ready\n", sb.ID)
	enc := json.NewEncoder(stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sb)
}
