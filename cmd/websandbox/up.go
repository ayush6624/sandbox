package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

func upCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create a new sandbox via the API server",
		RunE:  runUp,
	}
	addClientFlags(cmd)
	return cmd
}

func runUp(cmd *cobra.Command, args []string) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	sb, err := c.Create(context.Background())
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sandbox %s ready → http://localhost:%d\n", sb.ID, sb.HostPort)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sb)
}
