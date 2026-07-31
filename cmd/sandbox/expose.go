package main

import (
	"context"
	"fmt"
	"os"
	"strconv"
	"text/tabwriter"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/registry"
)

func exposeCmd() *cobra.Command {
	var urlOnly bool
	var raw bool
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
			if raw {
				if urlOnly {
					return fmt.Errorf("--raw and --url-only are mutually exclusive")
				}
				pm, err := c.ExposeRawPort(context.Background(), args[0], guestPort)
				if err != nil {
					return err
				}
				fmt.Printf("guest %d → %s:%d\n", pm.GuestPort, pm.PublicHost, pm.PublicPort)
				return nil
			}
			var pm registry.PortMapping
			if urlOnly {
				pm, err = c.ExposeURLPort(context.Background(), args[0], guestPort)
			} else {
				pm, err = c.ExposePort(context.Background(), args[0], guestPort)
			}
			if err != nil {
				return err
			}
			if pm.HostPort != 0 {
				fmt.Printf("guest %d → host %d", pm.GuestPort, pm.HostPort)
				if pm.URL != "" {
					fmt.Printf(" (%s)", pm.URL)
				}
				fmt.Println()
			} else if pm.URL != "" {
				fmt.Printf("guest %d → %s\n", pm.GuestPort, pm.URL)
			} else {
				fmt.Printf("guest %d exposed\n", pm.GuestPort)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&urlOnly, "url-only", false, "expose through public ingress without reserving a worker host port")
	cmd.Flags().BoolVar(&raw, "raw", false, "allocate a public raw TCP port (gateway only)")
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
			fmt.Fprintln(tw, "GUEST PORT\tHOST PORT\tMODE\tURL")
			for _, pm := range mappings {
				host := "-"
				if pm.HostPort != 0 {
					host = strconv.Itoa(pm.HostPort)
				}
				fmt.Fprintf(tw, "%d\t%s\t%s\t%s\n", pm.GuestPort, host, pm.Mode, pm.URL)
			}
			return tw.Flush()
		},
	}
	addClientFlags(cmd)
	return cmd
}

func unexposeCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "unexpose <id> <guest_port>",
		Short: "Remove a sandbox port exposure and release any raw public port",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			guestPort, err := strconv.Atoi(args[1])
			if err != nil || guestPort < 1 || guestPort > 65535 {
				return fmt.Errorf("invalid guest port %q", args[1])
			}
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			return c.DeletePort(context.Background(), args[0], guestPort)
		},
	}
	addClientFlags(cmd)
	return cmd
}
