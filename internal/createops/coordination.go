package createops

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// SetCoordination records dispatcher activity without changing assignment or outcomes.
func (s *SQLiteStore) SetCoordination(ctx context.Context, operationID string, memberIDs []string, phase string) error {
	if phase != "placing" && phase != "retrying" {
		return fmt.Errorf("invalid coordination phase %q", phase)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range memberIDs {
		var assigned, terminal bool
		var current sql.NullString
		err := tx.QueryRowContext(ctx, `SELECT worker_host IS NOT NULL,outcome IS NOT NULL,coordination_phase FROM create_members WHERE operation_id=? AND member_id=?`, operationID, id).Scan(&assigned, &terminal, &current)
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("member %s: %w", id, ErrNotFound)
		}
		if err != nil {
			return err
		}
		if terminal {
			continue
		}
		if assigned != (phase == "retrying") {
			return fmt.Errorf("member %s assignment does not permit coordination phase %s", id, phase)
		}
		if current.Valid && current.String == phase {
			continue
		}
		if _, err := tx.ExecContext(ctx, `UPDATE create_members SET coordination_phase=?,coordination_updated_at=? WHERE operation_id=? AND member_id=?`, phase, now, operationID, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}
