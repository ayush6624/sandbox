package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

const createProgressSelect = `SELECT p.snapshot,r.request_hash,r.phase,r.result,r.failure_status,r.failure_code,r.failure_detail
 FROM create_progress p LEFT JOIN create_requests r ON r.id=p.id`

const inlineTerminalCreateProgressPredicate = `json_extract(snapshot,'$.Condition') IN ('succeeded','failed')
 AND json_type(snapshot,'$.Outcome')='object'`

// Read progress and its historical outcome in one database snapshot. An inline
// outcome remains authoritative until compaction verifies it against the ledger.
func readCreateProgress(row rowScanner) (CreateProgress, error) {
	p, ledger, err := scanStoredCreateProgress(row)
	if err != nil {
		return p, err
	}
	if p.Outcome == nil && (p.Condition == "succeeded" || p.Condition == "failed") {
		p.Outcome, err = ledger.outcome(p)
	}
	return p, err
}

type createProgressLedger struct {
	hash   []byte
	phase  sql.NullString
	result []byte
	status sql.NullInt64
	code   sql.NullString
	detail sql.NullString
}

func scanStoredCreateProgress(row rowScanner) (CreateProgress, createProgressLedger, error) {
	var p CreateProgress
	var ledger createProgressLedger
	var body []byte
	if err := row.Scan(&body, &ledger.hash, &ledger.phase, &ledger.result, &ledger.status, &ledger.code, &ledger.detail); err != nil {
		return p, ledger, err
	}
	if err := json.Unmarshal(body, &p); err != nil {
		return p, ledger, fmt.Errorf("decode create progress: %w", err)
	}
	return p, ledger, nil
}

func (ledger createProgressLedger) outcome(p CreateProgress) (*CreateRequestResult, error) {
	if !ledger.phase.Valid || (ledger.phase.String != "succeeded" && ledger.phase.String != "failed") || ledger.phase.String != p.Condition {
		return nil, fmt.Errorf("create progress %q has no matching terminal ledger", p.ID)
	}
	if string(ledger.hash) != string(p.RequestHash[:]) {
		return nil, ErrCreateRequestConflict
	}
	outcome := &CreateRequestResult{Phase: ledger.phase.String, Status: int(ledger.status.Int64), Code: ledger.code.String, Detail: ledger.detail.String}
	if outcome.Phase == "succeeded" {
		var sb Sandbox
		if err := json.Unmarshal(ledger.result, &sb); err != nil {
			return nil, fmt.Errorf("decode create progress outcome: %w", err)
		}
		outcome.Sandbox = &sb
	}
	return outcome, nil
}

// CompactCreateProgress removes duplicate historical outcomes from at most limit
// legacy snapshots. The cursor advances over examined rows even on errors, so a
// damaged record cannot prevent later records from being compacted.
func (r *Registry) CompactCreateProgress(ctx context.Context, afterID string, limit int) (string, error) {
	if limit < 1 || limit > 1000 {
		return afterID, errors.New("create progress limit must be between 1 and 1000")
	}
	rows, err := r.rdb.QueryContext(ctx, `SELECT id FROM create_progress WHERE `+inlineTerminalCreateProgressPredicate+` AND id>? ORDER BY id LIMIT ?`, afterID, limit)
	if err != nil {
		return afterID, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return afterID, err
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	closeErr := rows.Close()
	if err = errors.Join(err, closeErr); err != nil {
		return afterID, err
	}
	if len(ids) == 0 {
		return "", nil
	}
	var errs []error
	next := afterID
	for _, id := range ids {
		if err := ctx.Err(); err != nil {
			return next, errors.Join(append(errs, err)...)
		}
		next = id
		if err := r.compactCreateProgress(ctx, id); err != nil {
			errs = append(errs, fmt.Errorf("compact create progress %q: %w", id, err))
		}
	}
	return next, errors.Join(errs...)
}

func (r *Registry) compactCreateProgress(ctx context.Context, id string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, ledger, err := scanStoredCreateProgress(tx.QueryRowContext(ctx, createProgressSelect+` WHERE p.id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if p.Outcome == nil || (p.Condition != "succeeded" && p.Condition != "failed") {
		return nil
	}
	outcome, err := ledger.outcome(p)
	if err != nil {
		return err
	}
	if !reflect.DeepEqual(p.Outcome, outcome) {
		return errors.New("inline outcome differs from historical ledger")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE create_progress SET snapshot=json_remove(snapshot,'$.Outcome') WHERE id=?`, id); err != nil {
		return err
	}
	return tx.Commit()
}
