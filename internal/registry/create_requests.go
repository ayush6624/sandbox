package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
)

var ErrCreateRequestConflict = errors.New("create request body conflicts with its recorded intent")

// CreateIntent links an internal request to allocation, including public fields.
// It is never attached to imported, adopted, or pool-building sandboxes.
type CreateIntent struct {
	ID              string
	Hash            [32]byte
	SourceType      string
	SourceID        string
	Metadata        map[string]string
	DefaultVcpus    int64
	DefaultMemMIB   int64
	ProgressAttempt int64
	ProgressOwner   CreateProgressOwner
	ProgressTarget  CreateProgressTarget
}

type CreateRequestResult struct {
	Phase   string
	Sandbox *Sandbox
	Status  int
	Code    string
	Detail  string
}

func (r *Registry) Path() string       { return r.path }
func (r *Registry) RegistryID() string { return r.registryID }

func (r *Registry) migrateCreateRequests() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS registry_identity (singleton INTEGER PRIMARY KEY CHECK(singleton=1), id TEXT NOT NULL);
 CREATE TABLE IF NOT EXISTS create_requests (
 id TEXT PRIMARY KEY, request_hash BLOB NOT NULL, sandbox_id TEXT UNIQUE,
 phase TEXT NOT NULL CHECK(phase IN ('allocated','succeeded','failed')),
 result BLOB, failure_status INTEGER NOT NULL DEFAULT 0,
 failure_code TEXT NOT NULL DEFAULT '', failure_detail TEXT NOT NULL DEFAULT '',
 created_at INTEGER NOT NULL, completed_at INTEGER,
 CHECK(phase='failed' OR sandbox_id IS NOT NULL),
 CHECK(phase!='succeeded' OR result IS NOT NULL));`)
	if err != nil {
		return err
	}
	if _, err := r.db.Exec(`INSERT OR IGNORE INTO registry_identity(singleton,id) VALUES (1,?)`, uuid.NewString()); err != nil {
		return err
	}
	if err := r.db.QueryRow(`SELECT id FROM registry_identity WHERE singleton=1`).Scan(&r.registryID); err != nil {
		return err
	}
	return r.migrateCreateProgress()
}

func (r *Registry) CreateRequest(ctx context.Context, intent CreateIntent) (CreateRequestResult, error) {
	var result CreateRequestResult
	if err := validateCreateProgressOwner(intent); err != nil {
		return result, err
	}
	var hash, body, progress []byte
	err := r.rdb.QueryRowContext(ctx, `SELECT request_hash,phase,result,failure_status,failure_code,failure_detail,
 (SELECT snapshot FROM create_progress WHERE id=create_requests.id) FROM create_requests WHERE id=?`, intent.ID).Scan(&hash, &result.Phase, &body, &result.Status, &result.Code, &result.Detail, &progress)
	if err != nil {
		return result, err
	}
	if string(hash) != string(intent.Hash[:]) {
		return result, ErrCreateRequestConflict
	}
	var p CreateProgress
	if progress != nil {
		if err := json.Unmarshal(progress, &p); err != nil {
			return result, fmt.Errorf("decode create progress: %w", err)
		}
	}
	if !matchesCreateProgressOwner(p, intent) {
		return result, ErrCreateRequestConflict
	}
	if result.Phase == "succeeded" {
		var sb Sandbox
		if err := json.Unmarshal(body, &sb); err != nil {
			return result, err
		}
		result.Sandbox = &sb
	}
	return result, nil
}

func applyCreateFields(sb *Sandbox, intent []CreateIntent) {
	if len(intent) == 1 {
		sb.SourceType, sb.SourceID, sb.Metadata = intent[0].SourceType, intent[0].SourceID, intent[0].Metadata
		if sb.Vcpus == 0 {
			sb.Vcpus = intent[0].DefaultVcpus
		}
		if sb.MemMIB == 0 {
			sb.MemMIB = intent[0].DefaultMemMIB
		}
	}
}

func linkCreateRequest(ctx context.Context, tx *sql.Tx, sandboxID string, intents []CreateIntent) error {
	if len(intents) == 0 {
		return nil
	}
	if len(intents) != 1 || intents[0].ID == "" {
		return errors.New("exactly one nonempty create intent is required")
	}
	intent := intents[0]
	if err := allocateCreateProgress(ctx, tx, intent); err != nil {
		return err
	}
	data, err := json.Marshal(intent.Metadata)
	if err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO create_requests(id,request_hash,sandbox_id,phase,created_at) VALUES (?,?,?,'allocated',?)`, intent.ID, intent.Hash[:], sandboxID, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("link create request: %w", err)
	}
	_, err = tx.ExecContext(ctx, `UPDATE sandboxes SET source_type=?,source_id=?,metadata=?, vcpus=CASE WHEN vcpus=0 THEN ? ELSE vcpus END, mem_mib=CASE WHEN mem_mib=0 THEN ? ELSE mem_mib END WHERE id=?`, intent.SourceType, intent.SourceID, string(data), intent.DefaultVcpus, intent.DefaultMemMIB, sandboxID)
	return err
}

func completeCreateRequest(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM create_requests WHERE sandbox_id=? AND phase='allocated')`, sandboxID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return nil
	}
	sb, err := scanSandbox(tx.QueryRowContext(ctx, `SELECT `+sandboxCols+` FROM sandboxes WHERE id=?`, sandboxID))
	if err != nil {
		return err
	}
	data, err := json.Marshal(sb)
	if err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE create_requests SET phase='succeeded',result=?,completed_at=? WHERE sandbox_id=? AND phase='allocated'`, data, time.Now().UnixMilli(), sandboxID); err != nil {
		return err
	}
	return terminalSandboxCreateProgress(ctx, tx, sandboxID)
}

// FailCreateRequest records a failure before allocation. Once allocated, only
// teardown can declare failure; an unknown live VM must not be marked rejected.
func (r *Registry) FailCreateRequest(ctx context.Context, intent CreateIntent, status int, code, detail string) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := progressForIntent(ctx, tx, intent); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO create_requests(id,request_hash,phase,failure_status,failure_code,failure_detail,created_at,completed_at) VALUES (?,?,'failed',?,?,?,?,?) ON CONFLICT(id) DO NOTHING`, intent.ID, intent.Hash[:], status, code, detail, time.Now().UnixMilli(), time.Now().UnixMilli()); err != nil {
		return err
	}
	if err := terminalCreateProgress(ctx, tx, intent.ID); err != nil {
		return err
	}
	return tx.Commit()
}

// CreateFailure retains a safe launch failure while teardown is unresolved.
type CreateFailure struct {
	Status int
	Code   string
	Detail string
}

// RecordCreateFailure saves the first cause without declaring a live allocation failed.
func (r *Registry) RecordCreateFailure(ctx context.Context, sandboxID string, failure CreateFailure) error {
	if failure.Status < 500 || failure.Status > 599 || failure.Code == "" || failure.Detail == "" {
		return errors.New("create failure requires a server error status and nonempty code and detail")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	res, err := tx.ExecContext(ctx, `UPDATE create_requests SET failure_status=?, failure_code=?, failure_detail=? WHERE sandbox_id=? AND phase='allocated' AND failure_status=0`, failure.Status, failure.Code, failure.Detail, sandboxID)
	if err != nil {
		return err
	}
	if n, err := res.RowsAffected(); err != nil {
		return err
	} else if n > 0 {
		p, err := readCreateProgress(tx.QueryRowContext(ctx, createProgressSelect+` WHERE p.id=(SELECT id FROM create_requests WHERE sandbox_id=?)`, sandboxID))
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return err
		}
		if err == nil {
			p.Condition = "tearing_down"
			if err := writeCreateProgress(ctx, tx, &p); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}
