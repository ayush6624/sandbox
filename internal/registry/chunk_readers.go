package registry

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/google/uuid"
	"golang.org/x/sys/unix"
)

const (
	MaxChunkReaders      = 4096
	MaxChunkReaderOwners = 256
)

var (
	ErrChunkReaderCapacity     = errors.New("chunk reader journal capacity exceeded")
	ErrChunkReaderConflict     = errors.New("chunk reader identity conflict")
	ErrChunkReaderOwnerUnknown = errors.New("chunk reader owner death is unproven")
)

type readerOwnerLock struct {
	id         string
	file       *os.File
	registered bool
}

// These descriptors deliberately outlive Registry.Close and Server.Serve.
// Only process exit releases a live incarnation's lock.
var processReaderOwners = struct {
	sync.Mutex
	locks map[string]readerOwnerLock
}{locks: make(map[string]readerOwnerLock)}

func (r *Registry) migrateChunkReaders() error {
	_, err := r.db.Exec(`
 CREATE TABLE IF NOT EXISTS chunk_reader_registry (singleton INTEGER PRIMARY KEY CHECK(singleton=1), identity TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS chunk_reader_owners (
 owner_id TEXT PRIMARY KEY, device TEXT NOT NULL, inode TEXT NOT NULL,
 dead INTEGER NOT NULL DEFAULT 0 CHECK(dead IN (0,1)));
 CREATE TABLE IF NOT EXISTS chunk_readers (
 reader_id TEXT PRIMARY KEY, set_id TEXT NOT NULL, root_id TEXT NOT NULL,
 owner_id TEXT NOT NULL REFERENCES chunk_reader_owners(owner_id));
 CREATE INDEX IF NOT EXISTS chunk_readers_owner ON chunk_readers(owner_id);
 INSERT OR IGNORE INTO chunk_reader_registry(singleton,identity) VALUES(1, ?);`, uuid.NewString())
	return err
}

func (r *Registry) readerWriteTx(ctx context.Context) (*sql.Tx, error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	// Obtain SQLite's writer reservation before reading admission/lifecycle state.
	if _, err = tx.ExecContext(ctx, `UPDATE chunk_reader_registry SET identity=identity WHERE singleton=1`); err != nil {
		_ = tx.Rollback()
		return nil, err
	}
	return tx, nil
}

func (r *Registry) readerLockDir() (string, error) {
	path, err := filepath.Abs(r.path)
	if err != nil {
		return "", err
	}
	return path + ".chunk-readers", nil
}

func readerFileIdentity(file *os.File) (string, string, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return "", "", err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG {
		return "", "", ErrChunkReaderOwnerUnknown
	}
	return fmt.Sprint(stat.Dev), fmt.Sprint(stat.Ino), nil
}

func syncReaderDirectory(path string) error {
	dir, err := os.Open(path)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func openReaderLock(path string) (*os.File, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	return os.NewFile(uintptr(fd), path), nil
}

func matchReaderFile(path, device, inode string) error {
	file, err := openReaderLock(path)
	if err != nil {
		return err
	}
	defer file.Close()
	gotDevice, gotInode, err := readerFileIdentity(file)
	if err != nil {
		return err
	}
	if gotDevice != device || gotInode != inode {
		return ErrChunkReaderOwnerUnknown
	}
	return nil
}

// ChunkReaderOwner registers before any reader acquisition can be journaled.
func (r *Registry) ChunkReaderOwner(ctx context.Context) (string, error) {
	processReaderOwners.Lock()
	defer processReaderOwners.Unlock()
	path, err := filepath.Abs(r.path)
	if err != nil {
		return "", err
	}
	dbFile, err := os.Open(path)
	if err != nil {
		return "", err
	}
	device, inode, err := readerFileIdentity(dbFile)
	_ = dbFile.Close()
	if err != nil {
		return "", err
	}
	tx, err := r.readerWriteTx(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	var registryID string
	if err := tx.QueryRowContext(ctx, `SELECT identity FROM chunk_reader_registry WHERE singleton=1`).Scan(&registryID); err != nil {
		return "", err
	}
	key := fmt.Sprintf("%s:%s:%s:%s", path, device, inode, registryID)
	if owner, ok := processReaderOwners.locks[key]; ok {
		var dead bool
		err := tx.QueryRowContext(ctx, `SELECT dead FROM chunk_reader_owners WHERE owner_id=?`, owner.id).Scan(&dead)
		if errors.Is(err, sql.ErrNoRows) && !owner.registered {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunk_reader_owners`).Scan(&count); err != nil {
				return "", err
			}
			if count >= MaxChunkReaderOwners {
				return "", ErrChunkReaderCapacity
			}
			device, inode, err := readerFileIdentity(owner.file)
			if err != nil {
				return "", err
			}
			if err := matchReaderFile(owner.file.Name(), device, inode); err != nil {
				return "", err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_reader_owners(owner_id,device,inode) VALUES(?,?,?)`, owner.id, device, inode); err != nil {
				return "", err
			}
			if err := tx.Commit(); err != nil {
				return "", err
			}
		} else if err != nil {
			return "", err
		}
		if dead {
			return "", ErrChunkReaderOwnerUnknown
		}
		owner.registered = true
		processReaderOwners.locks[key] = owner
		return owner.id, nil
	}
	dir, err := r.readerLockDir()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	if err := syncReaderDirectory(filepath.Dir(dir)); err != nil {
		return "", err
	}
	if err := removeUnregisteredReaderLocks(ctx, tx, dir); err != nil {
		return "", err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunk_reader_owners`).Scan(&count); err != nil {
		return "", err
	}
	if count >= MaxChunkReaderOwners {
		return "", ErrChunkReaderCapacity
	}
	id := uuid.NewString()
	lockPath := filepath.Join(dir, id)
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err != nil {
		return "", err
	}
	registered := false
	defer func() {
		if !registered {
			_ = file.Close()
			_ = os.Remove(lockPath)
			_ = syncReaderDirectory(dir)
		}
	}()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return "", err
	}
	device, inode, err = readerFileIdentity(file)
	if err != nil {
		return "", err
	}
	if err := file.Sync(); err != nil {
		return "", err
	}
	if err := syncReaderDirectory(dir); err != nil {
		return "", err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_reader_owners(owner_id,device,inode) VALUES(?,?,?)`, id, device, inode); err != nil {
		return "", err
	}
	// Retain the descriptor even if COMMIT reports an ambiguous failure. A
	// persisted registration must never look dead while this process can run.
	processReaderOwners.locks[key] = readerOwnerLock{id: id, file: file}
	registered = true
	if err := tx.Commit(); err != nil {
		return "", err
	}
	processReaderOwners.locks[key] = readerOwnerLock{id: id, file: file, registered: true}
	return id, nil
}

func removeUnregisteredReaderLocks(ctx context.Context, tx *sql.Tx, dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return err
	}
	// Registration holds the SQLite writer reservation. Bound inspection and
	// reserve room for its one new file, including busy unregistered locks
	// retained by processes whose registration COMMIT returned an error.
	entries, err := directory.ReadDir(MaxChunkReaderOwners + 2)
	_ = directory.Close()
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(entries) > MaxChunkReaderOwners+1 {
		return ErrChunkReaderCapacity
	}
	remaining := len(entries)
	for _, entry := range entries {
		id, err := uuid.Parse(entry.Name())
		if err != nil || id.String() != entry.Name() || id == uuid.Nil {
			continue
		}
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunk_reader_owners WHERE owner_id=?`, entry.Name()).Scan(&count); err != nil {
			return err
		}
		if count != 0 {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := openReaderLock(path)
		if err != nil {
			return err
		}
		err = unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if errors.Is(err, unix.EWOULDBLOCK) {
			_ = file.Close()
			continue
		}
		if err == nil {
			device, inode, identityErr := readerFileIdentity(file)
			err = identityErr
			if err == nil {
				err = matchReaderFile(path, device, inode)
			}
			if err == nil {
				err = os.Remove(path)
			}
		}
		_ = file.Close()
		if err != nil {
			return err
		}
		remaining--
	}
	if err := syncReaderDirectory(dir); err != nil {
		return err
	}
	if remaining >= MaxChunkReaderOwners+1 {
		return ErrChunkReaderCapacity
	}
	return nil
}

func validateReaderIdentity(identity chunkstore.ReaderIdentity) error {
	_, err := chunkstore.New(nil).RecoverReader(identity)
	return err
}

func (r *Registry) QueueChunkReader(ctx context.Context, identity chunkstore.ReaderIdentity) error {
	if err := validateReaderIdentity(identity); err != nil {
		return err
	}
	tx, err := r.readerWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing chunkstore.ReaderIdentity
	err = tx.QueryRowContext(ctx, `SELECT reader_id,set_id,root_id,owner_id FROM chunk_readers WHERE reader_id=?`, identity.ReaderID).Scan(&existing.ReaderID, &existing.SetID, &existing.RootID, &existing.OwnerID)
	exists := err == nil
	if exists && existing != identity {
		return ErrChunkReaderConflict
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var dead bool
	if err := tx.QueryRowContext(ctx, `SELECT dead FROM chunk_reader_owners WHERE owner_id=?`, identity.OwnerID).Scan(&dead); err != nil {
		return fmt.Errorf("%w: %v", ErrChunkReaderOwnerUnknown, err)
	}
	if dead {
		return ErrChunkReaderOwnerUnknown
	}
	if exists {
		return tx.Commit()
	}
	var count int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM chunk_readers`).Scan(&count); err != nil {
		return err
	}
	if count >= MaxChunkReaders {
		return ErrChunkReaderCapacity
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO chunk_readers(reader_id,set_id,root_id,owner_id) VALUES(?,?,?,?)`, identity.ReaderID, identity.SetID, identity.RootID, identity.OwnerID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) RemoveChunkReader(ctx context.Context, identity chunkstore.ReaderIdentity) error {
	if err := validateReaderIdentity(identity); err != nil {
		return err
	}
	tx, err := r.readerWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing chunkstore.ReaderIdentity
	err = tx.QueryRowContext(ctx, `SELECT reader_id,set_id,root_id,owner_id FROM chunk_readers WHERE reader_id=?`, identity.ReaderID).Scan(&existing.ReaderID, &existing.SetID, &existing.RootID, &existing.OwnerID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if existing != identity {
		return ErrChunkReaderConflict
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_readers WHERE reader_id=?`, identity.ReaderID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) ListChunkReaders(ctx context.Context, afterReaderID string, limit int) ([]chunkstore.ReaderIdentity, error) {
	if limit < 1 || limit > 1000 {
		return nil, fmt.Errorf("chunk reader page limit must be 1..1000")
	}
	rows, err := r.rdb.QueryContext(ctx, `SELECT reader_id,set_id,root_id,owner_id FROM chunk_readers WHERE reader_id>? ORDER BY reader_id LIMIT ?`, afterReaderID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var readers []chunkstore.ReaderIdentity
	for rows.Next() {
		var identity chunkstore.ReaderIdentity
		if err := rows.Scan(&identity.ReaderID, &identity.SetID, &identity.RootID, &identity.OwnerID); err != nil {
			return nil, err
		}
		readers = append(readers, identity)
	}
	return readers, rows.Err()
}

func (r *Registry) ConfirmChunkReaderOwnerDead(ctx context.Context, ownerID string) (bool, error) {
	id, err := uuid.Parse(ownerID)
	if err != nil || id == uuid.Nil || id.String() != ownerID {
		return false, ErrChunkReaderOwnerUnknown
	}
	tx, err := r.readerWriteTx(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	var device, inode string
	var dead bool
	if err := tx.QueryRowContext(ctx, `SELECT device,inode,dead FROM chunk_reader_owners WHERE owner_id=?`, ownerID).Scan(&device, &inode, &dead); err != nil {
		return false, fmt.Errorf("%w: %v", ErrChunkReaderOwnerUnknown, err)
	}
	dir, err := r.readerLockDir()
	if err != nil {
		return false, err
	}
	path := filepath.Join(dir, ownerID)
	if dead {
		return true, cleanupDeadReaderFile(path, device, inode)
	}
	file, err := openReaderLock(path)
	if err != nil {
		return false, fmt.Errorf("%w: %v", ErrChunkReaderOwnerUnknown, err)
	}
	defer file.Close()
	gotDevice, gotInode, err := readerFileIdentity(file)
	if err != nil {
		return false, err
	}
	if gotDevice != device || gotInode != inode {
		return false, ErrChunkReaderOwnerUnknown
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) {
			return false, nil
		}
		return false, err
	}
	if err := matchReaderFile(path, device, inode); err != nil {
		return false, fmt.Errorf("%w: %v", ErrChunkReaderOwnerUnknown, err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE chunk_reader_owners SET dead=1 WHERE owner_id=? AND device=? AND inode=? AND dead=0`, ownerID, device, inode); err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	// The receipt commits before unlink, so restart does not need this inode.
	return true, cleanupDeadReaderFile(path, device, inode)
}

func cleanupDeadReaderFile(path, device, inode string) error {
	if err := matchReaderFile(path, device, inode); err == nil {
		if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncReaderDirectory(filepath.Dir(path))
}

func (r *Registry) PruneChunkReaderOwners(ctx context.Context, afterOwnerID string, limit int) (string, error) {
	if limit < 1 || limit > 1000 {
		return afterOwnerID, fmt.Errorf("chunk owner page limit must be 1..1000")
	}
	rows, err := r.rdb.QueryContext(ctx, `SELECT owner_id FROM chunk_reader_owners WHERE owner_id>? AND NOT EXISTS(SELECT 1 FROM chunk_readers WHERE chunk_readers.owner_id=chunk_reader_owners.owner_id) ORDER BY owner_id LIMIT ?`, afterOwnerID, limit)
	if err != nil {
		return afterOwnerID, err
	}
	var owners []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			_ = rows.Close()
			return afterOwnerID, err
		}
		owners = append(owners, id)
	}
	err = rows.Err()
	_ = rows.Close()
	if err != nil {
		return afterOwnerID, err
	}
	if len(owners) == 0 {
		return "", nil
	}
	var failures []error
	next := afterOwnerID
	for _, id := range owners {
		next = id
		dead, err := r.ConfirmChunkReaderOwnerDead(ctx, id)
		if err != nil {
			failures = append(failures, err)
			continue
		}
		if !dead {
			continue
		}
		if err := r.pruneChunkReaderOwner(ctx, id); err != nil {
			failures = append(failures, err)
		}
	}
	return next, errors.Join(failures...)
}

func (r *Registry) pruneChunkReaderOwner(ctx context.Context, id string) error {
	tx, err := r.readerWriteTx(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var device, inode string
	err = tx.QueryRowContext(ctx, `SELECT device,inode FROM chunk_reader_owners WHERE owner_id=? AND dead=1 AND NOT EXISTS(SELECT 1 FROM chunk_readers WHERE owner_id=?)`, id, id).Scan(&device, &inode)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	dir, err := r.readerLockDir()
	if err != nil {
		return err
	}
	path := filepath.Join(dir, id)
	if err := matchReaderFile(path, device, inode); err == nil {
		if err := os.Remove(path); err != nil {
			return err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := syncReaderDirectory(dir); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM chunk_reader_owners WHERE owner_id=? AND dead=1`, id); err != nil {
		return err
	}
	return tx.Commit()
}
