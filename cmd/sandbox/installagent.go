package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/config"
)

const sandboxdUnit = `[Unit]
Description=Sandbox guest agent (exec + file API for the host)
After=network.target

[Service]
Type=simple
ExecStart=/usr/local/bin/sandboxd
Restart=on-failure
RestartSec=1
Environment=HOME=/home/sandbox

[Install]
WantedBy=multi-user.target
`

func installAgentCmd() *cobra.Command {
	var agentBin string
	cmd := &cobra.Command{
		Use:   "install-agent",
		Short: "Install/update the sandboxd agent inside the base rootfs (root required)",
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := config.Load(cfgPath)
			if err != nil {
				return err
			}
			return installAgent(cfg.RootfsBase, agentBin)
		},
	}
	cmd.Flags().StringVar(&cfgPath, "config", "configs/devbox.json", "path to JSON config")
	cmd.Flags().StringVar(&agentBin, "agent", "./sandboxd", "path to the sandboxd binary to install")
	return cmd
}

// installAgent loop-mounts the base rootfs image, copies the agent binary in,
// and enables its systemd unit (by writing the wants symlink directly).
func installAgent(rootfs, agentBin string) error {
	if _, err := os.Stat(rootfs); err != nil {
		return fmt.Errorf("base rootfs: %w", err)
	}
	bin, err := os.ReadFile(agentBin)
	if err != nil {
		return fmt.Errorf("agent binary: %w", err)
	}

	mnt, err := os.MkdirTemp("", "rootfs-mnt-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)

	if out, err := exec.Command("mount", "-o", "loop", rootfs, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount rootfs: %w: %s", err, out)
	}
	defer func() {
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "umount %s: %v: %s\n", mnt, err, out)
		}
	}()

	if err := os.WriteFile(filepath.Join(mnt, "usr/local/bin/sandboxd"), bin, 0o755); err != nil {
		return fmt.Errorf("write agent: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "etc/systemd/system/sandboxd.service"), []byte(sandboxdUnit), 0o644); err != nil {
		return fmt.Errorf("write unit: %w", err)
	}
	wants := filepath.Join(mnt, "etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return err
	}
	// The default exec cwd (sandboxd falls back to / when it's missing) —
	// creating it here heals base images built before the rootfs script did.
	if err := os.MkdirAll(filepath.Join(mnt, "home/sandbox/app"), 0o755); err != nil {
		return fmt.Errorf("create app dir: %w", err)
	}
	link := filepath.Join(wants, "sandboxd.service")
	_ = os.Remove(link)
	if err := os.Symlink("../sandboxd.service", link); err != nil {
		return fmt.Errorf("enable unit: %w", err)
	}

	fmt.Printf("sandboxd installed into %s and enabled\n", rootfs)
	return nil
}
