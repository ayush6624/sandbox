package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"
)

func exposeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "expose <id> <guest_port>",
		Short: "Forward an extra guest port to a host port",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			guestPort, err := strconv.Atoi(args[1])
			if err != nil {
				return fmt.Errorf("invalid guest port %q", args[1])
			}
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			pm, err := c.ExposePort(context.Background(), args[0], guestPort)
			if err != nil {
				return err
			}
			fmt.Printf("guest %d → host %d\n", pm.GuestPort, pm.HostPort)
			return nil
		},
	}
	addClientFlags(cmd)
	return cmd
}

func portsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "ports <id>",
		Short: "List forwarded ports of a sandbox",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			mappings, err := c.ListPorts(context.Background(), args[0])
			if err != nil {
				return err
			}
			tw := tabwriter.NewWriter(os.Stdout, 0, 2, 2, ' ', 0)
			fmt.Fprintln(tw, "GUEST PORT\tHOST PORT")
			for _, pm := range mappings {
				fmt.Fprintf(tw, "%d\t%d\n", pm.GuestPort, pm.HostPort)
			}
			return tw.Flush()
		},
	}
	addClientFlags(cmd)
	return cmd
}
