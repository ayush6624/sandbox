package main

import (
	"slices"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/registry"
)

// The whole point of the alias is that it tracks the sandbox, not the address,
// so two sandboxes handed the SAME recycled host port still get distinct
// known_hosts identities.
func TestSSHHostKeyAliasIsPerSandboxNotPerAddress(t *testing.T) {
	first := sshHostKeyAlias("11111111-1111-1111-1111-111111111111")
	second := sshHostKeyAlias("22222222-2222-2222-2222-222222222222")
	if first == second {
		t.Fatalf("two sandboxes share the known_hosts alias %q", first)
	}
	if !strings.Contains(first, "11111111-1111-1111-1111-111111111111") {
		t.Fatalf("alias %q does not carry the sandbox ID", first)
	}
	if got := sshHostKeyAlias("11111111-1111-1111-1111-111111111111"); got != first {
		t.Fatalf("alias is not stable for one sandbox: %q then %q", first, got)
	}

	recycledPort := 5231
	argsA := sshArgs("10.0.0.9", recycledPort, first, "", nil)
	argsB := sshArgs("10.0.0.9", recycledPort, second, "", nil)
	if slices.Equal(argsA, argsB) {
		t.Fatal("same argv for two sandboxes on a recycled port: known_hosts would collide")
	}
}

func TestSSHArgsPinsHostKeyHandlingBeforeDestination(t *testing.T) {
	args := sshArgs("10.0.0.9", 5231, "sandbox-abc", "", nil)

	dest := slices.Index(args, "sandbox@10.0.0.9")
	if dest == -1 {
		t.Fatalf("destination missing from %v", args)
	}
	if dest != len(args)-1 {
		t.Fatalf("destination must be last when there is no remote command: %v", args)
	}
	// ssh treats anything after the destination as the remote command, so every
	// option has to land before it.
	for _, want := range []string{
		"HostKeyAlias=sandbox-abc",
		"CheckHostIP=no",
		"StrictHostKeyChecking=accept-new",
	} {
		at := slices.Index(args, want)
		if at == -1 {
			t.Fatalf("option %q missing from %v", want, args)
		}
		if at > dest {
			t.Fatalf("option %q lands after the destination: %v", want, args)
		}
		if args[at-1] != "-o" {
			t.Fatalf("option %q is not introduced by -o: %v", want, args)
		}
	}
	if port := slices.Index(args, "-p"); port == -1 || args[port+1] != "5231" {
		t.Fatalf("host port not passed: %v", args)
	}
}

func TestSSHArgsPassesRemoteCommandAfterDestination(t *testing.T) {
	args := sshArgs("10.0.0.9", 5231, "sandbox-abc", "", []string{"uname", "-a"})
	dest := slices.Index(args, "sandbox@10.0.0.9")
	if dest == -1 || dest != len(args)-3 {
		t.Fatalf("remote command must follow the destination: %v", args)
	}
	if args[len(args)-2] != "uname" || args[len(args)-1] != "-a" {
		t.Fatalf("remote command mangled: %v", args)
	}
}

func TestSSHArgsJump(t *testing.T) {
	args := sshArgs("10.0.0.9", 5231, "sandbox-abc", "bastion", nil)
	at := slices.Index(args, "-J")
	if at == -1 || args[at+1] != "bastion" {
		t.Fatalf("jump host not passed: %v", args)
	}
	if at > slices.Index(args, "sandbox@10.0.0.9") {
		t.Fatalf("-J lands after the destination: %v", args)
	}
	if plain := sshArgs("10.0.0.9", 5231, "sandbox-abc", "", nil); slices.Contains(plain, "-J") {
		t.Fatalf("-J emitted without a jump host: %v", plain)
	}
}

func TestSSHTarget(t *testing.T) {
	tests := []struct {
		name    string
		sb      registry.Sandbox
		apiAddr string
		want    string
	}{{
		// Fleet mode: ports live on the owning worker, never on the gateway.
		name:    "host addr wins over the api address",
		sb:      registry.Sandbox{HostAddr: "10.160.0.51"},
		apiAddr: "10.160.0.100:9090",
		want:    "10.160.0.51",
	}, {
		name:    "falls back to the api host without its port",
		sb:      registry.Sandbox{},
		apiAddr: "10.160.0.51:8080",
		want:    "10.160.0.51",
	}, {
		name:    "api address without a port is used as-is",
		sb:      registry.Sandbox{},
		apiAddr: "worker.internal",
		want:    "worker.internal",
	}, {
		// Unix socket: the client is on the host holding the listeners.
		name: "local socket resolves to this machine",
		sb:   registry.Sandbox{},
		want: "127.0.0.1",
	}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := sshTarget(tc.sb, tc.apiAddr); got != tc.want {
				t.Fatalf("sshTarget = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestRenderSSHConfigMatchesSSHArgs(t *testing.T) {
	got := renderSSHConfig("sandbox-abc", "10.0.0.9", 5231, "")
	want := strings.Join([]string{
		"Host sandbox-abc",
		"  HostName 10.0.0.9",
		"  Port 5231",
		"  User sandbox",
		"  HostKeyAlias sandbox-abc",
		"  CheckHostIP no",
		"  StrictHostKeyChecking accept-new",
		"",
	}, "\n")
	if got != want {
		t.Fatalf("stanza mismatch:\ngot:\n%s\nwant:\n%s", got, want)
	}

	// The stanza and the wrapper must not drift: every option one applies has
	// to be applied by the other, or `ssh sandbox-<id>` and `sandbox ssh <id>`
	// would handle host keys differently.
	for _, opt := range sshOptions("sandbox-abc") {
		key, value, _ := strings.Cut(opt, "=")
		if !strings.Contains(got, "  "+key+" "+value+"\n") {
			t.Fatalf("stanza is missing %q from sshOptions:\n%s", opt, got)
		}
	}

	withJump := renderSSHConfig("sandbox-abc", "10.0.0.9", 5231, "bastion")
	if !strings.Contains(withJump, "  ProxyJump bastion\n") {
		t.Fatalf("jump host missing from stanza:\n%s", withJump)
	}
	if strings.Contains(got, "ProxyJump") {
		t.Fatalf("ProxyJump emitted without a jump host:\n%s", got)
	}
}
