package registry

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

type CreateStage string

const (
	CreateStageAdmission CreateStage = "worker_admission"
	CreateStageSource    CreateStage = "source_preparation"
	CreateStagePreparing CreateStage = "sandbox_preparation"
	CreateStageVM        CreateStage = "vm_start"
	CreateStageNetwork   CreateStage = "guest_network"
	CreateStageAgent     CreateStage = "guest_agent"
	CreateStageIdentity  CreateStage = "guest_identity"
	CreateStageReady     CreateStage = "ready"
	CreateStageAllocated CreateStage = "allocated"
)

var (
	ErrCreateProgressClosed = errors.New("create progress is closed")
	ErrCreateProgressStale  = errors.New("create progress attempt is stale")
)

type CreateStageMark struct {
	Stage       CreateStage
	Attempt     int64
	StartedAt   time.Time
	CompletedAt *time.Time
}

type CreateProgressOwner struct {
	CoordinatorID string
	OperationID   string
}

type CreateProgressTarget string

const (
	CreateProgressLocal   CreateProgressTarget = "local"
	CreateProgressGateway CreateProgressTarget = "gateway"
)

type CreateProgress struct {
	ID            string
	RegistryID    string
	RequestHash   [32]byte
	Attempt       int64
	Sequence      int64
	Current       CreateStageMark
	LastCompleted *CreateStageMark
	Condition     string
	ObservedAt    time.Time
	Outcome       *CreateRequestResult
	Owner         CreateProgressOwner
	Target        CreateProgressTarget
}

const ownedCreateProgressPredicate = `sequence>acknowledged
 AND json_extract(snapshot,'$.Owner.CoordinatorID')!=''
 AND json_extract(snapshot,'$.Owner.OperationID')!=''
 AND json_extract(snapshot,'$.Target') IN ('local','gateway')`

func validateCreateProgressOwner(intent CreateIntent) error {
	if intent.ProgressOwner == (CreateProgressOwner{}) && intent.ProgressTarget == "" {
		return nil
	}
	if intent.ProgressOwner.CoordinatorID == "" || intent.ProgressOwner.OperationID == "" ||
		(intent.ProgressTarget != CreateProgressLocal && intent.ProgressTarget != CreateProgressGateway) {
		return errors.New("create progress requires both owner ids and a local or gateway target")
	}
	return nil
}

func matchesCreateProgressOwner(p CreateProgress, intent CreateIntent) bool {
	return p.Owner == intent.ProgressOwner && p.Target == intent.ProgressTarget
}

func (r *Registry) migrateCreateProgress() error {
	_, err := r.db.Exec(`CREATE TABLE IF NOT EXISTS create_progress (
 id TEXT PRIMARY KEY, request_hash BLOB NOT NULL CHECK(length(request_hash)=32),
 attempt INTEGER NOT NULL CHECK(attempt>0), sequence INTEGER NOT NULL CHECK(sequence>0),
 acknowledged INTEGER NOT NULL DEFAULT 0 CHECK(acknowledged>=0 AND acknowledged<=sequence),
 snapshot BLOB NOT NULL);
 CREATE INDEX IF NOT EXISTS create_progress_pending ON create_progress(sequence,id) WHERE sequence>acknowledged;
 CREATE INDEX IF NOT EXISTS create_progress_owned_pending ON create_progress(id) WHERE ` + ownedCreateProgressPredicate + `;
 CREATE INDEX IF NOT EXISTS create_progress_inline_terminal ON create_progress(id) WHERE ` + inlineTerminalCreateProgressPredicate)
	return err
}

func progressForIntent(ctx context.Context, tx *sql.Tx, intent CreateIntent) (*CreateProgress, error) {
	if err := validateCreateProgressOwner(intent); err != nil {
		return nil, err
	}
	p, err := readCreateProgress(tx.QueryRowContext(ctx, createProgressSelect+` WHERE p.id=?`, intent.ID))
	if errors.Is(err, sql.ErrNoRows) {
		if intent.ProgressAttempt != 0 || intent.ProgressOwner != (CreateProgressOwner{}) {
			return nil, ErrCreateProgressStale
		}
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if p.RequestHash != intent.Hash || !matchesCreateProgressOwner(p, intent) {
		return nil, ErrCreateRequestConflict
	}
	if p.Attempt != intent.ProgressAttempt {
		return nil, ErrCreateProgressStale
	}
	return &p, nil
}

func writeCreateProgress(ctx context.Context, tx *sql.Tx, p *CreateProgress) error {
	p.Sequence++
	p.ObservedAt = time.Now().UTC()
	stored := *p
	if stored.Condition == "succeeded" || stored.Condition == "failed" {
		stored.Outcome = nil
	}
	body, err := json.Marshal(&stored)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(ctx, `UPDATE create_progress SET attempt=?,sequence=?,snapshot=? WHERE id=?`, p.Attempt, p.Sequence, body, p.ID)
	return err
}

func (r *Registry) BeginCreateProgress(ctx context.Context, intent CreateIntent) (CreateProgress, error) {
	if err := validateCreateProgressOwner(intent); err != nil {
		return CreateProgress{}, err
	}
	if intent.ID == "" || len(intent.ID) > 255 || intent.Hash == ([32]byte{}) {
		return CreateProgress{}, errors.New("create progress requires a request id and hash")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return CreateProgress{}, err
	}
	defer tx.Rollback()
	p, err := readCreateProgress(tx.QueryRowContext(ctx, createProgressSelect+` WHERE p.id=?`, intent.ID))
	exists := err == nil
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return CreateProgress{}, err
	}
	if exists && (p.RequestHash != intent.Hash || !matchesCreateProgressOwner(p, intent)) {
		return CreateProgress{}, ErrCreateRequestConflict
	}
	var closed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM create_requests WHERE id=?)`, intent.ID).Scan(&closed); err != nil {
		return CreateProgress{}, err
	}
	if closed {
		return CreateProgress{}, ErrCreateProgressClosed
	}
	now := time.Now().UTC()
	if !exists {
		p = CreateProgress{ID: intent.ID, RegistryID: r.registryID, RequestHash: intent.Hash, Owner: intent.ProgressOwner, Target: intent.ProgressTarget}
	}
	p.Attempt++
	p.Current = CreateStageMark{Stage: CreateStageAdmission, Attempt: p.Attempt, StartedAt: now}
	p.Condition = "active"
	p.Outcome = nil
	if exists {
		err = writeCreateProgress(ctx, tx, &p)
	} else {
		p.Sequence = 1
		p.ObservedAt = now
		var body []byte
		body, err = json.Marshal(p)
		if err == nil {
			_, err = tx.ExecContext(ctx, `INSERT INTO create_progress(id,request_hash,attempt,sequence,snapshot) VALUES(?,?,?,?,?)`, p.ID, p.RequestHash[:], p.Attempt, p.Sequence, body)
		}
	}
	if err != nil {
		return CreateProgress{}, err
	}
	if err := tx.Commit(); err != nil {
		return CreateProgress{}, err
	}
	return p, nil
}

func (r *Registry) AdvanceCreateProgress(ctx context.Context, intent CreateIntent, stage CreateStage) error {
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	p, err := progressForIntent(ctx, tx, intent)
	if err != nil || p == nil {
		return err
	}
	if p.Condition != "active" {
		return ErrCreateProgressClosed
	}
	if p.Current.Stage == stage {
		return nil
	}
	allowed := false
	switch p.Current.Stage {
	case CreateStageAdmission:
		allowed = stage == CreateStageSource
	case CreateStagePreparing:
		allowed = stage == CreateStageVM
	case CreateStageVM:
		allowed = stage == CreateStageNetwork || stage == CreateStageAgent
	case CreateStageNetwork:
		allowed = stage == CreateStageAgent
	case CreateStageAgent:
		allowed = stage == CreateStageIdentity
	}
	if !allowed {
		return fmt.Errorf("invalid create stage transition %s to %s", p.Current.Stage, stage)
	}
	now := time.Now().UTC()
	completed := p.Current
	completed.CompletedAt = &now
	p.LastCompleted = &completed
	p.Current = CreateStageMark{Stage: stage, Attempt: p.Attempt, StartedAt: now}
	if err := writeCreateProgress(ctx, tx, p); err != nil {
		return err
	}
	return tx.Commit()
}

func (r *Registry) CreateProgress(ctx context.Context, id string) (CreateProgress, error) {
	return readCreateProgress(r.rdb.QueryRowContext(ctx, createProgressSelect+` WHERE p.id=?`, id))
}

func (r *Registry) PendingCreateProgress(ctx context.Context, limit int) ([]CreateProgress, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("create progress limit must be between 1 and 1000")
	}
	return r.pendingCreateProgress(ctx, createProgressSelect+` WHERE sequence>acknowledged ORDER BY sequence,p.id LIMIT ?`, limit)
}

// PendingOwnedCreateProgress pages by request ID so a rejected observation cannot
// monopolize delivery. Callers wrap to the empty cursor after an empty page.
func (r *Registry) PendingOwnedCreateProgress(ctx context.Context, afterID string, limit int) ([]CreateProgress, error) {
	if limit < 1 || limit > 1000 {
		return nil, errors.New("create progress limit must be between 1 and 1000")
	}
	return r.pendingCreateProgress(ctx, createProgressSelect+` WHERE `+ownedCreateProgressPredicate+` AND p.id>? ORDER BY p.id LIMIT ?`, afterID, limit)
}

func (r *Registry) pendingCreateProgress(ctx context.Context, query string, args ...any) ([]CreateProgress, error) {
	rows, err := r.rdb.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make([]CreateProgress, 0)
	for rows.Next() {
		p, err := readCreateProgress(rows)
		if err != nil {
			return nil, err
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (r *Registry) AcknowledgeCreateProgress(ctx context.Context, id string, sequence int64) error {
	if sequence < 1 {
		return errors.New("create progress acknowledgement must be positive")
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var current int64
	if err := tx.QueryRowContext(ctx, `SELECT sequence FROM create_progress WHERE id=?`, id).Scan(&current); err != nil {
		return err
	}
	if sequence > current {
		return errors.New("create progress acknowledgement exceeds current sequence")
	}
	if _, err := tx.ExecContext(ctx, `UPDATE create_progress SET acknowledged=MAX(acknowledged,?) WHERE id=?`, sequence, id); err != nil {
		return err
	}
	return tx.Commit()
}

func allocateCreateProgress(ctx context.Context, tx *sql.Tx, intent CreateIntent) error {
	p, err := progressForIntent(ctx, tx, intent)
	if err != nil || p == nil {
		return err
	}
	if p.Condition != "active" {
		return ErrCreateProgressClosed
	}
	if p.Current.Stage != CreateStageSource {
		return fmt.Errorf("cannot allocate create from stage %s", p.Current.Stage)
	}
	now := time.Now().UTC()
	p.LastCompleted = &CreateStageMark{Stage: CreateStageAllocated, Attempt: p.Attempt, StartedAt: now, CompletedAt: &now}
	p.Current = CreateStageMark{Stage: CreateStagePreparing, Attempt: p.Attempt, StartedAt: now}
	return writeCreateProgress(ctx, tx, p)
}

func terminalCreateProgress(ctx context.Context, tx *sql.Tx, requestID string) error {
	var hash, body []byte
	var outcome CreateRequestResult
	err := tx.QueryRowContext(ctx, `SELECT request_hash,phase,result,failure_status,failure_code,failure_detail FROM create_requests WHERE id=?`, requestID).Scan(&hash, &outcome.Phase, &body, &outcome.Status, &outcome.Code, &outcome.Detail)
	if err != nil {
		return err
	}
	if outcome.Phase != "succeeded" && outcome.Phase != "failed" {
		return nil
	}
	p, err := readCreateProgress(tx.QueryRowContext(ctx, createProgressSelect+` WHERE p.id=?`, requestID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if string(hash) != string(p.RequestHash[:]) {
		return ErrCreateRequestConflict
	}
	if p.Outcome != nil {
		return nil
	}
	if outcome.Phase == "succeeded" {
		if p.Condition != "active" {
			return ErrCreateProgressClosed
		}
		var sb Sandbox
		if err := json.Unmarshal(body, &sb); err != nil {
			return err
		}
		outcome.Sandbox = &sb
		now := time.Now().UTC()
		p.Current = CreateStageMark{Stage: CreateStageReady, Attempt: p.Attempt, StartedAt: now, CompletedAt: &now}
		completed := p.Current
		p.LastCompleted = &completed
	}
	p.Condition = outcome.Phase
	p.Outcome = &outcome
	return writeCreateProgress(ctx, tx, &p)
}

func terminalSandboxCreateProgress(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	var requestID string
	err := tx.QueryRowContext(ctx, `SELECT id FROM create_requests WHERE sandbox_id=?`, sandboxID).Scan(&requestID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return terminalCreateProgress(ctx, tx, requestID)
}

func failAllocatedCreateRequest(ctx context.Context, tx *sql.Tx, sandboxID string) error {
	if _, err := tx.ExecContext(ctx, `UPDATE create_requests SET phase='failed',
 failure_status=CASE WHEN failure_status=0 THEN 500 ELSE failure_status END,
 failure_code=CASE WHEN failure_status=0 THEN 'create_interrupted' ELSE failure_code END,
 failure_detail=CASE WHEN failure_status=0 THEN 'Sandbox creation was interrupted.' ELSE failure_detail END,
 completed_at=? WHERE sandbox_id=? AND phase='allocated'`, time.Now().UnixMilli(), sandboxID); err != nil {
		return err
	}
	return terminalSandboxCreateProgress(ctx, tx, sandboxID)
}
