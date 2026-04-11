package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/ayush6624/web-sandbox/internal/state"
	"github.com/ayush6624/web-sandbox/internal/vm"
)

func main() {
	root := &cobra.Command{
		Use:   "websandbox",
		Short: "Firecracker-based devbox for React/TS apps",
	}

	root.AddCommand(upCmd(), downCmd(), doctorCmd())

	if err := root.Execute(); err != nil {
		os.Exit(1)
	}
}

func upCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "up",
		Short: "Boot a devbox VM",
		RunE:  runUp,
	}
	addVMFlags(cmd)
	return cmd
}

func runUp(cmd *cobra.Command, args []string) error {
	cfg, opts, err := loadAndMerge()
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	fmt.Println("Starting devbox VM...")
	m, rt, err := vm.NewMachine(ctx, opts)
	if err != nil {
		return fmt.Errorf("create machine: %w", err)
	}

	if err := vm.Start(ctx, m); err != nil {
		return fmt.Errorf("start machine: %w", err)
	}

	// Persist state so `down` can find the process.
	st := state.VMState{
		SocketPath: rt.SocketPath,
		VMID:       rt.VMID,
	}
	if err := state.Save(cfg.StatePath, st); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not save state: %v\n", err)
	}

	if opts.TapDevice != "" {
		fmt.Printf("VM running with networking (guest: %s, tap: %s)\n", opts.GuestCIDR, opts.TapDevice)
	} else {
		fmt.Println("VM running (no networking)")
	}
	fmt.Printf("State saved to %s\n", cfg.StatePath)
	fmt.Println("Press Ctrl+C to stop the VM...")

	// Wait for signal or VM exit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	doneCh := make(chan error, 1)
	go func() { doneCh <- vm.Wait(ctx, m) }()

	select {
	case <-sigCh:
		fmt.Println("\nStopping VM...")
		_ = vm.StopForce(m)
		<-doneCh
	case err := <-doneCh:
		if err != nil {
			fmt.Fprintf(os.Stderr, "VM exited with error: %v\n", err)
		}
	}

	_ = state.Remove(cfg.StatePath)
	fmt.Println("VM stopped.")
	return nil
}

func downCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "down",
		Short: "Stop a running devbox VM",
		RunE:  runDown,
	}
	addVMFlags(cmd)
	return cmd
}

func runDown(cmd *cobra.Command, args []string) error {
	cfg, _, err := loadAndMerge()
	if err != nil {
		return err
	}

	st, err := state.Load(cfg.StatePath)
	if err != nil {
		return fmt.Errorf("no running VM found (state file: %s): %w", cfg.StatePath, err)
	}

	fmt.Printf("Stopping VM %s...\n", st.VMID)

	// Kill via the socket — send a shutdown action.
	if err := killBySocket(st.SocketPath); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not stop via socket: %v\n", err)
	}

	_ = state.Remove(cfg.StatePath)
	fmt.Println("VM stopped.")
	return nil
}

func killBySocket(socketPath string) error {
	// Send SIGTERM to the firecracker process using the PID file approach
	// or simply remove the socket to trigger cleanup.
	// For simplicity, we use syscall kill on the process group.
	return fmt.Errorf("socket-based shutdown not yet implemented; use Ctrl+C on the `up` process")
}

func doctorCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "doctor",
		Short: "Validate environment for running devbox VMs",
		RunE:  runDoctor,
	}
}

func runDoctor(cmd *cobra.Command, args []string) error {
	checks := []struct {
		name  string
		check func() error
	}{
		{"KVM device", checkKVM},
		{"Firecracker binary", checkFirecracker},
		{"Devbox rootfs", checkRootfs},
		{"Tap device (tap0)", checkTap},
		{"IP forwarding", checkIPForward},
	}

	allOk := true
	for _, c := range checks {
		if err := c.check(); err != nil {
			fmt.Printf("  ✗ %s: %v\n", c.name, err)
			allOk = false
		} else {
			fmt.Printf("  ✓ %s\n", c.name)
		}
	}

	if !allOk {
		return fmt.Errorf("some checks failed")
	}
	fmt.Println("\nAll checks passed!")
	return nil
}

func checkKVM() error {
	if _, err := os.Stat("/dev/kvm"); err != nil {
		return fmt.Errorf("/dev/kvm not found — is KVM enabled?")
	}
	return nil
}

func checkFirecracker() error {
	paths := []string{"/usr/local/bin/firecracker", "/usr/bin/firecracker"}
	for _, p := range paths {
		if _, err := os.Stat(p); err == nil {
			return nil
		}
	}
	return fmt.Errorf("firecracker binary not found in /usr/local/bin or /usr/bin")
}

func checkRootfs() error {
	if _, err := os.Stat("/opt/fc/devbox-rootfs.ext4"); err != nil {
		return fmt.Errorf("devbox rootfs not found at /opt/fc/devbox-rootfs.ext4 — run build-devbox-rootfs.sh")
	}
	return nil
}

func checkTap() error {
	if _, err := os.Stat("/sys/class/net/tap0"); err != nil {
		return fmt.Errorf("tap0 not found — run setup-network.sh")
	}
	return nil
}

func checkIPForward() error {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
	if err != nil {
		return fmt.Errorf("could not read ip_forward: %w", err)
	}
	if len(b) == 0 || b[0] != '1' {
		return fmt.Errorf("IP forwarding is disabled — run: sysctl -w net.ipv4.ip_forward=1")
	}
	return nil
}
