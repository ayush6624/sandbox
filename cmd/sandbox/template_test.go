package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeImage lays out the parts of an extracted container image the overlay
// reads. Paths are relative to the image root.
func fakeImage(t *testing.T, passwd string, files ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, rel := range append(files, "bin/bash", "usr/sbin/ip") {
		path := filepath.Join(root, rel)
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(root, "etc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/passwd"), []byte(passwd), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "etc/group"), []byte("root:x:0:\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// sandboxd resolves the guest account by NAME, so the entry has to exist under
// that name whatever the image already uses uid 1000 for.
func TestEnsureGuestAccount(t *testing.T) {
	t.Run("adds the account at 1000 when free", func(t *testing.T) {
		root := fakeImage(t, "root:x:0:0:root:/root:/bin/bash\n")
		uid, gid, err := ensureGuestAccount(root)
		if err != nil {
			t.Fatal(err)
		}
		if uid != 1000 || gid != 1000 {
			t.Fatalf("uid/gid = %d/%d, want 1000/1000", uid, gid)
		}
		passwd := readFile(t, filepath.Join(root, "etc/passwd"))
		if !strings.Contains(passwd, "sandbox:x:1000:1000:") {
			t.Fatalf("no sandbox entry in %q", passwd)
		}
	})

	t.Run("picks a free uid when the image owns 1000", func(t *testing.T) {
		root := fakeImage(t, "root:x:0:0:root:/root:/bin/bash\nnode:x:1000:1000::/home/node:/bin/bash\n")
		uid, _, err := ensureGuestAccount(root)
		if err != nil {
			t.Fatal(err)
		}
		if uid != 1001 {
			t.Fatalf("uid = %d, want 1001 (1000 is taken by the image)", uid)
		}
	})

	t.Run("keeps an existing sandbox account", func(t *testing.T) {
		root := fakeImage(t, "sandbox:x:1234:5678::/home/sandbox:/bin/bash\n")
		uid, gid, err := ensureGuestAccount(root)
		if err != nil {
			t.Fatal(err)
		}
		if uid != 1234 || gid != 5678 {
			t.Fatalf("uid/gid = %d/%d, want the image's 1234/5678", uid, gid)
		}
		entries := 0
		for _, line := range strings.Split(readFile(t, filepath.Join(root, "etc/passwd")), "\n") {
			if strings.HasPrefix(line, "sandbox:") {
				entries++
			}
		}
		if entries != 1 {
			t.Fatalf("passwd has %d sandbox entries, want 1", entries)
		}
	})
}

// A missing bash or ip yields a template that boots and then misbehaves — an
// unreachable clone or a failing exec — so the build has to refuse up front.
func TestRequireGuestBinaries(t *testing.T) {
	root := fakeImage(t, "root:x:0:0:root:/root:/bin/bash\n")
	if err := requireGuestBinaries(root); err != nil {
		t.Fatalf("complete image rejected: %v", err)
	}
	for _, missing := range []string{"bin/bash", "usr/sbin/ip"} {
		root := fakeImage(t, "root:x:0:0:root:/root:/bin/bash\n")
		if err := os.Remove(filepath.Join(root, missing)); err != nil {
			t.Fatal(err)
		}
		if err := requireGuestBinaries(root); err == nil {
			t.Fatalf("image without %s accepted", missing)
		}
	}
}

func TestImageEnvScript(t *testing.T) {
	script := imageEnvScript([]string{
		"PATH=/opt/venv/bin:/usr/bin",
		"HOME=/root",       // guest-owned, must not leak into the shell
		"QUOTED=it's here", // single quotes must survive the shell quoting
		"MALFORMED",
	})
	if !strings.Contains(script, "export PATH='/opt/venv/bin:/usr/bin'\n") {
		t.Fatalf("PATH not exported: %q", script)
	}
	if strings.Contains(script, "HOME") {
		t.Fatalf("exported guest-owned HOME: %q", script)
	}
	if !strings.Contains(script, `export QUOTED='it'\''s here'`) {
		t.Fatalf("bad quoting: %q", script)
	}
	if strings.Contains(script, "MALFORMED") {
		t.Fatalf("exported a valueless entry: %q", script)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
