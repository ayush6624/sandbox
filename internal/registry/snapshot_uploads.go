package registry

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// SnapshotUpload is a read projection of this host's capture-owned upload job.
// Importing a Snapshot with this projection never imports its upload job.
type SnapshotUpload struct {
	State         string     `json:"state"`
	Attempts      int        `json:"attempts"`
	NextAttemptAt *time.Time `json:"next_attempt_at,omitempty"`
	Error         string     `json:"error,omitempty"`
}

// CreateCapturedSnapshot accepts local artifacts and their upload obligation
// together. Peer and object-store imports use CreateSnapshot instead.
func (r *Registry) CreateCapturedSnapshot(ctx context.Context, s Snapshot) error {
	if s.Golden || s.Role == SnapshotRoleBase || s.Role == SnapshotRoleBuiltin {
		return fmt.Errorf("internal base snapshots cannot own upload jobs")
	}
	s.Durability = "local"
	return r.createSnapshot(ctx, s, true)
}

// DueSnapshotUploads includes interrupted attempts. The server excludes its
// active attempts before claiming; one server owns this registry.
func (r *Registry) DueSnapshotUploads(ctx context.Context, now time.Time, limit int) ([]string, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := r.rdb.QueryContext(ctx, `
		SELECT snapshot_id FROM snapshot_uploads
		WHERE state = 'uploading' OR (state IN ('pending', 'retrying') AND next_attempt_at <= ?)
		ORDER BY COALESCE(next_attempt_at, 0), snapshot_id LIMIT ?`, now.UnixMilli(), limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

func (r *Registry) BeginSnapshotUpload(ctx context.Context, id string, now time.Time) (SnapshotUpload, error) {
	var upload SnapshotUpload
	err := r.db.QueryRowContext(ctx, `
		UPDATE snapshot_uploads SET state = 'uploading', attempts = attempts + 1, next_attempt_at = NULL
		WHERE snapshot_id = ? AND (state = 'uploading' OR
			(state IN ('pending', 'retrying') AND next_attempt_at <= ?))
		RETURNING state, attempts, error`, id, now.UnixMilli()).Scan(&upload.State, &upload.Attempts, &upload.Error)
	return upload, err
}

func (r *Registry) RetrySnapshotUpload(ctx context.Context, id string, next time.Time, message string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE snapshot_uploads SET state = 'retrying', next_attempt_at = ?, error = ?
		WHERE snapshot_id = ? AND state = 'uploading'`, next.UnixMilli(), message, id)
	return snapshotUploadChanged(res, err)
}

func (r *Registry) FailSnapshotUpload(ctx context.Context, id, message string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE snapshot_uploads SET state = 'failed', next_attempt_at = NULL, error = ?
		WHERE snapshot_id = ? AND state = 'uploading'`, message, id)
	return snapshotUploadChanged(res, err)
}

// CompleteSnapshotUpload commits public durability with removal of the upload
// obligation. A repeated completion is harmless; a deleted row stays deleted.
func (r *Registry) CompleteSnapshotUpload(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `
		UPDATE snapshots SET durability = 'durable' WHERE id = ?
		AND (durability = 'durable' OR EXISTS (
			SELECT 1 FROM snapshot_uploads WHERE snapshot_id = snapshots.id AND state = 'uploading'))`, id)
	if err := snapshotUploadChanged(res, err); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM snapshot_uploads WHERE snapshot_id = ?`, id); err != nil {
		return err
	}
	return tx.Commit()
}

// RetireGoldenSnapshot retains the immutable base for existing diff snapshots
// while releasing the active-golden constraint for the replacement.
func (r *Registry) RetireGoldenSnapshot(ctx context.Context, id string) error {
	res, err := r.db.ExecContext(ctx, `
		UPDATE snapshots SET golden = 0, role = ?, warm_target = 0
		WHERE id = ? AND (golden = 1 OR role = ?)`, SnapshotRoleBase, id, SnapshotRoleBase)
	return snapshotUploadChanged(res, err)
}

func snapshotUploadChanged(result sql.Result, err error) error {
	if err != nil {
		return err
	}
	n, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}
