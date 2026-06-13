package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/spf13/cobra"
)

func stopServerCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "stop-server",
		Short: "Stop the running sandbox server (SIGTERM; graceful teardown of all sandboxes)",
		RunE: func(cmd *cobra.Command, args []string) error {
			pids, err := findServerPIDs()
			if err != nil {
				return err
			}
			if len(pids) == 0 {
				fmt.Println("no sandbox server running")
				return nil
			}
			sig := syscall.SIGTERM
			if force {
				sig = syscall.SIGKILL
			}
			for _, pid := range pids {
				if err := syscall.Kill(pid, sig); err != nil {
					return fmt.Errorf("kill %d: %w", pid, err)
				}
				fmt.Printf("sent %s to server pid %d\n", sig, pid)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "SIGKILL instead of SIGTERM (skips graceful teardown; next serve will reconcile)")
	return cmd
}

// findServerPIDs scans /proc for `sandbox serve` processes other than ourselves.
func findServerPIDs() ([]int, error) {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return nil, fmt.Errorf("scan /proc (Linux only): %w", err)
	}
	self := os.Getpid()
	var pids []int
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		cmdline, err := os.ReadFile(filepath.Join("/proc", e.Name(), "cmdline"))
		if err != nil {
			continue
		}
		argv := strings.Split(string(cmdline), "\x00")
		if len(argv) >= 2 && strings.HasSuffix(argv[0], "sandbox") && argv[1] == "serve" {
			pids = append(pids, pid)
		}
	}
	return pids, nil
}
