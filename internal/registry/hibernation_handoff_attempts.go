package registry

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"unicode/utf8"

	"golang.org/x/sys/unix"
)

const handoffLockStripes = 64

var (
	ErrHandoffBusy          = errors.New("handoff attempt active")
	ErrHandoffAttemptClosed = errors.New("handoff attempt callback has returned")
)

// HandoffAttempt owns one generation until its callback and all producers drain.
// Callbacks must join their producers before returning and must not retain a copy.
type HandoffAttempt struct {
	*handoffAttemptState
}

type handoffAttemptState struct {
	mu         sync.Mutex
	reg        *Registry
	generation string
	active     bool
}

func (r *Registry) migrateHibernationHandoffAttempts() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS hibernation_handoff_locks (
 stripe INTEGER PRIMARY KEY CHECK(stripe >= 0 AND stripe < 64),
 device TEXT NOT NULL, inode TEXT NOT NULL)`)
	return err
}

// WithHibernationHandoff excludes producers and cleanup across Registry instances
// and processes. Registry.Close cannot release the callback-owned descriptor.
func (r *Registry) WithHibernationHandoff(ctx context.Context, generation string, fn func(*HandoffAttempt) error) error {
	if generation == "" || len(generation) > 1024 || !utf8.ValidString(generation) {
		return errors.New("invalid hibernation handoff generation")
	}
	if _, err := r.GetHibernationHandoff(ctx, generation); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	file, err := r.lockHibernationHandoff(ctx, generation)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := r.GetHibernationHandoff(ctx, generation); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	a := &HandoffAttempt{&handoffAttemptState{reg: r, generation: generation, active: true}}
	defer func() { a.mu.Lock(); a.active = false; a.mu.Unlock() }()
	return fn(a)
}

func (a *HandoffAttempt) Job(ctx context.Context) (HibernationHandoff, error) {
	if a == nil || a.handoffAttemptState == nil {
		return HibernationHandoff{}, ErrHandoffAttemptClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return HibernationHandoff{}, ErrHandoffAttemptClosed
	}
	return a.reg.GetHibernationHandoff(ctx, a.generation)
}

func (a *HandoffAttempt) CompleteBackup(ctx context.Context) error {
	if a == nil || a.handoffAttemptState == nil {
		return ErrHandoffAttemptClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return ErrHandoffAttemptClosed
	}
	res, err := a.reg.db.ExecContext(ctx, `UPDATE hibernation_handoffs SET backup_complete=1 WHERE generation=?`, a.generation)
	return snapshotUploadChanged(res, err)
}

// Remove cannot forget an unfinished backup obligation.
func (a *HandoffAttempt) Remove(ctx context.Context) error {
	if a == nil || a.handoffAttemptState == nil {
		return ErrHandoffAttemptClosed
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if !a.active {
		return ErrHandoffAttemptClosed
	}
	if _, err := a.reg.db.ExecContext(ctx, `DELETE FROM hibernation_handoffs WHERE generation=? AND backup_complete=1`, a.generation); err != nil {
		return err
	}
	if _, err := a.reg.GetHibernationHandoff(ctx, a.generation); errors.Is(err, sql.ErrNoRows) {
		return nil
	} else if err != nil {
		return err
	}
	return errors.New("hibernation backup is still pending")
}

// The SHA-256 first-byte mapping is the persisted lock format. Never change it
// while an older process can still own a handoff attempt.
func handoffLockStripe(generation string) int {
	digest := sha256.Sum256([]byte(generation))
	return int(digest[0]) % handoffLockStripes
}

func (r *Registry) lockHibernationHandoff(ctx context.Context, generation string) (_ *os.File, err error) {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	// Reserve SQLite's writer before examining initialization state.
	if _, err := tx.ExecContext(ctx, `UPDATE hibernation_handoff_locks SET stripe=stripe WHERE 0`); err != nil {
		return nil, err
	}
	stripe := handoffLockStripe(generation)
	var device, inode string
	err = tx.QueryRowContext(ctx, `SELECT device,inode FROM hibernation_handoff_locks WHERE stripe=?`, stripe).Scan(&device, &inode)
	initialized := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	dirPath, err := filepath.Abs(r.path + ".handoff-locks")
	if err != nil {
		return nil, err
	}
	if !initialized {
		if err := os.Mkdir(dirPath, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return nil, err
		}
		if err := syncReaderDirectory(filepath.Dir(dirPath)); err != nil {
			return nil, err
		}
	}
	dirFD, err := unix.Open(dirPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer unix.Close(dirFD)
	name := fmt.Sprintf("%02d.lock", stripe)
	flags := unix.O_RDWR | unix.O_CLOEXEC | unix.O_NOFOLLOW | unix.O_NONBLOCK
	if !initialized {
		flags |= unix.O_CREAT
	}
	fd, err := unix.Openat(dirFD, name, flags, 0600)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(dirPath, name))
	defer func() {
		if err != nil {
			_ = file.Close()
		}
	}()
	gotDevice, gotInode, err := readerFileIdentity(file)
	if err != nil {
		return nil, err
	}
	if initialized && (gotDevice != device || gotInode != inode) {
		return nil, errors.New("handoff lock file identity changed")
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, ErrHandoffBusy
		}
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(dirFD, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return nil, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || fmt.Sprint(stat.Dev) != gotDevice || fmt.Sprint(stat.Ino) != gotInode {
		return nil, errors.New("handoff lock file changed during acquisition")
	}
	if !initialized {
		if err := file.Sync(); err != nil {
			return nil, err
		}
		if err := unix.Fsync(dirFD); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO hibernation_handoff_locks(stripe,device,inode) VALUES(?,?,?)`, stripe, gotDevice, gotInode); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return file, nil
}
