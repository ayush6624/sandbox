package server

import (
	"strings"
	"testing"
)

func TestValidateSSHPubkey(t *testing.T) {
	const ed25519 = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIExampleKeyMaterialHere user@host"
	for _, tc := range []struct {
		name         string
		key          string
		wantErr      bool
		errSubstring string
	}{
		{name: "empty is fine (no ssh)", key: ""},
		{name: "ed25519", key: ed25519},
		{name: "rsa", key: "ssh-rsa AAAAB3NzaC1yc2E user@host"},
		{name: "ecdsa", key: "ecdsa-sha2-nistp256 AAAAE2Vj user@host"},
		{name: "sk-ed25519", key: "sk-ssh-ed25519@openssh.com AAAAGnNr user@host"},
		{name: "embedded newline", key: ed25519 + "\nssh-rsa AAAAB attacker", wantErr: true, errSubstring: "single line"},
		{name: "carriage return", key: ed25519 + "\r", wantErr: true, errSubstring: "single line"},
		{name: "not a key", key: "hello world", wantErr: true, errSubstring: "OpenSSH public key"},
		{name: "too long", key: "ssh-ed25519 " + strings.Repeat("A", 9<<10), wantErr: true, errSubstring: "too long"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSSHPubkey(tc.key)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("validateSSHPubkey(%q) should fail", tc.key)
				}
				if !strings.Contains(err.Error(), tc.errSubstring) {
					t.Fatalf("error %q should mention %q", err, tc.errSubstring)
				}
				return
			}
			if err != nil {
				t.Fatalf("validateSSHPubkey(%q): %v", tc.key, err)
			}
		})
	}
}
