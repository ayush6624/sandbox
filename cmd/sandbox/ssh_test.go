package main

import (
	"slices"
	"strings"
	"testing"
)

func TestSSHHostKeyAliasIsPerSandbox(t *testing.T) {
	first := sshHostKeyAlias("11111111-1111-1111-1111-111111111111")
	second := sshHostKeyAlias("22222222-2222-2222-2222-222222222222")
	if first == second {
		t.Fatalf("two sandboxes share the known_hosts alias %q", first)
	}
}

func TestSSHArgsUseCLIProxyAndIdentity(t *testing.T) {
	args := sshArgs("abc", "/tmp/test key", "'/opt/sandbox cli' ssh-proxy abc", nil)
	dest := slices.Index(args, "sandbox@abc.sbx.getaion.ai")
	if dest != len(args)-1 {
		t.Fatalf("destination must be last without a remote command: %v", args)
	}
	for _, want := range []string{
		"HostKeyAlias=sandbox-abc",
		"CheckHostIP=no",
		"StrictHostKeyChecking=accept-new",
		"IdentitiesOnly=yes",
		"ProxyCommand='/opt/sandbox cli' ssh-proxy abc",
	} {
		at := slices.Index(args, want)
		if at < 1 || args[at-1] != "-o" || at > dest {
			t.Fatalf("SSH option %q missing or misplaced: %v", want, args)
		}
	}
	if at := slices.Index(args, "-i"); at == -1 || args[at+1] != "/tmp/test key" {
		t.Fatalf("identity missing: %v", args)
	}
}

func TestSSHArgsPassRemoteCommandAfterDestination(t *testing.T) {
	args := sshArgs("abc", "/tmp/key", "sandbox ssh-proxy abc", []string{"uname", "-a"})
	dest := slices.Index(args, "sandbox@abc.sbx.getaion.ai")
	if dest != len(args)-3 || args[len(args)-2] != "uname" || args[len(args)-1] != "-a" {
		t.Fatalf("remote command is not after destination: %v", args)
	}
}

func TestRenderSSHConfigUsesDynamicCLIProxy(t *testing.T) {
	got := renderSSHConfig(
		"sandbox-abc", "abc.sbx.getaion.ai", "/tmp/key", "sandbox ssh-proxy abc",
	)
	for _, want := range []string{
		"Host sandbox-abc\n",
		"  HostName abc.sbx.getaion.ai\n",
		"  User sandbox\n",
		"  IdentityFile /tmp/key\n",
		"  ProxyCommand sandbox ssh-proxy abc\n",
		"  HostKeyAlias sandbox-abc\n",
		"  CheckHostIP no\n",
		"  StrictHostKeyChecking accept-new\n",
		"  IdentitiesOnly yes\n",
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("config missing %q:\n%s", want, got)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("simple-value"); got != "simple-value" {
		t.Fatalf("simple quote = %q", got)
	}
	if got := shellQuote("it's here"); got != `'it'"'"'s here'` {
		t.Fatalf("apostrophe quote = %q", got)
	}
}
