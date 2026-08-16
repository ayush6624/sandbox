package main

import (
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"sync"
	"time"
)

// Guests built from a container image (`sandbox template build`) have no
// service manager, so sandboxd owns sshd there itself: it starts on the first
// identity rotation — not before, since sshd exits immediately when no host key
// exists — and restarts on every later one, which is what stops a clone from
// serving the host key it inherited from the template.
//
// The process handle lives in memory and therefore survives snapshot, restore,
// and hibernation wake along with the rest of the agent.
var (
	sshdMu   sync.Mutex
	sshdProc *exec.Cmd
)

// sshdCandidates covers the paths sshd installs to; /usr/sbin is not on the
// PATH sandboxd inherits, so LookPath alone would miss the usual one.
var sshdCandidates = []string{"/usr/sbin/sshd", "/usr/local/sbin/sshd", "/sbin/sshd"}

func sshdBinary() (string, error) {
	for _, path := range sshdCandidates {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return path, nil
		}
	}
	return exec.LookPath("sshd")
}

// restartOwnSSHD (re)starts the agent-owned sshd and waits for it to own a
// port-22 listener, so a create that returns has SSH serving the key it just
// generated. An image without sshd is not an error: the template simply has no
// SSH, and `sandbox ssh` against it fails on its own terms.
func restartOwnSSHD() error {
	path, err := sshdBinary()
	if err != nil {
		log.Print("ssh: no sshd in this image; skipping (template guests without OpenSSH have no SSH access)")
		return nil
	}

	sshdMu.Lock()
	defer sshdMu.Unlock()

	if sshdProc != nil && sshdProc.Process != nil {
		_ = sshdProc.Process.Kill()
		_ = sshdProc.Wait()
		sshdProc = nil
	}
	// Debian's sshd refuses to start without its privilege separation
	// directory, and /run is a fresh tmpfs on every boot.
	if err := os.MkdirAll("/run/sshd", 0o755); err != nil {
		return fmt.Errorf("create /run/sshd: %w", err)
	}

	// -D keeps sshd in the foreground so this process stays its parent; -e
	// sends its log to our stderr, which is the VM console.
	cmd := exec.Command(path, "-D", "-e")
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start sshd: %w", err)
	}
	sshdProc = cmd
	// Reap it here rather than in PID 1's generic reaper: it is our child.
	go func() { _ = cmd.Wait() }()

	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, err := sshdListenerInode(cmd.Process.Pid); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("sshd started but never opened a port 22 listener")
		}
		time.Sleep(5 * time.Millisecond)
	}
}
