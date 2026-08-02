package main

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"

	"github.com/spf13/cobra"

	"github.com/ayush6624/sandbox/internal/client"
	"github.com/ayush6624/sandbox/internal/registry"
)

// SSH into a sandbox lands on the unprivileged guest account over the userspace
// port proxy. Fleet sandboxes get a public raw-TCP endpoint; single-host
// sandboxes get a host-local forwarded port. Both are allocated on demand.
const (
	sshGuestPort = 22
	sshGuestUser = "sandbox"
)

// sshHostKeyAlias is the identity every host key is recorded under in
// known_hosts. It MUST NOT be the host:port the connection actually uses.
// Host keys are unique per sandbox (see initializeGuestIdentity) while host
// ports are recycled from a pool, so keying on host:port makes the second
// sandbox to be handed port N look like a compromise of the first: OpenSSH
// prints "REMOTE HOST IDENTIFICATION HAS CHANGED!" and refuses to connect. The
// sandbox ID is a UUID that is never reused, so an alias built from it is
// stable for the life of one sandbox and collides with nothing afterwards.
func sshHostKeyAlias(id string) string {
	return "sandbox-" + id
}

// sshTarget resolves the host whose forwarded ports reach this sandbox. In
// fleet mode the gateway annotates the sandbox with its owning worker
// (HostAddr, already port-stripped), because the port-forward listeners live
// there and not on the gateway. Otherwise the ports are on whatever host the
// API itself is on: the given TCP address, or this machine for a Unix socket.
func sshTarget(sb registry.Sandbox, apiAddr string) string {
	if sb.HostAddr != "" {
		return sb.HostAddr
	}
	if apiAddr != "" {
		if host, _, err := net.SplitHostPort(apiAddr); err == nil && host != "" {
			return host
		}
		return apiAddr
	}
	return "127.0.0.1"
}

// sshOptions are the options shared by `sandbox ssh` and the generated config.
//
// HostKeyAlias is the actual fix for recycled host ports (see
// sshHostKeyAlias). CheckHostIP is pinned off because when it is on OpenSSH
// ALSO records an address-keyed entry, which reintroduces exactly the
// collision HostKeyAlias avoids — it defaults off only since OpenSSH 8.5, so
// older clients need it stated. StrictHostKeyChecking=accept-new trusts a
// never-before-seen alias without prompting (there is nothing to compare a
// fresh sandbox's key against, so the prompt is pure friction) while still
// refusing a changed key for an alias already known — which, given the alias
// is per-sandbox, only happens if that sandbox's identity really did change.
func sshOptions(alias string) []string {
	return []string{
		"HostKeyAlias=" + alias,
		"CheckHostIP=no",
		"StrictHostKeyChecking=accept-new",
	}
}

// sshArgs builds the argv for ssh. Options must precede the destination —
// anything after it is taken as the remote command, which is exactly how
// remoteCmd is passed through.
func sshArgs(host string, hostPort int, alias, jump string, remoteCmd []string) []string {
	args := []string{"-p", strconv.Itoa(hostPort)}
	for _, opt := range sshOptions(alias) {
		args = append(args, "-o", opt)
	}
	if jump != "" {
		args = append(args, "-J", jump)
	}
	args = append(args, sshGuestUser+"@"+host)
	return append(args, remoteCmd...)
}

// renderSSHConfig prints an ssh_config stanza so plain ssh/scp/rsync/editors
// reach the sandbox by name with the same host-key handling as `sandbox ssh`.
func renderSSHConfig(alias, host string, hostPort int, jump string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Host %s\n", alias)
	fmt.Fprintf(&b, "  HostName %s\n", host)
	fmt.Fprintf(&b, "  Port %d\n", hostPort)
	fmt.Fprintf(&b, "  User %s\n", sshGuestUser)
	for _, opt := range sshOptions(alias) {
		key, value, _ := strings.Cut(opt, "=")
		fmt.Fprintf(&b, "  %s %s\n", key, value)
	}
	if jump != "" {
		fmt.Fprintf(&b, "  ProxyJump %s\n", jump)
	}
	return b.String()
}

// ensureSSHPort returns the host port forwarding guest 22, forwarding it on
// first use. Exposing works on a hibernated sandbox without waking it, and the
// proxy wakes it when the SSH connection actually arrives.
func ensureSSHPort(ctx context.Context, c *client.Client, id string) (int, error) {
	mappings, err := c.ListPorts(ctx, id)
	if err != nil {
		return 0, err
	}
	for _, pm := range mappings {
		// A URL-only mapping carries no host port; fall through and upgrade it
		// rather than handing back port 0.
		if pm.GuestPort == sshGuestPort && pm.HostPort != 0 {
			return pm.HostPort, nil
		}
	}
	// Demand a host port explicitly: on a host configured with
	// default_url_only, an unqualified expose returns a URL-only mapping and
	// SSH has nothing to dial.
	pm, err := c.ExposeHostPort(ctx, id, sshGuestPort)
	if err != nil {
		return 0, fmt.Errorf("forward guest port %d: %w", sshGuestPort, err)
	}
	if pm.HostPort == 0 {
		return 0, fmt.Errorf("forward guest port %d: host returned no host port (mode %q)", sshGuestPort, pm.Mode)
	}
	return pm.HostPort, nil
}

// ensurePublicSSHPort returns the public raw-TCP endpoint forwarding guest 22.
// Raw allocation is idempotent, so calling it on every SSH connection both
// creates the endpoint on first use and retrieves the stable endpoint later.
func ensurePublicSSHPort(ctx context.Context, c *client.Client, id string) (string, int, error) {
	pm, err := c.ExposeRawPort(ctx, id, sshGuestPort)
	if err != nil {
		return "", 0, fmt.Errorf("allocate public SSH endpoint: %w", err)
	}
	if pm.PublicHost == "" || pm.PublicPort == 0 {
		return "", 0, fmt.Errorf(
			"allocate public SSH endpoint: gateway returned incomplete raw mapping (host %q, port %d, mode %q)",
			pm.PublicHost, pm.PublicPort, pm.Mode,
		)
	}
	return pm.PublicHost, pm.PublicPort, nil
}

// resolveSSHDestination gathers everything both commands need. HostAddr is set
// only by the fleet gateway, where users must connect through public raw TCP
// ingress rather than dialing a private worker. A single-host server omits it,
// so that deployment keeps using its local host-port proxy. An explicit jump
// host opts operators into the private worker path for break-glass debugging.
func resolveSSHDestination(ctx context.Context, c *client.Client, id, apiAddr string, private bool) (string, int, string, error) {
	sb, err := c.Get(ctx, id)
	if err != nil {
		return "", 0, "", err
	}
	if sb.HostAddr != "" && !private {
		host, publicPort, err := ensurePublicSSHPort(ctx, c, id)
		if err != nil {
			return "", 0, "", err
		}
		return host, publicPort, sshHostKeyAlias(sb.ID), nil
	}
	hostPort, err := ensureSSHPort(ctx, c, id)
	if err != nil {
		return "", 0, "", err
	}
	return sshTarget(sb, apiAddr), hostPort, sshHostKeyAlias(sb.ID), nil
}

func sshCmd() *cobra.Command {
	var jump string
	cmd := &cobra.Command{
		Use:   "ssh <id> [-- <command...>]",
		Short: "SSH into a sandbox (exposes guest port 22 on first use)",
		Long: "SSH into a sandbox as the unprivileged " + sshGuestUser + " user.\n\n" +
			"Fleet sandboxes receive a public raw-TCP endpoint on first use; single-host\n" +
			"sandboxes receive a host-local forwarded port. The sandbox's\n" +
			"host key is recorded in known_hosts under a per-sandbox alias so recycled\n" +
			"ports never look like a changed host key. Requires the sandbox to have\n" +
			"been created with an SSH key (sandbox up --ssh-key ...).",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			host, hostPort, alias, err := resolveSSHDestination(context.Background(), c, args[0], gwAddr, jump != "")
			if err != nil {
				return err
			}
			return runSSH(sshArgs(host, hostPort, alias, jump, args[1:]))
		},
	}
	cmd.Flags().StringVar(&jump, "jump", "", "use a ProxyJump host and the private worker route (operator fallback)")
	addClientFlags(cmd)
	return cmd
}

func sshConfigCmd() *cobra.Command {
	var jump string
	cmd := &cobra.Command{
		Use:   "ssh-config <id>",
		Short: "Print an ssh_config stanza for a sandbox (for ssh/scp/rsync/editors)",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			_, c, err := dialClient()
			if err != nil {
				return err
			}
			cmd.SilenceUsage = true
			host, hostPort, alias, err := resolveSSHDestination(context.Background(), c, args[0], gwAddr, jump != "")
			if err != nil {
				return err
			}
			fmt.Print(renderSSHConfig(alias, host, hostPort, jump))
			return nil
		},
	}
	cmd.Flags().StringVar(&jump, "jump", "", "emit a ProxyJump line using the private worker route (operator fallback)")
	addClientFlags(cmd)
	return cmd
}

// runSSH hands the terminal to ssh and exits with its status, so `sandbox ssh`
// behaves like ssh itself (including for scripted remote commands).
func runSSH(args []string) error {
	path, err := exec.LookPath("ssh")
	if err != nil {
		return fmt.Errorf("ssh client not found in PATH: %w", err)
	}
	proc := exec.Command(path, args...)
	proc.Stdin, proc.Stdout, proc.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := proc.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode())
		}
		return err
	}
	return nil
}
