package main

import (
	"context"
	"fmt"
	"text/tabwriter"
	"os"

	"github.com/spf13/cobra"
)

func listCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "list",
		Short: "List running sandboxes",
		RunE:  runList,
	}
	addClientFlags(cmd)
	return cmd
}

func runList(cmd *cobra.Command, args []string) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	sandboxes, err := c.List(context.Background())
	if err != nil {
		return err
	}
	if len(sandboxes) == 0 {
		fmt.Println("no running sandboxes")
		return nil
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
	fmt.Fprintln(tw, "ID\tGUEST\tHOST PORT\tTAP\tPID")
	for _, sb := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%d\t%s\t%d\n", sb.ID, sb.GuestIP, sb.HostPort, sb.TapDevice, sb.PID)
	}
	return tw.Flush()
}
