package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func readCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "read <id> <path>",
		Short: "Read a file from a sandbox to stdout",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			rc, err := c.ReadFile(context.Background(), args[0], args[1])
			if err != nil {
				return err
			}
			defer rc.Close()
			_, err = io.Copy(os.Stdout, rc)
			return err
		},
	}
	addClientFlags(cmd)
	return cmd
}

func writeCmd() *cobra.Command {
	var from string
	cmd := &cobra.Command{
		Use:   "write <id> <path>",
		Short: "Write stdin (or --from local file) to a path inside a sandbox",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			var src io.Reader = os.Stdin
			if from != "" {
				f, err := os.Open(from)
				if err != nil {
					return err
				}
				defer f.Close()
				src = f
			}
			return c.WriteFile(context.Background(), args[0], args[1], src)
		},
	}
	addClientFlags(cmd)
	cmd.Flags().StringVar(&from, "from", "", "local file to upload (default: stdin)")
	return cmd
}

func lsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ls <id> [path]",
		Short: "List a directory inside a sandbox",
		Args:  cobra.RangeArgs(1, 2),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			path := ""
			if len(args) == 2 {
				path = args[1]
			}
			entries, err := c.ListDir(context.Background(), args[0], path)
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 2, 4, 2, ' ', 0)
			for _, e := range entries {
				fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", e.Mode, e.Size, e.MTime.Format("Jan 02 15:04"), e.Name)
			}
			return tw.Flush()
		},
	}
	addClientFlags(cmd)
	return cmd
}
