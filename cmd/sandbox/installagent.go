package main

import (
	"crypto/sha256"
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

// Interactive shells run as root with HOME=/home/sandbox (see the unit above),
// so these rc files — not /root's or /etc/skel's — are what `bash -l` on the
// /shell pty actually reads. Without them the shell has no color at all.
const sandboxProfile = `# ~/.profile: sourced by login shells (sandboxd's /shell runs bash -l).
if [ -n "$BASH" ] && [ -f "$HOME/.bashrc" ]; then
	. "$HOME/.bashrc"
fi
`

const sandboxBashrc = `# ~/.bashrc for sandbox shells — enable colors (baked by install-agent).
case $- in *i*) ;; *) return ;; esac

eval "$(dircolors -b 2>/dev/null)"
alias ls='ls --color=auto'
alias ll='ls --color=auto -al'
alias grep='grep --color=auto'
alias fgrep='fgrep --color=auto'
alias egrep='egrep --color=auto'
alias diff='diff --color=auto'
alias ip='ip -color=auto'

PS1='\[\e[1;32m\]\u@\h\[\e[0m\]:\[\e[1;34m\]\w\[\e[0m\]\$ '
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
//
// When nothing changed since the last install (same agent bytes + same baked
// payloads, rootfs untouched since), it must be a true no-op — not even a
// mount, which would dirty the image file's mtime. goldenUsable keys the
// golden snapshot's validity on the base rootfs (mtime, size): the fleet runs
// install-agent on every alloc start, and gratuitously touching the image
// forced a golden rebuild on every serve restart — orphaning diff-hibernated
// sandboxes anchored to the old golden. The sidecar stamp records what was
// installed and the rootfs stat it left behind.
func installAgent(rootfs, agentBin string) error {
	fi, err := os.Stat(rootfs)
	if err != nil {
		return fmt.Errorf("base rootfs: %w", err)
	}
	bin, err := os.ReadFile(agentBin)
	if err != nil {
		return fmt.Errorf("agent binary: %w", err)
	}

	h := sha256.New()
	h.Write(bin)
	h.Write([]byte(sandboxdUnit))
	h.Write([]byte(sandboxProfile))
	h.Write([]byte(sandboxBashrc))
	sum := fmt.Sprintf("%x", h.Sum(nil))
	stampPath := rootfs + ".agent-stamp"
	stamp := fmt.Sprintf("%s %d %d\n", sum, fi.ModTime().Unix(), fi.Size())
	if prev, err := os.ReadFile(stampPath); err == nil && string(prev) == stamp {
		fmt.Printf("sandboxd already installed in %s (unchanged); leaving the image untouched\n", rootfs)
		return nil
	}

	mnt, err := os.MkdirTemp("", "rootfs-mnt-")
	if err != nil {
		return err
	}
	defer os.Remove(mnt)

	if out, err := exec.Command("mount", "-o", "loop", rootfs, mnt).CombinedOutput(); err != nil {
		return fmt.Errorf("mount rootfs: %w: %s", err, out)
	}
	unmounted := false
	umount := func() {
		if unmounted {
			return
		}
		unmounted = true
		if out, err := exec.Command("umount", mnt).CombinedOutput(); err != nil {
			fmt.Fprintf(os.Stderr, "umount %s: %v: %s\n", mnt, err, out)
		}
	}
	defer umount()

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
	// Shell rc files live in /home/sandbox (the shell's HOME), not /root —
	// written unconditionally, like the unit, so updates propagate.
	if err := os.WriteFile(filepath.Join(mnt, "home/sandbox/.profile"), []byte(sandboxProfile), 0o644); err != nil {
		return fmt.Errorf("write .profile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "home/sandbox/.bashrc"), []byte(sandboxBashrc), 0o644); err != nil {
		return fmt.Errorf("write .bashrc: %w", err)
	}
	link := filepath.Join(wants, "sandboxd.service")
	_ = os.Remove(link)
	if err := os.Symlink("../sandboxd.service", link); err != nil {
		return fmt.Errorf("enable unit: %w", err)
	}

	// Unmount NOW (idempotent with the defer) so the image is fully flushed —
	// only then is its stat final and safe to stamp.
	umount()
	if fi, err := os.Stat(rootfs); err == nil {
		newStamp := fmt.Sprintf("%s %d %d\n", sum, fi.ModTime().Unix(), fi.Size())
		if err := os.WriteFile(stampPath, []byte(newStamp), 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "write agent stamp: %v (next install-agent will rewrite the image)\n", err)
		}
	}

	fmt.Printf("sandboxd installed into %s and enabled\n", rootfs)
	return nil
}
