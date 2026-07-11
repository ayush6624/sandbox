package main

import (
	"context"
	"fmt"

	"github.com/spf13/cobra"
)

func hibernateCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hibernate <id>",
		Short: "Freeze an idle sandbox to disk, releasing its slot (next exec wakes it)",
		Args:  cobra.ExactArgs(1),
		RunE:  runHibernate,
	}
	addClientFlags(cmd)
	return cmd
}

func runHibernate(cmd *cobra.Command, args []string) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	sb, err := c.Hibernate(context.Background(), args[0])
	if err != nil {
		return err
	}
	fmt.Printf("sandbox %s hibernated.\n", sb.ID)
	return nil
}
