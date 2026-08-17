package main

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// setupIdentityEnv repoints the guest identity paths at a temp dir and restores
// them (plus the command hook and restart delay) when the test ends.
func setupIdentityEnv(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	oldMarker, oldPattern := guestIdentityMarker, sshHostKeyPattern
	oldSSHDir, oldAuthorized := sshDir, authorizedKeysPath
	oldRun := runIdentityCommand
	oldDirect := reloadSSHDirect
	oldDelay := identityRestartDelay
	t.Cleanup(func() {
		guestIdentityMarker, sshHostKeyPattern = oldMarker, oldPattern
		sshDir, authorizedKeysPath = oldSSHDir, oldAuthorized
		runIdentityCommand = oldRun
		reloadSSHDirect = oldDirect
		identityRestartDelay = oldDelay
	})
	reloadSSHDirect = func() error { return errors.New("direct reload unavailable in test") }
	identityRestartDelay = 0

	guestIdentityMarker = filepath.Join(dir, "state", "identity")
	sshHostKeyPattern = filepath.Join(dir, "ssh", "ssh_host_*")
	sshDir = filepath.Join(dir, "home", ".ssh")
	authorizedKeysPath = filepath.Join(sshDir, "authorized_keys")
	if err := os.MkdirAll(filepath.Dir(sshHostKeyPattern), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sshDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return dir
}

// keygenTarget returns the -f path ssh-keygen was told to write, and fails the
// test if the invocation asks for anything but a single Ed25519 key.
func keygenTarget(t *testing.T, args []string) string {
	t.Helper()
	target, keyType := "", ""
	for i, arg := range args {
		if arg == "-A" {
			t.Fatalf("ssh-keygen -A also builds RSA-3072 (~1.2s per create in a 2-vCPU guest): %v", args)
		}
		if i+1 >= len(args) {
			continue
		}
		switch arg {
		case "-f":
			target = args[i+1]
		case "-t":
			keyType = args[i+1]
		}
	}
	// Anything but ed25519 here — rsa above all — is the regression this pins.
	if keyType != "ed25519" {
		t.Fatalf("ssh-keygen key type = %q, want ed25519: %v", keyType, args)
	}
	if target == "" {
		t.Fatalf("ssh-keygen has no -f target: %v", args)
	}
	return target
}

// Rotation must REMOVE every inherited credential and STOP the inherited
// listener, on every distinct sandbox, and must not generate a key — generation
// moved to first SSH use (ensureSSHHostKey). Stopping is the load-bearing half:
// a restored clone resumes a live sshd that already holds the source's host key
// in memory, so deleting the key files alone would leave it serving them.
func TestInitializeGuestIdentityRemovesInheritedStateWithoutGenerating(t *testing.T) {
	dir := setupIdentityEnv(t)

	oldKey := filepath.Join(dir, "ssh", "ssh_host_ed25519_key")
	if err := os.WriteFile(oldKey, []byte("inherited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorizedKeysPath, []byte("inherited login"), 0o600); err != nil {
		t.Fatal(err)
	}

	generations, stops := 0, 0
	runIdentityCommand = func(name string, args ...string) error {
		switch name {
		case "ssh-keygen":
			generations++
			return os.WriteFile(keygenTarget(t, args), []byte("unique"), 0o600)
		case "systemctl":
			if len(args) > 0 && args[0] == "stop" {
				stops++
			}
			return nil
		default:
			return errors.New("unexpected command")
		}
	}

	if err := initializeGuestIdentity("sandbox-one"); err != nil {
		t.Fatal(err)
	}
	if generations != 0 {
		t.Fatalf("create generated %d host keys; generation must be deferred to first SSH use", generations)
	}
	if stops != 1 {
		t.Fatalf("inherited sshd stops = %d, want 1 — a live sshd keeps serving the key it already loaded", stops)
	}
	if _, err := os.Stat(oldKey); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inherited host key was not removed: %v", err)
	}
	if _, err := os.Stat(authorizedKeysPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inherited authorized_keys was not removed: %v", err)
	}

	// Same sandbox: idempotent. Note this must hold with NO host key present,
	// which is exactly the state rotation now leaves behind.
	if err := initializeGuestIdentity("sandbox-one"); err != nil {
		t.Fatal(err)
	}
	if stops != 1 {
		t.Fatalf("same identity was not idempotent: stops %d", stops)
	}
	// A different sandbox id (a clone) must rotate again.
	if err := initializeGuestIdentity("sandbox-two"); err != nil {
		t.Fatal(err)
	}
	if stops != 2 {
		t.Fatalf("clone identity did not rotate: stops %d", stops)
	}
	if generations != 0 {
		t.Fatalf("rotation generated %d host keys, want 0", generations)
	}
}

// The generation that create no longer does must happen on first SSH use, still
// Ed25519-only, and must bring the listener back up (rotation stopped it).
func TestEnsureSSHHostKeyGeneratesOnceAndStartsSSHD(t *testing.T) {
	dir := setupIdentityEnv(t)

	generations, restarts := 0, 0
	runIdentityCommand = func(name string, args ...string) error {
		switch name {
		case "ssh-keygen":
			generations++
			return os.WriteFile(keygenTarget(t, args), []byte("unique"), 0o600)
		case "systemctl":
			if len(args) > 0 && (args[0] == "restart" || args[0] == "reload") {
				restarts++
			}
			return nil
		default:
			return errors.New("unexpected command")
		}
	}

	if err := ensureSSHHostKey(); err != nil {
		t.Fatal(err)
	}
	if generations != 1 {
		t.Fatalf("first SSH use generated %d keys, want 1", generations)
	}
	if restarts == 0 {
		t.Fatal("sshd was not started; rotation stopped it, so SSH would not serve")
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh", "ssh_host_ed25519_key")); err != nil {
		t.Fatalf("host key missing after ensureSSHHostKey: %v", err)
	}

	// Second call must not regenerate — a rotated key must never be replaced
	// under a live session — but must still ensure the listener is up.
	before := restarts
	if err := ensureSSHHostKey(); err != nil {
		t.Fatal(err)
	}
	if generations != 1 {
		t.Fatalf("second SSH use regenerated the host key: generations %d", generations)
	}
	if restarts <= before {
		t.Fatal("second call did not ensure sshd was running")
	}
}

func TestGuestIdentityGeneratesEd25519HostKeyOnly(t *testing.T) {
	dir := setupIdentityEnv(t)

	var got []string
	runIdentityCommand = func(name string, args ...string) error {
		if name != "ssh-keygen" {
			return nil
		}
		got = append([]string{name}, args...)
		return os.WriteFile(keygenTarget(t, args), []byte("unique"), 0o600)
	}

	if err := ensureSSHHostKey(); err != nil {
		t.Fatal(err)
	}

	want := []string{
		"ssh-keygen", "-q", "-t", "ed25519",
		"-f", filepath.Join(dir, "ssh", "ssh_host_ed25519_key"),
		"-N", "",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("keygen invocation = %q, want %q", got, want)
	}
	if _, err := os.Stat(filepath.Join(dir, "ssh", "ssh_host_rsa_key")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("an RSA host key was generated: %v", err)
	}
}

func TestRestartSSHServiceRetriesAfterResettingFailure(t *testing.T) {
	oldRun, oldDelay := runIdentityCommand, identityRestartDelay
	oldDirect := reloadSSHDirect
	t.Cleanup(func() {
		runIdentityCommand = oldRun
		reloadSSHDirect = oldDirect
		identityRestartDelay = oldDelay
	})
	reloadSSHDirect = func() error { return errors.New("direct reload unavailable in test") }
	identityRestartDelay = 0

	restarts, resets := 0, 0
	runIdentityCommand = func(name string, args ...string) error {
		if name != "systemctl" || len(args) == 0 {
			return errors.New("unexpected command")
		}
		switch args[0] {
		case "reload":
			return errors.New("restored service cannot reload")
		case "reset-failed":
			resets++
			return nil
		case "restart":
			restarts++
			if restarts < 3 {
				return errors.New("transient restored-service failure")
			}
			return nil
		default:
			return errors.New("unexpected systemctl action")
		}
	}

	if err := restartSSHService(); err != nil {
		t.Fatal(err)
	}
	if restarts != 3 || resets != 3 {
		t.Fatalf("restart attempts=%d resets=%d, want 3 and 3", restarts, resets)
	}
}

func TestRestartSSHServiceUsesVerifiedDirectReload(t *testing.T) {
	oldRun, oldDirect := runIdentityCommand, reloadSSHDirect
	t.Cleanup(func() {
		runIdentityCommand = oldRun
		reloadSSHDirect = oldDirect
	})

	directCalls := 0
	reloadSSHDirect = func() error {
		directCalls++
		return nil
	}
	runIdentityCommand = func(string, ...string) error {
		return errors.New("systemctl must not run after direct reload")
	}
	if err := restartSSHService(); err != nil {
		t.Fatal(err)
	}
	if directCalls != 1 {
		t.Fatalf("direct reload calls=%d, want 1", directCalls)
	}
}

func TestValidSandboxIdentity(t *testing.T) {
	for _, id := range []string{"sandbox-1", "4dcda1a6_9.test"} {
		if !validSandboxIdentity(id) {
			t.Errorf("validSandboxIdentity(%q) = false", id)
		}
	}
	for _, id := range []string{"", "../sandbox", "space here", "line\nbreak"} {
		if validSandboxIdentity(id) {
			t.Errorf("validSandboxIdentity(%q) = true", id)
		}
	}
}

// The stop path guards against a stale pid file and PID reuse, both of which a
// restored guest carries easily: /run is a fresh tmpfs on a cold boot but a
// snapshot-restored guest resumes with whatever /run/sshd.pid held at snapshot
// time. Trusting it blindly would either fail creates (signalling a stranger) or
// silently skip the stop (leaving an inherited key served).
func TestInheritedSSHDPIDRejectsStalePIDAndReuse(t *testing.T) {
	dir := t.TempDir()
	oldPath := sshdPIDPath
	t.Cleanup(func() { sshdPIDPath = oldPath })
	sshdPIDPath = filepath.Join(dir, "sshd.pid")

	// No pid file at all: the ordinary cold-boot case.
	if _, ok := inheritedSSHDPID(); ok {
		t.Fatal("reported an sshd with no pid file present")
	}
	// A pid that is not running.
	if err := os.WriteFile(sshdPIDPath, []byte("999999\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := inheritedSSHDPID(); ok {
		t.Fatal("stale pid file was treated as a live sshd")
	}
	// A live pid that is NOT sshd — this test process. This is the PID-reuse
	// guard: signalling it would kill an unrelated process.
	if err := os.WriteFile(sshdPIDPath, []byte(strconv.Itoa(os.Getpid())), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, ok := inheritedSSHDPID(); ok {
		t.Fatal("a live non-sshd process was treated as the inherited sshd")
	}
	// Garbage, and pid <= 1.
	for _, bad := range []string{"", "not-a-pid", "0", "1", "-5"} {
		if err := os.WriteFile(sshdPIDPath, []byte(bad), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, ok := inheritedSSHDPID(); ok {
			t.Fatalf("pid file %q was accepted", bad)
		}
	}
}

// A guest with no sshd running must not fail its create, however systemctl exits.
func TestStopSSHServiceToleratesNoRunningSSHD(t *testing.T) {
	dir := t.TempDir()
	oldPath, oldRun := sshdPIDPath, runIdentityCommand
	t.Cleanup(func() { sshdPIDPath, runIdentityCommand = oldPath, oldRun })
	sshdPIDPath = filepath.Join(dir, "sshd.pid")
	runIdentityCommand = func(string, ...string) error {
		return errors.New("Failed to stop ssh.service: Unit ssh.service not loaded")
	}
	if err := stopSSHService(); err != nil {
		t.Fatalf("stop with no sshd and a failing systemctl: %v", err)
	}
}
