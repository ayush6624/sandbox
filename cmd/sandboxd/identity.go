package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/agentapi"
)

var (
	guestIdentityMu sync.Mutex

	guestIdentityMarker = "/var/lib/sandboxd/identity"
	sshHostKeyPattern   = "/etc/ssh/ssh_host_*"
	runIdentityCommand  = func(name string, args ...string) error {
		if out, err := exec.Command(name, args...).CombinedOutput(); err != nil {
			return fmt.Errorf("%s %s: %w: %s", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
		}
		return nil
	}
	identityRestartDelay = 100 * time.Millisecond
)

func handleGuestIdentity(w http.ResponseWriter, r *http.Request) {
	var req agentapi.GuestIdentityRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httpError(w, http.StatusBadRequest, fmt.Errorf("decode body: %w", err))
		return
	}
	if err := initializeGuestIdentity(strings.TrimSpace(req.SandboxID)); err != nil {
		code := http.StatusInternalServerError
		if errors.Is(err, errInvalidSandboxIdentity) {
			code = http.StatusBadRequest
		}
		httpError(w, code, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

var errInvalidSandboxIdentity = errors.New("invalid sandbox identity")

// initializeGuestIdentity rotates state that must be unique to every
// independently created sandbox. Its durable marker makes retries for the same
// sandbox idempotent, while a rootfs copied from an image or snapshot always
// carries a different marker and therefore rotates before create returns.
//
// Only an Ed25519 host key is generated: `ssh-keygen -A` also builds RSA-3072,
// which measured ~1.2 s inside a 2-vCPU guest and was almost the entire cost of
// this call (Ed25519 is ~7 ms). sshd is pinned to that key by the baked
// sshd_config.d/sandbox.conf, so the absent RSA/ECDSA keys are not missed.
func initializeGuestIdentity(sandboxID string) error {
	if !validSandboxIdentity(sandboxID) {
		return fmt.Errorf("%w: sandbox_id must be 1-128 letters, digits, dots, underscores, or hyphens", errInvalidSandboxIdentity)
	}

	guestIdentityMu.Lock()
	defer guestIdentityMu.Unlock()

	if marker, err := os.ReadFile(guestIdentityMarker); err == nil &&
		strings.TrimSpace(string(marker)) == sandboxID && sshHostKeysPresent() {
		return nil
	}

	paths, err := filepath.Glob(sshHostKeyPattern)
	if err != nil {
		return fmt.Errorf("list ssh host keys: %w", err)
	}
	for _, path := range paths {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove inherited ssh host key %s: %w", path, err)
		}
	}
	// Never carry a source sandbox's login key into an independent clone.
	if err := os.Remove(authorizedKeysPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove inherited authorized_keys: %w", err)
	}
	// Every /etc/ssh/ssh_host_* file was just removed, so ssh-keygen has nothing
	// to overwrite and cannot prompt; runIdentityCommand also leaves Stdin nil,
	// which exec wires to /dev/null, so it can never block on input either.
	if err := runIdentityCommand("ssh-keygen", "-q", "-t", "ed25519", "-f", sshHostKeyPath("ed25519"), "-N", ""); err != nil {
		return fmt.Errorf("generate ssh host key: %w", err)
	}
	if !sshHostKeysPresent() {
		return errors.New("generate ssh host key: ssh-keygen produced no private host key")
	}
	if err := restartSSHService(); err != nil {
		return fmt.Errorf("restart ssh service: %w", err)
	}
	if err := writeIdentityMarker(sandboxID); err != nil {
		return err
	}
	return nil
}

func restartSSHService() error {
	const attempts = 3
	// A restored systemd can retain the source VM's failed/start-limit state.
	// Clear it before the first restart and between retries. The bounded retry
	// also covers the brief socket handoff race seen when several snapshot
	// clones rotate their inherited SSH identities concurrently.
	_ = runIdentityCommand("systemctl", "reset-failed", "ssh.service")
	var lastErr error
	for attempt := 1; attempt <= attempts; attempt++ {
		if err := runIdentityCommand("systemctl", "restart", "ssh.service"); err == nil {
			return nil
		} else {
			lastErr = err
		}
		if attempt < attempts {
			time.Sleep(identityRestartDelay * time.Duration(attempt))
			_ = runIdentityCommand("systemctl", "reset-failed", "ssh.service")
		}
	}
	return lastErr
}

// sshHostKeyPath names a host key beside the ones sshHostKeyPattern matches, so
// generation and the glob-based checks always agree (including under tests that
// repoint the pattern at a temp dir).
func sshHostKeyPath(keyType string) string {
	return filepath.Join(filepath.Dir(sshHostKeyPattern), "ssh_host_"+keyType+"_key")
}

func sshHostKeysPresent() bool {
	paths, err := filepath.Glob(sshHostKeyPattern)
	if err != nil {
		return false
	}
	for _, path := range paths {
		if !strings.HasSuffix(path, ".pub") {
			if info, err := os.Stat(path); err == nil && info.Mode().IsRegular() && info.Size() > 0 {
				return true
			}
		}
	}
	return false
}

func writeIdentityMarker(sandboxID string) error {
	dir := filepath.Dir(guestIdentityMarker)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create identity state dir: %w", err)
	}
	tmp, err := os.CreateTemp(dir, ".identity-*")
	if err != nil {
		return fmt.Errorf("create identity marker: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath)
	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return fmt.Errorf("chmod identity marker: %w", err)
	}
	if _, err := tmp.WriteString(sandboxID + "\n"); err != nil {
		tmp.Close()
		return fmt.Errorf("write identity marker: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return fmt.Errorf("sync identity marker: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("close identity marker: %w", err)
	}
	if err := os.Rename(tmpPath, guestIdentityMarker); err != nil {
		return fmt.Errorf("install identity marker: %w", err)
	}
	return nil
}

func validSandboxIdentity(id string) bool {
	if id == "" || len(id) > 128 {
		return false
	}
	for _, r := range id {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' ||
			r >= '0' && r <= '9' || r == '.' || r == '_' || r == '-' {
			continue
		}
		return false
	}
	return true
}
