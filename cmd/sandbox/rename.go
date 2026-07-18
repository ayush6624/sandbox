package main

import (
	"context"
	"encoding/json"
	"os"

	"github.com/spf13/cobra"
)

func renameCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "rename <id> <name>",
		Short: "Set a sandbox's display name (\"\" clears it)",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			sb, err := c.Rename(context.Background(), args[0], args[1])
			if err != nil {
				return err
			}
			enc := json.NewEncoder(os.Stdout)
			enc.SetIndent("", "  ")
			return enc.Encode(sb)
		},
	}
	addClientFlags(cmd)
	return cmd
}
