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

	"github.com/ayush6624/web-sandbox/internal/config"
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
