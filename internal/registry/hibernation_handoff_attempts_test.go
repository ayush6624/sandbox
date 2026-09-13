package registry

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func insertAttemptJob(t *testing.T, r *Registry, g string) {
	t.Helper()
	if _, err := r.db.Exec(`INSERT INTO hibernation_handoffs(generation,sandbox_id,offer,expected_revision) VALUES(?,?,?,0)`, g, "sandbox", []byte(`{}`)); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffAttemptLifetime(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	insertAttemptJob(t, r, "generation")
	peer, err := Open(r.path, r.pools)
	if err != nil {
		t.Fatal(err)
	}
	defer peer.Close()
	var escaped HandoffAttempt
	sentinel := errors.New("callback failed")
	err = r.WithHibernationHandoff(ctx, "generation", func(a *HandoffAttempt) error {
		escaped = *a
		if err := peer.WithHibernationHandoff(ctx, "generation", func(*HandoffAttempt) error { t.Fatal("contending callback ran"); return nil }); !errors.Is(err, ErrHandoffBusy) {
			t.Fatalf("contention: %v", err)
		}
		if err := r.AckHibernationHandoffCache(ctx, "generation"); err != nil {
			t.Fatal(err)
		}
		job, err := a.Job(ctx)
		if err != nil || !job.CacheReady {
			t.Fatalf("stale flags: %+v %v", job, err)
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		if err := peer.WithHibernationHandoff(ctx, "generation", func(*HandoffAttempt) error { t.Fatal("Close revoked attempt"); return nil }); !errors.Is(err, ErrHandoffBusy) {
			t.Fatalf("after Close: %v", err)
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatal(err)
	}
	if _, err := escaped.Job(ctx); !errors.Is(err, ErrHandoffAttemptClosed) {
		t.Fatalf("escaped read: %v", err)
	}
	if err := escaped.CompleteBackup(ctx); !errors.Is(err, ErrHandoffAttemptClosed) {
		t.Fatalf("escaped completion: %v", err)
	}
	if err := escaped.Remove(ctx); !errors.Is(err, ErrHandoffAttemptClosed) {
		t.Fatalf("escaped removal: %v", err)
	}
	if err := peer.WithHibernationHandoff(ctx, "generation", func(a *HandoffAttempt) error { return a.CompleteBackup(ctx) }); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffAttemptStripeFilesAreBounded(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	generations := make(map[int]string)
	for n := 0; len(generations) < handoffLockStripes; n++ {
		g := fmt.Sprint("generation-", n)
		generations[handoffLockStripe(g)] = g
	}
	for _, g := range generations {
		insertAttemptJob(t, r, g)
		if err := r.WithHibernationHandoff(ctx, g, func(a *HandoffAttempt) error {
			if err := a.CompleteBackup(ctx); err != nil {
				return err
			}
			return a.Remove(ctx)
		}); err != nil {
			t.Fatal(err)
		}
	}
	entries, err := os.ReadDir(r.path + ".handoff-locks")
	if err != nil || len(entries) != 64 {
		t.Fatalf("files=%d: %v", len(entries), err)
	}
	var count int
	if err := r.rdb.QueryRow(`SELECT COUNT(*) FROM hibernation_handoff_locks`).Scan(&count); err != nil || count != 64 {
		t.Fatalf("rows=%d: %v", count, err)
	}
	g := "collision-original"
	other := ""
	for n := 0; other == ""; n++ {
		candidate := fmt.Sprint("collision-", n)
		if handoffLockStripe(candidate) == handoffLockStripe(g) {
			other = candidate
		}
	}
	insertAttemptJob(t, r, g)
	insertAttemptJob(t, r, other)
	if err := r.WithHibernationHandoff(ctx, g, func(*HandoffAttempt) error {
		err := r.WithHibernationHandoff(ctx, other, func(*HandoffAttempt) error { t.Fatal("stripe collision admitted"); return nil })
		if !errors.Is(err, ErrHandoffBusy) {
			t.Fatalf("collision: %v", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffAttemptRejectsChangedFiles(t *testing.T) {
	for _, change := range []string{"missing", "replaced", "symlink", "directory"} {
		t.Run(change, func(t *testing.T) {
			r, ctx := testRegistry(t), context.Background()
			g := "generation"
			insertAttemptJob(t, r, g)
			if err := r.WithHibernationHandoff(ctx, g, func(*HandoffAttempt) error { return nil }); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(r.path+".handoff-locks", fmt.Sprintf("%02d.lock", handoffLockStripe(g)))
			// Keep the original inode allocated so replacement cannot reuse its number.
			old, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer old.Close()
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch change {
			case "replaced":
				err = os.WriteFile(path, nil, 0600)
			case "symlink":
				err = os.Symlink(r.path, path)
			case "directory":
				err = os.Mkdir(path, 0700)
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := r.WithHibernationHandoff(ctx, g, func(*HandoffAttempt) error { t.Fatal("changed lock admitted"); return nil }); err == nil {
				t.Fatal("accepted changed file")
			}
			if change == "missing" {
				if _, err := os.Lstat(path); !os.IsNotExist(err) {
					t.Fatalf("missing lock recreated: %v", err)
				}
			}
		})
	}
}

func TestHandoffAttemptProcess(t *testing.T) {
	path := os.Getenv("HANDOFF_ATTEMPT_CHILD_DB")
	if path == "" {
		return
	}
	r, err := Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	err = r.WithHibernationHandoff(context.Background(), "generation", func(a *HandoffAttempt) error {
		if os.Getenv("HANDOFF_ATTEMPT_COMPLETE") == "1" {
			if err := a.CompleteBackup(context.Background()); err != nil {
				t.Fatal(err)
			}
		}
		if err := r.Close(); err != nil {
			t.Fatal(err)
		}
		fmt.Println("locked after Close")
		_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
		os.Exit(0) // Simulate death without returning callback or closing its descriptor.
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestHandoffAttemptProcessDeathAndReplay(t *testing.T) {
	for _, complete := range []bool{false, true} {
		t.Run(fmt.Sprint(complete), func(t *testing.T) {
			r, ctx := testRegistry(t), context.Background()
			insertAttemptJob(t, r, "generation")
			cmd := exec.Command(os.Args[0], "-test.run=^TestHandoffAttemptProcess$")
			cmd.Env = append(os.Environ(), "HANDOFF_ATTEMPT_CHILD_DB="+r.path)
			if complete {
				cmd.Env = append(cmd.Env, "HANDOFF_ATTEMPT_COMPLETE=1")
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr strings.Builder
			cmd.Stderr = &stderr
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			stopped := false
			stop := func() {
				if stopped {
					return
				}
				stopped = true
				_ = stdin.Close()
				if err := cmd.Wait(); err != nil {
					t.Fatalf("child: %v %s", err, stderr.String())
				}
			}
			t.Cleanup(stop)
			line, err := bufio.NewReader(stdout).ReadString('\n')
			if err != nil || line != "locked after Close\n" {
				t.Fatalf("child ready: %q %v %s", line, err, stderr.String())
			}
			if err := r.WithHibernationHandoff(ctx, "generation", func(*HandoffAttempt) error { t.Fatal("live child ignored"); return nil }); !errors.Is(err, ErrHandoffBusy) {
				t.Fatalf("live child: %v", err)
			}
			stop()
			if err := r.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(r.path, r.pools)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			if err := reopened.WithHibernationHandoff(ctx, "generation", func(a *HandoffAttempt) error {
				job, err := a.Job(ctx)
				if err != nil {
					return err
				}
				if job.BackupComplete != complete {
					t.Fatalf("completion lost: %+v", job)
				}
				if !complete {
					return a.CompleteBackup(ctx)
				}
				return a.Remove(ctx)
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestHandoffAttemptCancellationAndPanic(t *testing.T) {
	r := testRegistry(t)
	insertAttemptJob(t, r, "generation")
	ctx, cancel := context.WithCancel(context.Background())
	var escaped *HandoffAttempt
	func() {
		defer func() {
			if recover() != "callback panic" {
				t.Fatal("callback panic not propagated")
			}
		}()
		_ = r.WithHibernationHandoff(ctx, "generation", func(a *HandoffAttempt) error {
			escaped = a
			cancel()
			if err := r.WithHibernationHandoff(context.Background(), "generation", func(*HandoffAttempt) error { t.Fatal("cancellation unlocked callback"); return nil }); !errors.Is(err, ErrHandoffBusy) {
				t.Fatal(err)
			}
			panic("callback panic")
		})
	}()
	if err := escaped.CompleteBackup(context.Background()); !errors.Is(err, ErrHandoffAttemptClosed) {
		t.Fatal(err)
	}
	if err := r.WithHibernationHandoff(context.Background(), "generation", func(*HandoffAttempt) error { return nil }); err != nil {
		t.Fatal(err)
	}
}

func TestHandoffAttemptResumesUnregisteredFile(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	insertAttemptJob(t, r, "generation")
	dir := r.path + ".handoff-locks"
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, fmt.Sprintf("%02d.lock", handoffLockStripe("generation")))
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.WithHibernationHandoff(ctx, "generation", func(*HandoffAttempt) error { return nil }); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Fatal("initialization replaced existing file")
	}
	var synchronous int
	if err := r.db.QueryRow(`PRAGMA synchronous`).Scan(&synchronous); err != nil || synchronous != 2 {
		t.Fatalf("synchronous=%d: %v", synchronous, err)
	}
	if err := r.WithHibernationHandoff(ctx, "absent", func(*HandoffAttempt) error { t.Fatal("absent callback"); return nil }); err != nil {
		t.Fatal(err)
	}
	for _, g := range []string{"", strings.Repeat("x", 1025), string([]byte{255})} {
		if err := r.WithHibernationHandoff(ctx, g, func(*HandoffAttempt) error { t.Fatal("invalid callback"); return nil }); err == nil {
			t.Fatal("invalid generation accepted")
		}
	}
}
