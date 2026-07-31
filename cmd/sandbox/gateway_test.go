package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestGatewayEdgeTokenFlagsFailClosed pins the startup validation on the edge
// credential flags. A gateway with no edge credential falls back to accepting
// the CLIENT credential on /route and /raw-route — which is the disclosure the
// edge domain exists to close — so selecting that fallback must be an explicit
// choice (omit both flags), never the side effect of an unset shell variable
// expanded into a unit file as `--edge-token=`.
func TestGatewayEdgeTokenFlagsFailClosed(t *testing.T) {
	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.tokens")
	if err := os.WriteFile(empty, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	loose := filepath.Join(dir, "loose.tokens")
	if err := os.WriteFile(loose, []byte("edge-token\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "passed empty",
			args: []string{"--edge-token="},
			want: "was passed empty",
		},
		{
			name: "file passed empty",
			args: []string{"--edge-token-file="},
			want: "was passed empty",
		},
		{
			name: "missing file",
			args: []string{"--edge-token-file", filepath.Join(dir, "absent")},
			want: "stat credential file",
		},
		{
			name: "file with no credentials",
			args: []string{"--edge-token-file", empty},
			want: "contains no credentials",
		},
		{
			name: "world-readable file",
			args: []string{"--edge-token-file", loose},
			want: "must not be group/world accessible",
		},
		{
			name: "edge equals client",
			args: []string{"--edge-token", "client-token"},
			want: "edge and client credentials must differ",
		},
		{
			name: "edge equals worker",
			args: []string{"--edge-token", "worker-token"},
			want: "edge and worker-control credentials must differ",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cmd := gatewayCmd()
			cmd.SetArgs(append([]string{
				// A listener that ValidateListener rejects, so a configuration
				// that WAS accepted still fails before binding a port. Any
				// credential error must therefore precede this one.
				"--listen", "0.0.0.0:0",
				"--management-transport", "private_proxy",
				"--token", "client-token",
				"--worker-token", "worker-token",
			}, tc.args...))
			cmd.SilenceUsage, cmd.SilenceErrors = true, true
			err := cmd.Execute()
			if err == nil {
				t.Fatal("accepted an invalid edge credential configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}
