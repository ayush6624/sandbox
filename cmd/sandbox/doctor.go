package main

import (
	"fmt"
	"net"
	"os"
	"os/exec"
	"runtime"
	"strings"

	"github.com/fatih/color"
	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/config"
)

func doctorCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "doctor",
		Short: "Validate environment for running the sandbox server",
		RunE:  runDoctor,
	}
	cmd.Flags().StringVar(&cfgPath, "config", "configs/devbox.json", "path to JSON config")
	return cmd
}

func runDoctor(cmd *cobra.Command, args []string) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return fmt.Errorf("load config: %w", err)
	}

	pass, fail, warn := 0, 0, 0
	red := color.New(color.FgRed).SprintFunc()
	green := color.New(color.FgGreen).SprintFunc()
	yellow := color.New(color.FgYellow).SprintFunc()

	check := func(name string, fn func() (string, error)) {
		detail, err := fn()
		if err != nil {
			fmt.Printf("  %s %s — %v\n", red("✗"), name, err)
			fail++
		} else {
			fmt.Printf("  %s %s%s\n", green("✓"), name, detail)
			pass++
		}
	}
	warnCheck := func(name string, fn func() (string, error)) {
		detail, err := fn()
		if err != nil {
			fmt.Printf("  %s %s — %v\n", yellow("!"), name, err)
			warn++
		} else {
			fmt.Printf("  %s %s%s\n", green("✓"), name, detail)
			pass++
		}
	}

	fmt.Println()
	fmt.Println("sandbox doctor")
	fmt.Println()

	fmt.Println("Platform")
	check("Linux", func() (string, error) {
		if runtime.GOOS != "linux" {
			return "", fmt.Errorf("running on %s (need linux)", runtime.GOOS)
		}
		return fmt.Sprintf(" (%s/%s)", runtime.GOOS, runtime.GOARCH), nil
	})
	check("KVM", func() (string, error) {
		if _, err := os.Stat("/dev/kvm"); err != nil {
			return "", fmt.Errorf("/dev/kvm not accessible — is KVM enabled?")
		}
		return " (/dev/kvm)", nil
	})
	fmt.Println()

	fmt.Println("Firecracker")
	check("Binary", func() (string, error) {
		if _, err := os.Stat(cfg.FirecrackerBin); err != nil {
			return "", fmt.Errorf("not found at %s", cfg.FirecrackerBin)
		}
		return fmt.Sprintf(" (%s)", cfg.FirecrackerBin), nil
	})
	warnCheck("Version", func() (string, error) {
		out, err := exec.Command(cfg.FirecrackerBin, "--version").CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("could not determine")
		}
		first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0]
		return fmt.Sprintf(" (%s)", first), nil
	})
	check("Seccomp policy", func() (string, error) {
		if cfg.DisableSeccomp {
			return "", fmt.Errorf("disabled by config — remove disable_seccomp for production")
		}
		return " (restrictive Firecracker filters enabled)", nil
	})
	check("VMM log bound", func() (string, error) {
		if cfg.FirecrackerLogMaxBytes <= 0 {
			return "", fmt.Errorf("firecracker_log_max_bytes must be positive")
		}
		return fmt.Sprintf(" (%d bytes per VM)", cfg.FirecrackerLogMaxBytes), nil
	})
	check("VMM log retention", func() (string, error) {
		if cfg.FirecrackerLogRetentionHours <= 0 || cfg.FirecrackerLogMaxFiles <= 0 {
			return "", fmt.Errorf("log retention hours and max files must be positive")
		}
		return fmt.Sprintf(" (%dh, max %d retained failures)",
			cfg.FirecrackerLogRetentionHours, cfg.FirecrackerLogMaxFiles), nil
	})
	fmt.Println()

	fmt.Println("Guest assets")
	check("Kernel", func() (string, error) {
		if _, err := os.Stat(cfg.KernelImage); err != nil {
			return "", fmt.Errorf("not found at %s", cfg.KernelImage)
		}
		return fmt.Sprintf(" (%s)", cfg.KernelImage), nil
	})
	check("Base rootfs", func() (string, error) {
		if _, err := os.Stat(cfg.RootfsBase); err != nil {
			return "", fmt.Errorf("not found at %s — run build-devbox-rootfs.sh", cfg.RootfsBase)
		}
		return fmt.Sprintf(" (%s)", cfg.RootfsBase), nil
	})
	warnCheck("Reflink CoW", func() (string, error) {
		if err := probeReflink(cfg.RootfsDir); err != nil {
			return "", fmt.Errorf("%s can't reflink — rootfs/restore/fan-out fall back to a full copy (put it on XFS/btrfs): %v", cfg.RootfsDir, err)
		}
		return fmt.Sprintf(" (%s supports cp --reflink)", cfg.RootfsDir), nil
	})
	fmt.Println()

	fmt.Println("Networking")
	check(fmt.Sprintf("Bridge (%s)", cfg.Bridge), func() (string, error) {
		if _, err := os.Stat("/sys/class/net/" + cfg.Bridge); err != nil {
			return "", fmt.Errorf("%s not found — run setup-network.sh", cfg.Bridge)
		}
		return "", nil
	})
	warnCheck("IP forwarding", func() (string, error) {
		b, err := os.ReadFile("/proc/sys/net/ipv4/ip_forward")
		if err != nil {
			return "", fmt.Errorf("could not read ip_forward: %w", err)
		}
		if len(b) == 0 || b[0] != '1' {
			return "", fmt.Errorf("disabled — run: sysctl -w net.ipv4.ip_forward=1")
		}
		return "", nil
	})
	check("Bridge firewall", func() (string, error) {
		b, err := os.ReadFile("/proc/sys/net/bridge/bridge-nf-call-iptables")
		if err != nil {
			return "", fmt.Errorf("br_netfilter unavailable: %w", err)
		}
		if len(b) == 0 || b[0] != '1' {
			return "", fmt.Errorf("bridge-nf-call-iptables disabled — guest isolation rules are ineffective")
		}
		if cfg.AllowInterGuestNetwork {
			return "", fmt.Errorf("allow_inter_guest_network is enabled")
		}
		return " (guest-to-guest isolation enabled)", nil
	})
	fmt.Println()

	fmt.Println("Server")
	warnCheck("API socket", func() (string, error) {
		if _, err := os.Stat(cfg.SocketPath); err != nil {
			return "", fmt.Errorf("not present — server not running")
		}
		c, err := net.Dial("unix", cfg.SocketPath)
		if err != nil {
			return "", fmt.Errorf("cannot dial: %w", err)
		}
		c.Close()
		return fmt.Sprintf(" (%s)", cfg.SocketPath), nil
	})

	fmt.Println()
	if fail > 0 {
		fmt.Printf("%s, %d passed", red(fmt.Sprintf("%d failed", fail)), pass)
		if warn > 0 {
			fmt.Printf(", %s", yellow(fmt.Sprintf("%d warnings", warn)))
		}
		fmt.Println()
		return fmt.Errorf("%d check(s) failed", fail)
	}
	if warn > 0 {
		fmt.Printf("%s, %s\n", green(fmt.Sprintf("%d passed", pass)), yellow(fmt.Sprintf("%d warnings", warn)))
	} else {
		fmt.Printf("%s\n", green(fmt.Sprintf("All %d checks passed", pass)))
	}
	return nil
}

// probeReflink verifies dir's filesystem supports copy-on-write clones by
// actually attempting `cp --reflink=always` on a tiny temp file (the same
// mechanism provisioner.CloneFile relies on) and cleaning up after itself.
func probeReflink(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	src, err := os.CreateTemp(dir, ".reflink-probe-*")
	if err != nil {
		return err
	}
	srcPath := src.Name()
	_, _ = src.WriteString("probe")
	src.Close()
	dstPath := srcPath + ".clone"
	defer os.Remove(srcPath)
	defer os.Remove(dstPath)
	if out, err := exec.Command("cp", "--reflink=always", srcPath, dstPath).CombinedOutput(); err != nil {
		return fmt.Errorf("%s", strings.TrimSpace(string(out)))
	}
	return nil
}
