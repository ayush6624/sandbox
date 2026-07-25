package vm

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestBoundedLogFileCapsPersistedBytes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firecracker.log")
	w, err := openBoundedLog(path, 8)
	if err != nil {
		t.Fatal(err)
	}
	if n, err := w.Write([]byte("123456")); err != nil || n != 6 {
		t.Fatalf("first write = (%d, %v), want (6, nil)", n, err)
	}
	if n, err := w.Write([]byte("7890")); err != nil || n != 4 {
		t.Fatalf("second write = (%d, %v), want (4, nil)", n, err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, []byte("12345678")) {
		t.Fatalf("log = %q, want exact 8-byte prefix", got)
	}
	if info, err := os.Stat(path); err != nil {
		t.Fatal(err)
	} else if info.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o, want 600", info.Mode().Perm())
	}
}

func TestBoundedLogFileWriteFailureStillDrainsProducer(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firecracker-failed-sink.log")
	w, err := openBoundedLog(path, 64)
	if err != nil {
		t.Fatal(err)
	}
	if err := w.file.Close(); err != nil {
		t.Fatal(err)
	}
	payload := []byte("must be reported as consumed")
	if n, err := w.Write(payload); err != nil || n != len(payload) {
		t.Fatalf("write after sink failure = (%d, %v), want (%d, nil)", n, err, len(payload))
	}
}

func TestProcessArgsSeccompDefault(t *testing.T) {
	got := processArgs("/run/fc.sock", "vm-1", false)
	for _, arg := range got {
		if arg == "--no-seccomp" {
			t.Fatalf("secure default unexpectedly contains --no-seccomp: %v", got)
		}
	}
}

func TestProcessArgsDevelopmentEscapeHatch(t *testing.T) {
	got := processArgs("/run/fc.sock", "vm-1", true)
	if got[len(got)-1] != "--no-seccomp" {
		t.Fatalf("args = %v, want explicit --no-seccomp suffix", got)
	}
}

func TestVMMLogExpectedExitIsRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firecracker-expected.log")
	log, err := openVMMLog(path, 64, time.Hour, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("diagnostic")); err != nil {
		t.Fatal(err)
	}
	log.markExpectedExit()
	log.finishExit(errors.New("signal: terminated"))
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("expected-exit log still exists: %v", err)
	}
}

func TestVMMLogUnexpectedExitIsRetained(t *testing.T) {
	path := filepath.Join(t.TempDir(), "firecracker-crash.log")
	log, err := openVMMLog(path, 64, time.Hour, 8)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := log.Write([]byte("crash diagnostic")); err != nil {
		t.Fatal(err)
	}
	log.finishExit(errors.New("unexpected exit"))
	if got, err := os.ReadFile(path); err != nil {
		t.Fatal(err)
	} else if string(got) != "crash diagnostic" {
		t.Fatalf("retained log = %q", got)
	}
}

func TestPruneVMMLogsBoundsAgeCountAndPermissions(t *testing.T) {
	dir := t.TempDir()
	now := time.Now()
	for i := 0; i < 4; i++ {
		path := filepath.Join(dir, "firecracker-"+string(rune('a'+i))+".log")
		if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
		mtime := now.Add(-time.Duration(i) * time.Hour)
		if err := os.Chtimes(path, mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	unrelated := filepath.Join(dir, "service.log")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := pruneVMMLogs(dir, 90*time.Minute, 2, now); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"firecracker-a.log", "firecracker-b.log"} {
		info, err := os.Stat(filepath.Join(dir, name))
		if err != nil {
			t.Fatalf("%s missing: %v", name, err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("%s mode = %o, want 600", name, info.Mode().Perm())
		}
	}
	for _, name := range []string{"firecracker-c.log", "firecracker-d.log"} {
		if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
			t.Fatalf("%s was not pruned: %v", name, err)
		}
	}
	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated log was touched: %v", err)
	}
}

func TestPruneVMMLogsDoesNotTouchActiveLog(t *testing.T) {
	dir := t.TempDir()
	activePath := filepath.Join(dir, "firecracker-active.log")
	log, err := openVMMLog(activePath, 64, time.Nanosecond, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer log.Close()
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(activePath, old, old); err != nil {
		t.Fatal(err)
	}
	if err := pruneVMMLogs(dir, time.Nanosecond, 1, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(activePath); err != nil {
		t.Fatalf("active log was pruned: %v", err)
	}
}
