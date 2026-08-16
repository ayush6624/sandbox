package main

import (
	"os"
	"strings"
	"testing"
)

// The guest sudoers rule has two sources of truth: sandboxSudoers here (which
// repairs an existing base image) and the heredoc in build-devbox-rootfs.sh
// (which builds a new one). A rootfs prepared by either route has to end up
// with the same rule, and nothing else fails when they drift — the image just
// quietly comes out without sudo on one of the two paths.
func TestSudoersRuleMatchesRootfsBuilder(t *testing.T) {
	const rule = "sandbox ALL=(ALL) NOPASSWD: ALL"

	if !strings.Contains(sandboxSudoers, rule) {
		t.Fatalf("sandboxSudoers does not grant %q:\n%s", rule, sandboxSudoers)
	}
	script, err := os.ReadFile("../../scripts/build-devbox-rootfs.sh")
	if err != nil {
		t.Fatalf("read rootfs builder: %v", err)
	}
	if !strings.Contains(string(script), rule) {
		t.Fatalf("build-devbox-rootfs.sh does not grant %q — the two rootfs paths have drifted", rule)
	}
}
