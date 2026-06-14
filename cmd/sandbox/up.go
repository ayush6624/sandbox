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
	var ttl int
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create a new sandbox via the API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runUp(ttl)
		},
	}
	addClientFlags(cmd)
	cmd.Flags().IntVar(&ttl, "ttl", 0, "auto-destroy the sandbox after this many seconds (0 = never)")
	return cmd
}

func runUp(ttl int) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	sb, err := c.Create(context.Background(), client.CreateOpts{TimeoutSec: ttl})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sandbox %s ready → http://localhost:%d\n", sb.ID, sb.HostPort)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sb)
}
