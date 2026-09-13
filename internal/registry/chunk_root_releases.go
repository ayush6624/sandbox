package registry

import (
	"context"
	"errors"
	"fmt"
)

var (
	ErrInvalidChunkRootRelease  = errors.New("invalid chunk root release")
	ErrChunkRootReleaseConflict = errors.New("chunk root release sandbox identity conflict")
)

// ChunkRootRelease records a durable obligation to retire one generation root.
// Revision is the first control revision that established the obligation.
// Fenced records proven retirement authority. It may coexist with a positive
// Revision when later proof upgrades an existing obligation.
type ChunkRootRelease struct {
	SetID     string
	RootID    string
	SandboxID string
	Revision  int64
	Fenced    bool
}

func (r *Registry) migrateChunkRootReleases() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS chunk_root_releases (
	 set_id TEXT NOT NULL,
	 root_id TEXT NOT NULL,
	 sandbox_id TEXT NOT NULL,
	 revision INTEGER NOT NULL,
	 fenced INTEGER NOT NULL CHECK (fenced IN (0,1)),
	 PRIMARY KEY (set_id, root_id),
	 CHECK (set_id <> ''),
	 CHECK (root_id <> ''),
	 CHECK (sandbox_id <> ''),
	 CHECK (revision >= 0),
	 CHECK (revision > 0 OR fenced = 1)
	)`)
	return err
}

// QueueChunkRootRelease persists an obligation before the authority-changing
// operation. A replay preserves the first observed revision and may only raise
// Fenced. The conditional upsert makes a different sandbox identity a conflict
// without modifying the existing row.
func (r *Registry) QueueChunkRootRelease(ctx context.Context, release ChunkRootRelease) error {
	if err := validateChunkRootRelease(release); err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, `
		INSERT INTO chunk_root_releases(set_id, root_id, sandbox_id, revision, fenced)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(set_id, root_id) DO UPDATE SET
			fenced = MAX(chunk_root_releases.fenced, excluded.fenced)
		WHERE chunk_root_releases.sandbox_id = excluded.sandbox_id`,
		release.SetID, release.RootID, release.SandboxID, release.Revision, release.Fenced)
	if err != nil {
		return fmt.Errorf("queue chunk root release: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("queue chunk root release rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("%w for set %q root %q", ErrChunkRootReleaseConflict, release.SetID, release.RootID)
	}
	return nil
}

// ListChunkRootReleases returns pending obligations in stable key order.
func (r *Registry) ListChunkRootReleases(ctx context.Context) ([]ChunkRootRelease, error) {
	rows, err := r.rdb.QueryContext(ctx, `
		SELECT set_id, root_id, sandbox_id, revision, fenced
		FROM chunk_root_releases
		ORDER BY set_id, root_id`)
	if err != nil {
		return nil, fmt.Errorf("list chunk root releases: %w", err)
	}
	defer rows.Close()

	var releases []ChunkRootRelease
	for rows.Next() {
		var release ChunkRootRelease
		if err := rows.Scan(&release.SetID, &release.RootID, &release.SandboxID, &release.Revision, &release.Fenced); err != nil {
			return nil, fmt.Errorf("scan chunk root release: %w", err)
		}
		releases = append(releases, release)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list chunk root releases: %w", err)
	}
	return releases, nil
}

// RemoveChunkRootRelease clears an obligation after remote retirement has been
// confirmed. Removing a missing obligation succeeds.
func (r *Registry) RemoveChunkRootRelease(ctx context.Context, setID, rootID string) error {
	if setID == "" || rootID == "" {
		return ErrInvalidChunkRootRelease
	}
	if _, err := r.db.ExecContext(ctx, `DELETE FROM chunk_root_releases WHERE set_id=? AND root_id=?`, setID, rootID); err != nil {
		return fmt.Errorf("remove chunk root release: %w", err)
	}
	return nil
}

func validateChunkRootRelease(release ChunkRootRelease) error {
	if release.SetID == "" || release.RootID == "" || release.SandboxID == "" || release.Revision < 0 || release.Revision == 0 && !release.Fenced {
		return ErrInvalidChunkRootRelease
	}
	return nil
}
