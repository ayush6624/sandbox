package registry

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/google/uuid"
)

func readerTestRegistry(t *testing.T) *Registry {
	t.Helper()
	r, err := Open(filepath.Join(t.TempDir(), "registry.db"), Pools{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	return r
}

func readerTestIdentity(t *testing.T, owner string) chunkstore.ReaderIdentity {
	t.Helper()
	reader, err := chunkstore.New(nil).PrepareReader("11111111-1111-4111-8111-111111111111", "reader-test-root", owner)
	if err != nil {
		t.Fatal(err)
	}
	return reader.Identity()
}

// This child retains its lock after closing SQLite, then exits without cleanup.
func TestChunkReaderProcess(t *testing.T) {
	path := os.Getenv("CHUNK_READER_CHILD_DB")
	if path == "" {
		return
	}
	r, err := Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	owner, err := r.ChunkReaderOwner(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	identity := readerTestIdentity(t, owner)
	if err := r.QueueChunkReader(context.Background(), identity); err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	if err := json.NewEncoder(os.Stdout).Encode(identity); err != nil {
		t.Fatal(err)
	}
	_, _ = bufio.NewReader(os.Stdin).ReadString('\n')
	os.Exit(0)
}

func startReaderProcess(t *testing.T, path string) (chunkstore.ReaderIdentity, func()) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestChunkReaderProcess$")
	cmd.Env = append(os.Environ(), "CHUNK_READER_CHILD_DB="+path)
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
			t.Fatalf("reader child: %v: %s", err, stderr.String())
		}
	}
	t.Cleanup(stop)
	var identity chunkstore.ReaderIdentity
	if err := json.NewDecoder(stdout).Decode(&identity); err != nil {
		t.Fatalf("reader child identity: %v", err)
	}
	return identity, stop
}

func TestChunkReaderProcessDeathAndRegistryClose(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	identity, stop := startReaderProcess(t, path)
	r, err := Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if dead, err := r.ConfirmChunkReaderOwnerDead(ctx, identity.OwnerID); err != nil || dead {
		t.Fatalf("live child with closed registry: dead=%v err=%v", dead, err)
	}
	rows, err := r.ListChunkReaders(ctx, "", 32)
	if err != nil || len(rows) != 1 || rows[0] != identity {
		t.Fatalf("child durable row: %v %v", rows, err)
	}
	stop()
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if dead, err := r.ConfirmChunkReaderOwnerDead(ctx, identity.OwnerID); err != nil || !dead {
		t.Fatalf("dead child: dead=%v err=%v", dead, err)
	}
	dir, _ := r.readerLockDir()
	if _, err := os.Stat(filepath.Join(dir, identity.OwnerID)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("dead lock retained: %v", err)
	}
	// The receipt survives reopening and removing the lock, even with debt left.
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if dead, err := r.ConfirmChunkReaderOwnerDead(ctx, identity.OwnerID); err != nil || !dead {
		t.Fatalf("receipt replay: dead=%v err=%v", dead, err)
	}
	if _, err := r.PruneChunkReaderOwners(ctx, "", 32); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM chunk_reader_owners`).Scan(&count); err != nil || count != 1 {
		t.Fatalf("pruned owner with debt: %d %v", count, err)
	}
	if err := r.RemoveChunkReader(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if _, err := r.PruneChunkReaderOwners(ctx, "", 32); err != nil {
		t.Fatal(err)
	}
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM chunk_reader_owners`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("empty owner retained: %d %v", count, err)
	}
}

func TestChunkReaderOwnerSurvivesLocalReopen(t *testing.T) {
	ctx := context.Background()
	r := readerTestRegistry(t)
	first, err := r.ChunkReaderOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(r.path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	second, err := reopened.ChunkReaderOwner(ctx)
	if err != nil || first != second {
		t.Fatalf("owner changed: %s %s %v", first, second, err)
	}
	if dead, err := reopened.ConfirmChunkReaderOwnerDead(ctx, first); err != nil || dead {
		t.Fatalf("live owner: %v %v", dead, err)
	}
}

func TestChunkReaderMissingReplacedAndCopiedLocks(t *testing.T) {
	for _, mode := range []string{"missing", "replaced", "copied"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			source := filepath.Join(t.TempDir(), "registry.db")
			identity, stop := startReaderProcess(t, source)
			stop()
			r, err := Open(source, Pools{})
			if err != nil {
				t.Fatal(err)
			}
			defer r.Close()
			dir, _ := r.readerLockDir()
			lockPath := filepath.Join(dir, identity.OwnerID)
			if mode == "copied" {
				if err := r.Close(); err != nil {
					t.Fatal(err)
				}
				dest := filepath.Join(t.TempDir(), "registry.db")
				data, err := os.ReadFile(source)
				if err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(dest, data, 0600); err != nil {
					t.Fatal(err)
				}
				r, err = Open(dest, Pools{})
				if err != nil {
					t.Fatal(err)
				}
				defer r.Close()
				dir, _ = r.readerLockDir()
				if err := os.MkdirAll(dir, 0700); err != nil {
					t.Fatal(err)
				}
				lockPath = filepath.Join(dir, identity.OwnerID)
			} else {
				// Rename preserves the old inode so replacement cannot reuse it.
				if err := os.Rename(lockPath, lockPath+".original"); err != nil {
					t.Fatal(err)
				}
			}
			if mode != "missing" {
				if err := os.WriteFile(lockPath, []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			if dead, err := r.ConfirmChunkReaderOwnerDead(ctx, identity.OwnerID); dead || !errors.Is(err, ErrChunkReaderOwnerUnknown) {
				t.Fatalf("%s accepted: dead=%v err=%v", mode, dead, err)
			}
			var dead bool
			if err := r.db.QueryRow(`SELECT dead FROM chunk_reader_owners WHERE owner_id=?`, identity.OwnerID).Scan(&dead); err != nil || dead {
				t.Fatalf("false receipt: %v %v", dead, err)
			}
		})
	}
}

func TestChunkReaderJournalIdentityRollbackAndPaging(t *testing.T) {
	ctx := context.Background()
	r := readerTestRegistry(t)
	owner, err := r.ChunkReaderOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	identity := readerTestIdentity(t, owner)
	if err := r.QueueChunkReader(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if err := r.QueueChunkReader(ctx, identity); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"set", "root", "owner"} {
		other := identity
		switch field {
		case "set":
			other.SetID = uuid.NewString()
		case "root":
			other.RootID = "other-root"
		case "owner":
			other.OwnerID = uuid.NewString()
		}
		if err := r.QueueChunkReader(ctx, other); !errors.Is(err, ErrChunkReaderConflict) {
			t.Fatalf("queue %s conflict: %v", field, err)
		}
		if err := r.RemoveChunkReader(ctx, other); !errors.Is(err, ErrChunkReaderConflict) {
			t.Fatalf("remove %s conflict: %v", field, err)
		}
	}
	if _, err := r.db.Exec(`CREATE TRIGGER reject_reader_insert BEFORE INSERT ON chunk_readers BEGIN SELECT RAISE(ABORT,'injected insertion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.QueueChunkReader(ctx, readerTestIdentity(t, owner)); err == nil {
		t.Fatal("injected insertion succeeded")
	}
	rows, err := r.ListChunkReaders(ctx, "", 1)
	if err != nil || len(rows) != 1 || rows[0] != identity {
		t.Fatalf("rollback rows: %v %v", rows, err)
	}
	rows, err = r.ListChunkReaders(ctx, identity.ReaderID, 1)
	if err != nil || len(rows) != 0 {
		t.Fatalf("cursor rows: %v %v", rows, err)
	}
	for _, limit := range []int{0, 1001} {
		if _, err := r.ListChunkReaders(ctx, "", limit); err == nil {
			t.Fatal("invalid limit accepted")
		}
	}
	if _, err := r.db.Exec(`CREATE TRIGGER reject_reader_delete BEFORE DELETE ON chunk_readers BEGIN SELECT RAISE(ABORT,'injected deletion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveChunkReader(ctx, identity); err == nil {
		t.Fatal("injected deletion succeeded")
	}
	rows, err = r.ListChunkReaders(ctx, "", 1)
	if err != nil || len(rows) != 1 {
		t.Fatalf("failed deletion lost debt: %v %v", rows, err)
	}
	if _, err := r.db.Exec(`DROP TRIGGER reject_reader_delete`); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveChunkReader(ctx, identity); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveChunkReader(ctx, identity); err != nil {
		t.Fatal(err)
	}
}

func TestChunkReaderAdmissionLimits(t *testing.T) {
	ctx := context.Background()
	t.Run("readers", func(t *testing.T) {
		r := readerTestRegistry(t)
		owner, err := r.ChunkReaderOwner(ctx)
		if err != nil {
			t.Fatal(err)
		}
		_, err = r.db.Exec(`WITH RECURSIVE numbers(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n<?) INSERT INTO chunk_readers(reader_id,set_id,root_id,owner_id) SELECT printf('00000000-0000-4000-8000-%012d',n),'11111111-1111-4111-8111-111111111111','reader-test-root',? FROM numbers`, MaxChunkReaders, owner)
		if err != nil {
			t.Fatal(err)
		}
		if err := r.QueueChunkReader(ctx, readerTestIdentity(t, owner)); !errors.Is(err, ErrChunkReaderCapacity) {
			t.Fatalf("reader limit: %v", err)
		}
		existing := chunkstore.ReaderIdentity{SetID: "11111111-1111-4111-8111-111111111111", RootID: "reader-test-root", OwnerID: owner, ReaderID: "00000000-0000-4000-8000-000000000001"}
		if err := r.QueueChunkReader(ctx, existing); err != nil {
			t.Fatalf("replay at capacity: %v", err)
		}
		if err := r.RemoveChunkReader(ctx, existing); err != nil {
			t.Fatal(err)
		}
		if err := r.QueueChunkReader(ctx, readerTestIdentity(t, owner)); err != nil {
			t.Fatalf("admission after cleanup: %v", err)
		}
	})
	t.Run("owners", func(t *testing.T) {
		r := readerTestRegistry(t)
		_, err := r.db.Exec(`WITH RECURSIVE numbers(n) AS (SELECT 1 UNION ALL SELECT n+1 FROM numbers WHERE n<?) INSERT INTO chunk_reader_owners(owner_id,device,inode) SELECT printf('00000000-0000-4000-8000-%012d',n),'0','0' FROM numbers`, MaxChunkReaderOwners)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := r.ChunkReaderOwner(ctx); !errors.Is(err, ErrChunkReaderCapacity) {
			t.Fatalf("owner limit: %v", err)
		}
		next, err := r.PruneChunkReaderOwners(ctx, "", 32)
		if err == nil || next != "00000000-0000-4000-8000-000000000032" {
			t.Fatalf("pruning failed to advance over errors: %q %v", next, err)
		}
		next, err = r.PruneChunkReaderOwners(ctx, next, 32)
		if err == nil || next != fmt.Sprintf("00000000-0000-4000-8000-%012d", 64) {
			t.Fatalf("second prune page: %q %v", next, err)
		}
	})
}

func TestChunkReaderRegistrationRollback(t *testing.T) {
	ctx := context.Background()
	r := readerTestRegistry(t)
	if _, err := r.db.Exec(`CREATE TRIGGER reject_reader_owner BEFORE INSERT ON chunk_reader_owners BEGIN SELECT RAISE(ABORT,'injected registration failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err := r.ChunkReaderOwner(ctx); err == nil {
		t.Fatal("registration succeeded")
	}
	dir, _ := r.readerLockDir()
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 0 {
		t.Fatalf("failed registration files: %v %v", files, err)
	}
	if _, err := r.db.Exec(`DROP TRIGGER reject_reader_owner`); err != nil {
		t.Fatal(err)
	}
	owner, err := r.ChunkReaderOwner(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// Model a failed COMMIT: the descriptor remains protected, but its row did
	// not commit. The next writer can prove absence and register this same lock.
	if _, err := r.db.Exec(`DELETE FROM chunk_reader_owners WHERE owner_id=?`, owner); err != nil {
		t.Fatal(err)
	}
	processReaderOwners.Lock()
	for key, lock := range processReaderOwners.locks {
		if lock.id == owner {
			lock.registered = false
			processReaderOwners.locks[key] = lock
		}
	}
	processReaderOwners.Unlock()
	retried, err := r.ChunkReaderOwner(ctx)
	if err != nil || retried != owner {
		t.Fatalf("registration retry: %s %v", retried, err)
	}
	if dead, err := r.ConfirmChunkReaderOwnerDead(ctx, owner); err != nil || dead {
		t.Fatalf("retried lock lost: %v %v", dead, err)
	}
}

func TestChunkReaderDeadReceiptAndCleanupRetry(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	identity, stop := startReaderProcess(t, path)
	stop()
	r, err := Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.db.Exec(`CREATE TRIGGER reject_dead_receipt BEFORE UPDATE OF dead ON chunk_reader_owners BEGIN SELECT RAISE(ABORT,'injected receipt failure'); END`); err != nil {
		t.Fatal(err)
	}
	if dead, err := r.ConfirmChunkReaderOwnerDead(ctx, identity.OwnerID); err == nil || dead {
		t.Fatalf("failed receipt accepted: %v %v", dead, err)
	}
	dir, _ := r.readerLockDir()
	lockPath := filepath.Join(dir, identity.OwnerID)
	if _, err := os.Stat(lockPath); err != nil {
		t.Fatalf("uncommitted receipt unlinked file: %v", err)
	}
	if _, err := r.db.Exec(`DROP TRIGGER reject_dead_receipt`); err != nil {
		t.Fatal(err)
	}
	// Model a crash after the actual child's death and receipt commit, before
	// unlink. Replay must clean the file even while reader debt remains.
	if _, err := r.db.Exec(`UPDATE chunk_reader_owners SET dead=1 WHERE owner_id=?`, identity.OwnerID); err != nil {
		t.Fatal(err)
	}
	if err := r.QueueChunkReader(ctx, identity); !errors.Is(err, ErrChunkReaderOwnerUnknown) {
		t.Fatalf("replayed acquisition for dead owner: %v", err)
	}
	if dead, err := r.ConfirmChunkReaderOwnerDead(ctx, identity.OwnerID); err != nil || !dead {
		t.Fatalf("receipt cleanup replay: %v %v", dead, err)
	}
	if _, err := os.Stat(lockPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("receipt file remained: %v", err)
	}
}

func TestChunkReaderDeadReceiptPreservesReplacement(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	identity, stop := startReaderProcess(t, path)
	stop()
	r, err := Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	if _, err := r.db.Exec(`UPDATE chunk_reader_owners SET dead=1 WHERE owner_id=?`, identity.OwnerID); err != nil {
		t.Fatal(err)
	}
	dir, _ := r.readerLockDir()
	lockPath := filepath.Join(dir, identity.OwnerID)
	if err := os.Rename(lockPath, lockPath+".original"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(lockPath, []byte("keep"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := r.RemoveChunkReader(ctx, identity); err != nil {
		t.Fatal(err)
	}
	next, err := r.PruneChunkReaderOwners(ctx, "", 32)
	if err == nil || next != identity.OwnerID {
		t.Fatalf("prune replacement: %q %v", next, err)
	}
	got, err := os.ReadFile(lockPath)
	if err != nil || string(got) != "keep" {
		t.Fatalf("replacement removed: %q %v", got, err)
	}
	var count int
	if err := r.db.QueryRow(`SELECT COUNT(*) FROM chunk_reader_owners WHERE owner_id=?`, identity.OwnerID).Scan(&count); err != nil || count != 1 {
		t.Fatalf("cleanup failure lost receipt: %d %v", count, err)
	}
}

func TestChunkReaderOrphanFileBound(t *testing.T) {
	ctx := context.Background()
	r := readerTestRegistry(t)
	dir, _ := r.readerLockDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	orphan := filepath.Join(dir, uuid.NewString())
	if err := os.WriteFile(orphan, nil, 0600); err != nil {
		t.Fatal(err)
	}
	// A bounded startup pass reclaims an interrupted registration.
	if _, err := r.ChunkReaderOwner(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(orphan); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan remained: %v", err)
	}
	oversized := readerTestRegistry(t)
	dir, _ = oversized.readerLockDir()
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < MaxChunkReaderOwners+2; i++ {
		if err := os.WriteFile(filepath.Join(dir, fmt.Sprint(i)), nil, 0600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := oversized.ChunkReaderOwner(ctx); !errors.Is(err, ErrChunkReaderCapacity) {
		t.Fatalf("oversized directory admission: %v", err)
	}
}
