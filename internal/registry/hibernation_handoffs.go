package registry

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
)

const MaxPendingHibernationHandoffs = 32

var ErrHibernationHandoffLimit = errors.New("pending hibernation handoff limit reached")

// HibernationHandoff owns immutable source files after their serving row is gone.
// Cache acknowledgment and cloud completion are independent retention facts.
type HibernationHandoff struct {
	Generation       string
	SandboxID        string
	Offer            []byte
	ExpectedRevision int64
	Published        bool // Offer was published or superseded by a newer control revision.
	BackupComplete   bool
	CacheReady       bool
}

func (r *Registry) migrateHibernationHandoffs() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS hibernation_handoffs (
 generation TEXT PRIMARY KEY,
 sandbox_id TEXT NOT NULL,
 offer BLOB NOT NULL,
 expected_revision INTEGER NOT NULL,
 published INTEGER NOT NULL DEFAULT 0 CHECK (published IN (0,1)),
 backup_complete INTEGER NOT NULL DEFAULT 0 CHECK (backup_complete IN (0,1)),
 cache_ready INTEGER NOT NULL DEFAULT 0 CHECK (cache_ready IN (0,1))
 ); CREATE INDEX IF NOT EXISTS hibernation_handoffs_sandbox ON hibernation_handoffs(sandbox_id)`)
	return err
}

// CommitHibernationHandoff couples release of the frozen row with its durable
// upload/publication obligation. Replaying an old generation cannot delete a
// sandbox that has since returned to this registry.
func (r *Registry) CommitHibernationHandoff(ctx context.Context, job HibernationHandoff) error {
	if job.Generation == "" || job.SandboxID == "" || !json.Valid(job.Offer) || job.ExpectedRevision < 0 || job.Published || job.BackupComplete || job.CacheReady {
		return errors.New("invalid initial hibernation handoff")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var existing HibernationHandoff
	err = tx.QueryRowContext(ctx, `SELECT sandbox_id, offer, expected_revision FROM hibernation_handoffs WHERE generation=?`, job.Generation).Scan(&existing.SandboxID, &existing.Offer, &existing.ExpectedRevision)
	if err == nil {
		if existing.SandboxID != job.SandboxID || existing.ExpectedRevision != job.ExpectedRevision || !bytes.Equal(existing.Offer, job.Offer) {
			return errors.New("hibernation generation already names another handoff")
		}
		return tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	var pending int
	if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM hibernation_handoffs`).Scan(&pending); err != nil {
		return err
	}
	if pending >= MaxPendingHibernationHandoffs {
		return ErrHibernationHandoffLimit
	}
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM sandboxes WHERE id=?`, job.SandboxID).Scan(&status); err != nil {
		return err
	}
	if status != StatusHibernated {
		return fmt.Errorf("sandbox %s is %s, not hibernated", job.SandboxID, status)
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO hibernation_handoffs(generation,sandbox_id,offer,expected_revision) VALUES(?,?,?,?)`, job.Generation, job.SandboxID, job.Offer, job.ExpectedRevision); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sandbox_ports WHERE sandbox_id=?`, job.SandboxID); err != nil {
		return err
	}
	if err := failAllocatedCreateRequest(ctx, tx, job.SandboxID); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM sandboxes WHERE id=?`, job.SandboxID); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) GetHibernationHandoff(ctx context.Context, generation string) (HibernationHandoff, error) {
	var job HibernationHandoff
	err := r.rdb.QueryRowContext(ctx, `SELECT generation,sandbox_id,offer,expected_revision,published,backup_complete,cache_ready FROM hibernation_handoffs WHERE generation=?`, generation).Scan(&job.Generation, &job.SandboxID, &job.Offer, &job.ExpectedRevision, &job.Published, &job.BackupComplete, &job.CacheReady)
	return job, err
}

func (r *Registry) ListHibernationHandoffs(ctx context.Context) ([]HibernationHandoff, error) {
	rows, err := r.rdb.QueryContext(ctx, `SELECT generation,sandbox_id,offer,expected_revision,published,backup_complete,cache_ready FROM hibernation_handoffs ORDER BY generation`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var jobs []HibernationHandoff
	for rows.Next() {
		var job HibernationHandoff
		if err := rows.Scan(&job.Generation, &job.SandboxID, &job.Offer, &job.ExpectedRevision, &job.Published, &job.BackupComplete, &job.CacheReady); err != nil {
			return nil, err
		}
		jobs = append(jobs, job)
	}
	return jobs, rows.Err()
}

func (r *Registry) MarkHibernationHandoffPublished(ctx context.Context, generation string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE hibernation_handoffs SET published=1 WHERE generation=?`, generation)
	return snapshotUploadChanged(res, err)
}

func (r *Registry) AckHibernationHandoffCache(ctx context.Context, generation string) error {
	res, err := r.db.ExecContext(ctx, `UPDATE hibernation_handoffs SET cache_ready=1 WHERE generation=?`, generation)
	return snapshotUploadChanged(res, err)
}
