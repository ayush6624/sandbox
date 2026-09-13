package createops

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

var ErrNotFound = errors.New("create operation not found")

const pendingOperationsPredicate = `status IN ('pending','running') OR (type='sandbox_create' AND completed_at IS NOT NULL AND final_status IS NULL)`

type SQLiteStore struct {
	db            *sql.DB
	coordinatorID string
}

func Open(path string) (*SQLiteStore, error) {
	if path == "" {
		return nil, errors.New("create operations database path is required")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path+"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(ON)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &SQLiteStore{db: db}
	if err := s.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *SQLiteStore) migrate(ctx context.Context) error {
	conn, err := s.db.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `BEGIN IMMEDIATE`); err != nil {
		return err
	}
	defer conn.ExecContext(context.Background(), `ROLLBACK`)
	_, err = conn.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS create_operations (
 id TEXT PRIMARY KEY, scope TEXT NOT NULL UNIQUE, body_hash BLOB NOT NULL,
 request_id TEXT NOT NULL DEFAULT '',
 type TEXT NOT NULL CHECK(type IN ('sandbox_create','sandbox_batch_create')),
 max_parallelism INTEGER NOT NULL CHECK(max_parallelism BETWEEN 1 AND 32),
 status TEXT NOT NULL CHECK(status IN ('pending','running','succeeded','failed','partially_succeeded')), accepted_status INTEGER NOT NULL,
 accepted_body BLOB NOT NULL, final_status INTEGER, final_body BLOB, created_at TEXT NOT NULL, completed_at TEXT
);
CREATE TABLE IF NOT EXISTS create_members (
 operation_id TEXT NOT NULL, member_id TEXT NOT NULL PRIMARY KEY, item_index INTEGER NOT NULL CHECK(item_index >= 0),
 spec BLOB NOT NULL, worker_host TEXT, worker_registry TEXT, worker_progress INTEGER NOT NULL DEFAULT 0, outcome BLOB, progress BLOB, coordination_phase TEXT, coordination_updated_at TEXT,
 UNIQUE(operation_id, item_index), FOREIGN KEY(operation_id) REFERENCES create_operations(id),
 CHECK((worker_host IS NULL AND worker_registry IS NULL) OR (worker_host IS NOT NULL AND worker_host != '' AND worker_registry IS NOT NULL AND worker_registry != ''))
);`)
	if err != nil {
		return err
	}
	var hasRequestID int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('create_operations') WHERE name='request_id'`).Scan(&hasRequestID); err != nil {
		return err
	}
	if hasRequestID == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE create_operations ADD COLUMN request_id TEXT NOT NULL DEFAULT ''`); err != nil {
			return err
		}
	}
	var hasWorkerProgress int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('create_members') WHERE name='worker_progress'`).Scan(&hasWorkerProgress); err != nil {
		return err
	}
	if hasWorkerProgress == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE create_members ADD COLUMN worker_progress INTEGER NOT NULL DEFAULT 0`); err != nil {
			return err
		}
	}
	var hasProgress int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('create_members') WHERE name='progress'`).Scan(&hasProgress); err != nil {
		return err
	}
	if hasProgress == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE create_members ADD COLUMN progress BLOB`); err != nil {
			return err
		}
	}
	var hasCoordination int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_info('create_members') WHERE name='coordination_phase'`).Scan(&hasCoordination); err != nil {
		return err
	}
	if hasCoordination == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE create_members ADD COLUMN coordination_phase TEXT; ALTER TABLE create_members ADD COLUMN coordination_updated_at TEXT`); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS create_coordinator_identity (singleton INTEGER PRIMARY KEY CHECK(singleton=1), id TEXT NOT NULL)`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `INSERT OR IGNORE INTO create_coordinator_identity(singleton,id) VALUES(1,?)`, uuid.NewString()); err != nil {
		return err
	}
	if err := conn.QueryRowContext(ctx, `SELECT id FROM create_coordinator_identity WHERE singleton=1`).Scan(&s.coordinatorID); err != nil {
		return err
	}
	var hasCreatedOrder int
	if err := conn.QueryRowContext(ctx, `SELECT COUNT(*) FROM pragma_table_xinfo('create_operations') WHERE name='created_at_order'`).Scan(&hasCreatedOrder); err != nil {
		return err
	}
	if hasCreatedOrder == 0 {
		if _, err := conn.ExecContext(ctx, `ALTER TABLE create_operations ADD COLUMN created_at_order TEXT GENERATED ALWAYS AS (substr(created_at,1,19)||'.'||substr(rtrim(substr(created_at,21),'Z')||'000000000',1,9)||'Z') VIRTUAL`); err != nil {
			return err
		}
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS create_operations_history ON create_operations(created_at_order DESC,id DESC)`); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `CREATE INDEX IF NOT EXISTS create_operations_pending_order ON create_operations(created_at_order,id) WHERE `+pendingOperationsPredicate); err != nil {
		return err
	}
	if _, err := conn.ExecContext(ctx, `DROP INDEX IF EXISTS create_operations_pending`); err != nil {
		return err
	}
	_, err = conn.ExecContext(ctx, `COMMIT`)
	return err
}

func (s *SQLiteStore) CoordinatorID() string { return s.coordinatorID }

func (s *SQLiteStore) Close() error { return s.db.Close() }

func (s *SQLiteStore) Accept(ctx context.Context, req AcceptRequest) (Acceptance, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Acceptance{}, err
	}
	defer tx.Rollback()
	var hash, body []byte
	var status int
	var id string
	err = tx.QueryRowContext(ctx, `SELECT id, body_hash, accepted_status, accepted_body FROM create_operations WHERE scope=?`, req.Scope).Scan(&id, &hash, &status, &body)
	if err == nil {
		if string(hash) != string(req.BodyHash) {
			return Acceptance{}, ErrIdempotencyConflict
		}
		op, err := s.operation(ctx, tx, id)
		if err != nil {
			return Acceptance{}, err
		}
		return Acceptance{Operation: op, ResponseStatus: status, ResponseBody: body}, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return Acceptance{}, err
	}
	now := time.Now().UTC()
	if _, err = tx.ExecContext(ctx, `INSERT INTO create_operations(id,request_id,scope,body_hash,type,max_parallelism,status,accepted_status,accepted_body,created_at) VALUES(?,?,?,?,?,?,?,?,?,?)`, req.ID, req.RequestID, req.Scope, req.BodyHash, req.Type, req.MaxParallelism, "pending", req.ResponseStatus, req.ResponseBody, now.Format(time.RFC3339Nano)); err != nil {
		return Acceptance{}, err
	}
	for _, m := range req.Members {
		spec, err := json.Marshal(m.Spec)
		if err != nil {
			return Acceptance{}, err
		}
		if _, err = tx.ExecContext(ctx, `INSERT INTO create_members(operation_id,member_id,item_index,spec,coordination_phase,coordination_updated_at) VALUES(?,?,?,?,'queued',?)`, req.ID, m.ID, m.Index, spec, now.Format(time.RFC3339Nano)); err != nil {
			return Acceptance{}, err
		}
	}
	op, err := s.operation(ctx, tx, req.ID)
	if err != nil {
		return Acceptance{}, err
	}
	if err = tx.Commit(); err != nil {
		return Acceptance{}, err
	}
	return Acceptance{Operation: op, Created: true, ResponseStatus: req.ResponseStatus, ResponseBody: req.ResponseBody}, nil
}

func (s *SQLiteStore) SetFinalResponse(ctx context.Context, id string, status int, body []byte) error {
	_, err := s.db.ExecContext(ctx, `UPDATE create_operations SET final_status=?,final_body=? WHERE id=? AND type='sandbox_create' AND completed_at IS NOT NULL AND final_status IS NULL`, status, body, id)
	return err
}
func (s *SQLiteStore) FinalResponse(ctx context.Context, id string) (int, []byte, bool, error) {
	var status sql.NullInt64
	var body []byte
	err := s.db.QueryRowContext(ctx, `SELECT final_status,final_body FROM create_operations WHERE id=?`, id).Scan(&status, &body)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil, false, ErrNotFound
	}
	if err != nil {
		return 0, nil, false, err
	}
	return int(status.Int64), body, status.Valid, nil
}

func (s *SQLiteStore) Get(ctx context.Context, id string) (Operation, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	op, err := s.operation(ctx, tx, id)
	if err != nil {
		return Operation{}, err
	}
	return op, tx.Commit()
}
func (s *SQLiteStore) List(ctx context.Context, query ListQuery) (OperationPage, error) {
	if query.Limit < 1 || query.Limit > 100 || query.Offset < 0 || (query.BeforeID != "" && query.Offset != 0) {
		return OperationPage{}, ErrInvalidPage
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return OperationPage{}, err
	}
	defer tx.Rollback()
	statement := `SELECT id FROM create_operations ORDER BY created_at_order DESC,id DESC LIMIT ? OFFSET ?`
	args := []any{query.Limit + 1, query.Offset}
	if query.BeforeID != "" {
		var created string
		if err := tx.QueryRowContext(ctx, `SELECT created_at_order FROM create_operations WHERE id=?`, query.BeforeID).Scan(&created); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return OperationPage{}, ErrInvalidPage
			}
			return OperationPage{}, err
		}
		statement = `SELECT id FROM create_operations WHERE (created_at_order,id)<(?,?) ORDER BY created_at_order DESC,id DESC LIMIT ?`
		args = []any{created, query.BeforeID, query.Limit + 1}
	}
	rows, err := tx.QueryContext(ctx, statement, args...)
	if err != nil {
		return OperationPage{}, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return OperationPage{}, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return OperationPage{}, err
	}
	rows.Close()
	if len(ids) == 0 && query.Offset > 0 {
		var count int
		if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM create_operations`).Scan(&count); err != nil {
			return OperationPage{}, err
		}
		if query.Offset > count {
			return OperationPage{}, ErrInvalidPage
		}
	}
	page := OperationPage{Operations: make([]Operation, 0, query.Limit)}
	if len(ids) > query.Limit {
		ids = ids[:query.Limit]
		page.NextID = ids[len(ids)-1]
	}
	for _, id := range ids {
		op, err := s.operation(ctx, tx, id)
		if err != nil {
			return OperationPage{}, err
		}
		page.Operations = append(page.Operations, op)
	}
	return page, tx.Commit()
}
func (s *SQLiteStore) Pending(ctx context.Context) ([]Operation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id FROM create_operations WHERE `+pendingOperationsPredicate+` ORDER BY created_at_order ASC,id ASC`)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	var out []Operation
	for _, id := range ids {
		op, err := s.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

func (s *SQLiteStore) Assign(ctx context.Context, operationID string, memberIDs []string, worker Worker) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, id := range memberIDs {
		res, err := tx.ExecContext(ctx, `UPDATE create_members SET worker_host=?,worker_registry=?,worker_progress=?,coordination_phase='assigned',coordination_updated_at=? WHERE operation_id=? AND member_id=? AND outcome IS NULL AND worker_host IS NULL`, worker.HostID, worker.RegistryID, worker.CreateProgress, now, operationID, id)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		if n != 1 {
			return fmt.Errorf("member %s was already assigned or completed", id)
		}
	}
	_, err = tx.ExecContext(ctx, `UPDATE create_operations SET status='running' WHERE id=? AND status='pending'`, operationID)
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SQLiteStore) Record(ctx context.Context, operationID string, outcomes []Outcome) (Operation, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return Operation{}, err
	}
	defer tx.Rollback()
	op, err := s.record(ctx, tx, operationID, outcomes)
	if err != nil {
		return Operation{}, err
	}
	if err = tx.Commit(); err != nil {
		return Operation{}, err
	}
	return op, nil
}

func (s *SQLiteStore) record(ctx context.Context, tx *sql.Tx, operationID string, outcomes []Outcome) (Operation, error) {
	now := time.Now().UTC()
	for _, out := range outcomes {
		b, err := json.Marshal(out)
		if err != nil {
			return Operation{}, err
		}
		if _, err := tx.ExecContext(ctx, `UPDATE create_members SET outcome=?,coordination_phase='completed',coordination_updated_at=? WHERE operation_id=? AND member_id=? AND outcome IS NULL`, b, now.Format(time.RFC3339Nano), operationID, out.ID); err != nil {
			return Operation{}, err
		}
	}
	op, err := s.operation(ctx, tx, operationID)
	if err != nil {
		return Operation{}, err
	}
	done := true
	success := 0
	failed := 0
	for _, m := range op.Members {
		if m.Outcome == nil {
			done = false
			continue
		}
		if m.Outcome.Failure == nil {
			success++
		} else {
			failed++
		}
	}
	if done && op.CompletedAt == nil {
		status := "succeeded"
		if success == 0 {
			status = "failed"
		} else if failed > 0 {
			status = "partially_succeeded"
		}
		if _, err = tx.ExecContext(ctx, `UPDATE create_operations SET status=?,completed_at=? WHERE id=?`, status, now.Format(time.RFC3339Nano), operationID); err != nil {
			return Operation{}, err
		}
		op.Status = status
		op.CompletedAt = &now
	}
	return op, nil
}

type queryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
	QueryContext(context.Context, string, ...any) (*sql.Rows, error)
}

func (s *SQLiteStore) operation(ctx context.Context, q queryer, id string) (Operation, error) {
	var op Operation
	var created string
	var completed sql.NullString
	err := q.QueryRowContext(ctx, `SELECT id,request_id,type,max_parallelism,status,created_at,completed_at FROM create_operations WHERE id=?`, id).Scan(&op.ID, &op.RequestID, &op.Type, &op.MaxParallelism, &op.Status, &created, &completed)
	if errors.Is(err, sql.ErrNoRows) {
		return Operation{}, ErrNotFound
	}
	if err != nil {
		return Operation{}, err
	}
	op.CreatedAt, _ = time.Parse(time.RFC3339Nano, created)
	if completed.Valid {
		t, _ := time.Parse(time.RFC3339Nano, completed.String)
		op.CompletedAt = &t
	}
	rows, err := q.QueryContext(ctx, `SELECT member_id,item_index,spec,worker_host,worker_registry,worker_progress,outcome,progress,coordination_phase,coordination_updated_at FROM create_members WHERE operation_id=? ORDER BY item_index`, id)
	if err != nil {
		return Operation{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var m StoredMember
		var spec, progress []byte
		var host, reg, out, coordinationPhase, coordinationUpdated sql.NullString
		var workerProgress bool
		if err := rows.Scan(&m.ID, &m.Index, &spec, &host, &reg, &workerProgress, &out, &progress, &coordinationPhase, &coordinationUpdated); err != nil {
			return Operation{}, err
		}
		if err := json.Unmarshal(spec, &m.Spec); err != nil {
			return Operation{}, err
		}
		if host.Valid {
			m.Worker = &Worker{HostID: host.String, RegistryID: reg.String, CreateProgress: workerProgress}
		}
		if out.Valid {
			var o Outcome
			if err := json.Unmarshal([]byte(out.String), &o); err != nil {
				return Operation{}, err
			}
			m.Outcome = &o
		}
		if progress != nil {
			var p registry.CreateProgress
			if err := json.Unmarshal(progress, &p); err != nil {
				return Operation{}, err
			}
			m.Progress = &p
		}
		if coordinationPhase.Valid {
			updated, err := time.Parse(time.RFC3339Nano, coordinationUpdated.String)
			if err != nil {
				return Operation{}, err
			}
			m.Coordination = &Coordination{Phase: coordinationPhase.String, UpdatedAt: updated}
		}
		op.Members = append(op.Members, m)
	}
	if err := rows.Err(); err != nil {
		return Operation{}, err
	}
	op.Requested = len(op.Members)
	return op, nil
}

var _ Store = (*SQLiteStore)(nil)
