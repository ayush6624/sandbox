package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func downCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "down <id>",
		Short: "Stop and remove a sandbox via the API server",
		Args:  cobra.ExactArgs(1),
		RunE:  runDown,
	}
	addClientFlags(cmd)
	return cmd
}

func runDown(cmd *cobra.Command, args []string) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	id := args[0]
	if err := c.Destroy(context.Background(), id); err != nil {
		return err
	}
	fmt.Printf("sandbox %s stopped.\n", id)
	return nil
}
