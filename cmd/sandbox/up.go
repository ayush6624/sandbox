package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/client"
)

func upCmd() *cobra.Command {
	var name string
	var ttl, hibernateAfter int
	var vcpus, memMIB int64
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create a new sandbox via the API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUp(name, ttl, hibernateAfter, vcpus, memMIB)
		},
	}
	addClientFlags(cmd)
	cmd.Flags().StringVar(&name, "name", "", "display name for the sandbox")
	cmd.Flags().IntVar(&ttl, "ttl", 0, "auto-destroy the sandbox after this many seconds (0 = never)")
	cmd.Flags().IntVar(&hibernateAfter, "hibernate-after", 0, "freeze the sandbox after this many idle seconds (-1 = never, 0 = host default)")
	cmd.Flags().Int64Var(&vcpus, "vcpus", 0, "vCPU override for this sandbox (0 = host template default; forces a cold boot)")
	cmd.Flags().Int64Var(&memMIB, "mem", 0, "memory override in MiB for this sandbox (0 = host template default; forces a cold boot)")
	return cmd
}

func runUp(name string, ttl, hibernateAfter int, vcpus, memMIB int64) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	sb, err := c.Create(context.Background(), client.CreateOpts{
		Name:              name,
		TimeoutSec:        ttl,
		HibernateAfterSec: hibernateAfter,
		Vcpus:             vcpus,
		MemMIB:            memMIB,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sandbox %s ready → http://localhost:%d\n", sb.ID, sb.HostPort)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sb)
}
