package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/client"
)

func upCmd() *cobra.Command {
	var name, sshKey string
	var ttl, hibernateAfter int
	var vcpus, memMIB int64
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Create a new sandbox via the API server",
		RunE: func(cmd *cobra.Command, args []string) error {
			pubkey, err := resolveSSHKey(sshKey)
			if err != nil {
				return err
			}
			return runUp(name, ttl, hibernateAfter, vcpus, memMIB, pubkey)
		},
	}
	addClientFlags(cmd)
	cmd.Flags().StringVar(&name, "name", "", "display name for the sandbox")
	cmd.Flags().IntVar(&ttl, "ttl", 0, "auto-destroy the sandbox after this many seconds (0 = never)")
	cmd.Flags().IntVar(&hibernateAfter, "hibernate-after", 0, "freeze the sandbox after this many idle seconds (-1 = never, 0 = host default)")
	cmd.Flags().Int64Var(&vcpus, "vcpus", 0, "vCPU override for this sandbox (0 = host template default; forces a cold boot)")
	cmd.Flags().Int64Var(&memMIB, "mem", 0, "memory override in MiB for this sandbox (0 = host template default; forces a cold boot)")
	cmd.Flags().StringVar(&sshKey, "ssh-key", "", "install an SSH public key for root login — expose guest port 22 before connecting")
	return cmd
}

// resolveSSHKey turns the --ssh-key flag into a public-key line: empty stays
// empty; a value naming an existing file is read from disk; anything else is
// treated as the key literal.
func resolveSSHKey(v string) (string, error) {
	v = strings.TrimSpace(v)
	if v == "" {
		return "", nil
	}
	if info, err := os.Stat(v); err == nil && !info.IsDir() {
		b, err := os.ReadFile(v)
		if err != nil {
			return "", fmt.Errorf("read ssh key %s: %w", v, err)
		}
		return strings.TrimSpace(string(b)), nil
	}
	return v, nil
}

func runUp(name string, ttl, hibernateAfter int, vcpus, memMIB int64, sshPubkey string) error {
	_, c, err := dialClient()
	if err != nil {
		return err
	}
	sb, err := c.Create(context.Background(), client.CreateOpts{
		Name:              name,
		TimeoutSec:        ttl,
		HibernateAfterSec: hibernateAfter,
		Vcpus:             vcpus,
		MemMIB:            memMIB,
		SSHPubkey:         sshPubkey,
	})
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "sandbox %s ready\n", sb.ID)
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(sb)
}
