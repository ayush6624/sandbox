package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	reloadSSHDirect      = reloadSSHWithSignal
	identityRestartDelay = 100 * time.Millisecond
	// sshdPIDPath is where sshd records its master pid; a variable so tests can
	// exercise the stop path without a real sshd.
	sshdPIDPath = "/run/sshd.pid"
	// inheritedSSHDPIDFn is the live-sshd probe, injectable so tests can pin the
	// stop path without a real sshd (its comm check cannot be faked).
	inheritedSSHDPIDFn = inheritedSSHDPID
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

	// Marker match alone is idempotent now: this call only REMOVES inherited
	// state, which is idempotent by nature. It deliberately does not require
	// sshHostKeysPresent() any more — after rotation there is no host key, by
	// design, until ensureSSHHostKey generates one on first SSH use.
	if marker, err := os.ReadFile(guestIdentityMarker); err == nil &&
		strings.TrimSpace(string(marker)) == sandboxID {
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
	// Removing the key FILES is not sufficient and this is the whole reason the
	// old code generated eagerly: a restored clone resumes a LIVE sshd that
	// already loaded the source's host key into memory, so it would keep serving
	// it and one sandbox could impersonate another. Stop that listener. It is a
	// signal, not a keygen and not a 500 ms listener poll, and on a cold boot
	// there is nothing running to stop (the base image ships no host key, so
	// sshd never came up).
	if err := stopSSHService(); err != nil {
		return fmt.Errorf("stop inherited ssh service: %w", err)
	}
	return writeIdentityMarker(sandboxID)
}

// ensureSSHHostKey generates this sandbox's unique Ed25519 host key and brings
// sshd up, on first SSH use rather than on every create.
//
// Create used to pay this unconditionally, and it is not cheap: the Ed25519
// keygen itself is ~7 ms, but the `ssh-keygen` fork plus restartSSHService —
// which SIGHUPs sshd and then polls /proc/net/tcp every 1 ms for up to 500 ms
// waiting for a replacement listener inode — measured ~148 ms idle and ~685 ms
// under a 16-way fanout. Most sandboxes never use SSH, so that was per-clone
// work spent on nothing; a 32-way fanout is guest-CPU-bound, so it came
// straight off the fanout's critical path.
//
// The uniqueness guarantee is unchanged. initializeGuestIdentity still removes
// every inherited key and stops the inherited listener EAGERLY, so a sandbox
// can never serve a key it did not generate itself — it simply has no SSH at
// all until this runs.
func ensureSSHHostKey() error {
	guestIdentityMu.Lock()
	defer guestIdentityMu.Unlock()

	// A template built from a container image need not contain OpenSSH. There is
	// then no host key to generate and no sshd to impersonate, so SSH access
	// fails on its own terms rather than failing this call.
	if _, err := exec.LookPath("ssh-keygen"); err != nil {
		log.Print("identity: no ssh-keygen in this image; skipping host key generation")
		return nil
	}
	if !sshHostKeysPresent() {
		// Every /etc/ssh/ssh_host_* file was removed at rotation, so ssh-keygen
		// has nothing to overwrite and cannot prompt; runIdentityCommand also
		// leaves Stdin nil, which exec wires to /dev/null, so it can never block
		// on input either.
		if err := runIdentityCommand("ssh-keygen", "-q", "-t", "ed25519", "-f", sshHostKeyPath("ed25519"), "-N", ""); err != nil {
			return fmt.Errorf("generate ssh host key: %w", err)
		}
		if !sshHostKeysPresent() {
			return errors.New("generate ssh host key: ssh-keygen produced no private host key")
		}
	}
	// Unconditional, not only after generating: rotation stopped sshd, so a
	// second call with a key already on disk still has to bring the listener up.
	if err := restartSSHService(); err != nil {
		return fmt.Errorf("start ssh service: %w", err)
	}
	return nil
}

// stopSSHService stops the sshd a clone inherited from its snapshot.
//
// Best-effort about HOW, strict about the OUTCOME. This is deliberately not
// "trust systemctl's exit code": the failure here is fatal to the create,
// because a listener still holding an inherited key is the impersonation this
// rotation exists to prevent — so it asserts that no inherited sshd master
// process remains rather than that a command reported success. A benign non-zero
// exit (no such unit, no service manager) must not fail a create, and a
// *successful*-looking stop that left the process up must.
func stopSSHService() error {
	// A template guest has no service manager: sandboxd owns sshd directly.
	if initMode() {
		stopOwnSSHD()
		return nil
	}
	// Cheap check FIRST. `systemctl stop` is a D-Bus round trip that measured
	// ~120 ms in-guest — about what the eager keygen it replaced cost — so
	// running it unconditionally made this whole deferral nearly worthless
	// (measured: identity stayed at 108-135 ms per clone). Since the golden is
	// itself built by a cold boot, which has no host key and therefore never
	// starts sshd, its clones inherit no listener at all and the common case is
	// that there is nothing to stop.
	pid, ok := inheritedSSHDPIDFn()
	if !ok {
		return nil // nothing running — the ordinary case
	}
	// Something IS serving an inherited key. Prefer the service manager, because
	// it is deterministic about respawn: a bare SIGTERM can leave systemd
	// restarting a unit whose Restart= policy counts the signal as a failure, and
	// it would then fail repeatedly with no host key and burn the start limit.
	_ = runIdentityCommand("systemctl", "stop", "ssh.service")
	if _, still := inheritedSSHDPIDFn(); !still {
		return nil
	}
	// systemd absent or the unit unknown: signal the master pid directly.
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signal inherited sshd pid %d: %w", pid, err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, still := inheritedSSHDPIDFn(); !still {
			return nil
		}
		if time.Now().After(deadline) {
			// Escalate once before giving up; a create must not proceed while an
			// inherited key is still being served.
			_ = syscall.Kill(pid, syscall.SIGKILL)
			if _, still := inheritedSSHDPIDFn(); still {
				return fmt.Errorf("inherited sshd pid %d still serving after stop", pid)
			}
			return nil
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// inheritedSSHDPID reports the sshd master process recorded in /run/sshd.pid,
// and whether one is actually alive and really is sshd (guarding against PID
// reuse and a stale pid file, which a restored guest can easily carry).
func inheritedSSHDPID() (int, bool) {
	rawPID, err := os.ReadFile(sshdPIDPath)
	if err != nil {
		return 0, false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || pid <= 1 {
		return 0, false
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil || strings.TrimSpace(string(comm)) != "sshd" {
		return 0, false
	}
	return pid, true
}

func restartSSHService() error {
	// In a template guest there is no service manager at all — sandboxd is
	// PID 1's child and owns sshd directly, so none of the fallbacks below
	// apply and the systemctl ones would only turn a working guest into a
	// failed create.
	if initMode() {
		return restartOwnSSHD()
	}

	// Snapshot restores preserve sshd's master PID. Signal that process
	// directly, then prove it completed re-exec by observing a replacement
	// port-22 listener inode. sshd loads host keys before opening that listener,
	// so this avoids systemd's D-Bus round trip and `sshd -t` while retaining a
	// deterministic no-inherited-key readiness gate.
	if err := reloadSSHDirect(); err == nil {
		return nil
	}

	// A successful reload makes sshd re-exec and adopt the new host key without
	// tearing down the listener. This is both safer for concurrent connections
	// and materially faster than a full systemd restart on snapshot clones.
	// Keep it as the compatibility fallback for guests without procfs or an
	// sshd PID file.
	if err := runIdentityCommand("systemctl", "reload", "ssh.service"); err == nil {
		return nil
	}

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

func reloadSSHWithSignal() error {
	pidPath := sshdPIDPath
	rawPID, err := os.ReadFile(pidPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", pidPath, err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(rawPID)))
	if err != nil || pid <= 1 {
		return fmt.Errorf("invalid sshd pid %q", strings.TrimSpace(string(rawPID)))
	}
	comm, err := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
	if err != nil {
		return fmt.Errorf("verify sshd pid %d: %w", pid, err)
	}
	if strings.TrimSpace(string(comm)) != "sshd" {
		return fmt.Errorf("pid %d is %q, not sshd", pid, strings.TrimSpace(string(comm)))
	}
	oldListener, err := sshdListenerInode(pid)
	if err != nil {
		return fmt.Errorf("find sshd listener: %w", err)
	}
	if err := syscall.Kill(pid, syscall.SIGHUP); err != nil {
		return fmt.Errorf("signal sshd pid %d: %w", pid, err)
	}

	deadline := time.Now().Add(500 * time.Millisecond)
	for {
		newListener, listenerErr := sshdListenerInode(pid)
		if listenerErr == nil && newListener != "" && newListener != oldListener {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("sshd did not replace listener inode %s: %w", oldListener, listenerErr)
		}
		time.Sleep(time.Millisecond)
	}
}

func sshdListenerInode(pid int) (string, error) {
	socketInodes := make(map[string]struct{})
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return "", err
	}
	for _, entry := range entries {
		target, err := os.Readlink(fmt.Sprintf("/proc/%d/fd/%s", pid, entry.Name()))
		if err == nil && strings.HasPrefix(target, "socket:[") && strings.HasSuffix(target, "]") {
			socketInodes[strings.TrimSuffix(strings.TrimPrefix(target, "socket:["), "]")] = struct{}{}
		}
	}

	for _, table := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		raw, err := os.ReadFile(table)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		for _, line := range strings.Split(string(raw), "\n") {
			fields := strings.Fields(line)
			// local_address ends in :0016 (port 22), state 0A is LISTEN,
			// and field 9 is the socket inode in both proc tables.
			if len(fields) > 9 && strings.HasSuffix(fields[1], ":0016") && fields[3] == "0A" {
				if _, ok := socketInodes[fields[9]]; ok {
					return fields[9], nil
				}
			}
		}
	}
	return "", errors.New("port 22 listener not owned by sshd")
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
