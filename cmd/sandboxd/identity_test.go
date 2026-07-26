package main

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestInitializeGuestIdentityRotatesOncePerSandbox(t *testing.T) {
	dir := t.TempDir()
	oldMarker, oldPattern := guestIdentityMarker, sshHostKeyPattern
	oldSSHDir, oldAuthorized := sshDir, authorizedKeysPath
	oldRun := runIdentityCommand
	oldDelay := identityRestartDelay
	t.Cleanup(func() {
		guestIdentityMarker, sshHostKeyPattern = oldMarker, oldPattern
		sshDir, authorizedKeysPath = oldSSHDir, oldAuthorized
		runIdentityCommand = oldRun
		identityRestartDelay = oldDelay
	})
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
	oldKey := filepath.Join(dir, "ssh", "ssh_host_ed25519_key")
	if err := os.WriteFile(oldKey, []byte("inherited"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authorizedKeysPath, []byte("inherited login"), 0o600); err != nil {
		t.Fatal(err)
	}

	generations, restarts := 0, 0
	runIdentityCommand = func(name string, args ...string) error {
		switch name {
		case "ssh-keygen":
			generations++
			return os.WriteFile(oldKey, []byte("unique"), 0o600)
		case "systemctl":
			if len(args) > 0 && args[0] == "restart" {
				restarts++
			}
			return nil
		default:
			return errors.New("unexpected command")
		}
	}

	if err := initializeGuestIdentity("sandbox-one"); err != nil {
		t.Fatal(err)
	}
	if generations != 1 || restarts != 1 {
		t.Fatalf("first initialization commands = generations %d, restarts %d", generations, restarts)
	}
	if _, err := os.Stat(authorizedKeysPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("inherited authorized_keys was not removed: %v", err)
	}
	if err := initializeGuestIdentity("sandbox-one"); err != nil {
		t.Fatal(err)
	}
	if generations != 1 || restarts != 1 {
		t.Fatalf("same identity was not idempotent: generations %d, restarts %d", generations, restarts)
	}
	if err := initializeGuestIdentity("sandbox-two"); err != nil {
		t.Fatal(err)
	}
	if generations != 2 || restarts != 2 {
		t.Fatalf("clone identity did not rotate: generations %d, restarts %d", generations, restarts)
	}
}

func TestRestartSSHServiceRetriesAfterResettingFailure(t *testing.T) {
	oldRun, oldDelay := runIdentityCommand, identityRestartDelay
	t.Cleanup(func() {
		runIdentityCommand = oldRun
		identityRestartDelay = oldDelay
	})
	identityRestartDelay = 0

	restarts, resets := 0, 0
	runIdentityCommand = func(name string, args ...string) error {
		if name != "systemctl" || len(args) == 0 {
			return errors.New("unexpected command")
		}
		switch args[0] {
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
