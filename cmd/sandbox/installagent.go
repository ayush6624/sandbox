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

const guestIdentityImageVersion = "2"

const sandboxSSHDConfig = `# Managed by sandbox install-agent — key-only user access.
PermitRootLogin no
PubkeyAuthentication yes
PasswordAuthentication no
KbdInteractiveAuthentication no
AllowUsers sandbox
`

// User-facing commands and shells run as the sandbox account, so these are the
// rc files read by `bash -l` on the /shell pty.
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
	h.Write([]byte(guestIdentityImageVersion))
	h.Write([]byte(sandboxSSHDConfig))
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
	if err := hardenGuestIdentity(mnt); err != nil {
		return err
	}
	wants := filepath.Join(mnt, "etc/systemd/system/multi-user.target.wants")
	if err := os.MkdirAll(wants, 0o755); err != nil {
		return err
	}
	// Shell rc files live in the sandbox user's home and are written
	// unconditionally, like the unit, so updates propagate.
	if err := os.WriteFile(filepath.Join(mnt, "home/sandbox/.profile"), []byte(sandboxProfile), 0o644); err != nil {
		return fmt.Errorf("write .profile: %w", err)
	}
	if err := os.WriteFile(filepath.Join(mnt, "home/sandbox/.bashrc"), []byte(sandboxBashrc), 0o644); err != nil {
		return fmt.Errorf("write .bashrc: %w", err)
	}
	for _, name := range []string{".profile", ".bashrc"} {
		if err := os.Chown(filepath.Join(mnt, "home/sandbox", name), 1000, 1000); err != nil {
			return fmt.Errorf("chown %s: %w", name, err)
		}
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

// hardenGuestIdentity upgrades existing base images as well as newly built
// ones. The rootfs is mounted but not running, so chroot is the least
// surprising way to let the image's own account tools update passwd/shadow.
func hardenGuestIdentity(mnt string) error {
	script := `
set -eu
id sandbox >/dev/null 2>&1 ||
  useradd --create-home --uid 1000 --user-group --shell /bin/bash sandbox
test "$(id -u sandbox)" = 1000
test "$(id -g sandbox)" = 1000
passwd -l sandbox >/dev/null
passwd -l root >/dev/null
install -d -o sandbox -g sandbox -m 0755 /home/sandbox/app
chown -R sandbox:sandbox /home/sandbox
rm -f /etc/ssh/ssh_host_*
systemctl disable ssh.socket >/dev/null 2>&1 || true
systemctl enable ssh.service >/dev/null 2>&1 || true
systemctl disable serial-getty@ttyS0.service >/dev/null 2>&1 || true
`
	if out, err := exec.Command("chroot", mnt, "/bin/sh", "-c", script).CombinedOutput(); err != nil {
		return fmt.Errorf("harden guest identity: %w: %s", err, out)
	}
	sshDir := filepath.Join(mnt, "etc/ssh/sshd_config.d")
	if err := os.MkdirAll(sshDir, 0o755); err != nil {
		return fmt.Errorf("create ssh config dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(sshDir, "sandbox.conf"), []byte(sandboxSSHDConfig), 0o644); err != nil {
		return fmt.Errorf("write ssh config: %w", err)
	}
	return nil
}
