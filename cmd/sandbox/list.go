package main

import (
	"context"
	"fmt"
	"os"
	"text/tabwriter"

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
	fmt.Fprintln(tw, "ID\tNAME\tGUEST\tTAP\tPID")
	for _, sb := range sandboxes {
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%d\n", sb.ID, sb.Name, sb.GuestIP, sb.TapDevice, sb.PID)
	}
	return tw.Flush()
}
