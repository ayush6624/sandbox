package createops

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/ayush6624/sandbox/internal/registry"
)

var ErrProgressConflict = errors.New("create progress conflicts with coordinator state")

// IngestProgress acknowledges only the incoming sequence, after its transaction
// commits. Terminal success still needs Execute to confirm current routability.
func (s *SQLiteStore) IngestProgress(ctx context.Context, worker Worker, p registry.CreateProgress) (int64, error) {
	if p.Owner.CoordinatorID != s.coordinatorID || p.Owner.OperationID == "" {
		return 0, fmt.Errorf("%w: wrong owner", ErrProgressConflict)
	}
	if err := validateProgress(p); err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var spec, previous []byte
	var host, reg sql.NullString
	err = tx.QueryRowContext(ctx, `SELECT spec,worker_host,worker_registry,progress FROM create_members WHERE operation_id=? AND member_id=?`, p.Owner.OperationID, p.ID).Scan(&spec, &host, &reg, &previous)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, ErrNotFound
	}
	if err != nil {
		return 0, err
	}
	if !host.Valid || !reg.Valid || worker.HostID != host.String || worker.RegistryID != reg.String || p.RegistryID != reg.String {
		return 0, fmt.Errorf("%w: wrong assigned worker", ErrProgressConflict)
	}
	if p.RequestHash != sha256.Sum256(spec) {
		return 0, fmt.Errorf("%w: wrong request hash", ErrProgressConflict)
	}
	if previous != nil {
		var retained registry.CreateProgress
		if err := json.Unmarshal(previous, &retained); err != nil {
			return 0, err
		}
		if p.Target != retained.Target {
			return 0, fmt.Errorf("%w: changed target", ErrProgressConflict)
		}
		if p.Sequence < retained.Sequence {
			return p.Sequence, tx.Commit()
		}
		if p.Sequence == retained.Sequence {
			if !reflect.DeepEqual(p, retained) {
				return 0, fmt.Errorf("%w: changed observation at retained sequence", ErrProgressConflict)
			}
			return p.Sequence, tx.Commit()
		}
		if retained.Outcome != nil || p.Attempt < retained.Attempt {
			return 0, fmt.Errorf("%w: closed observation or decreasing attempt", ErrProgressConflict)
		}
	}
	body, err := json.Marshal(p)
	if err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE create_members SET progress=? WHERE operation_id=? AND member_id=?`, body, p.Owner.OperationID, p.ID); err != nil {
		return 0, err
	}
	if p.Condition == "failed" {
		failure := &Failure{Status: p.Outcome.Status, Code: p.Outcome.Code, Detail: p.Outcome.Detail}
		if _, err = s.record(ctx, tx, p.Owner.OperationID, []Outcome{{ID: p.ID, Failure: failure}}); err != nil {
			return 0, err
		}
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return p.Sequence, nil
}

func validateProgress(p registry.CreateProgress) error {
	invalid := func() error { return fmt.Errorf("%w: invalid observation", ErrProgressConflict) }
	if p.ID == "" || p.RegistryID == "" || p.Sequence < 1 || p.Attempt < 1 || p.Sequence < p.Attempt || p.ObservedAt.IsZero() || (p.Target != registry.CreateProgressLocal && p.Target != registry.CreateProgressGateway) {
		return invalid()
	}
	validMark := func(m registry.CreateStageMark) bool {
		switch m.Stage {
		case registry.CreateStageAdmission, registry.CreateStageSource, registry.CreateStagePreparing, registry.CreateStageVM, registry.CreateStageNetwork, registry.CreateStageAgent, registry.CreateStageIdentity, registry.CreateStageReady, registry.CreateStageAllocated:
		default:
			return false
		}
		return m.Attempt > 0 && m.Attempt <= p.Attempt && !m.StartedAt.IsZero() && (m.CompletedAt == nil || !m.CompletedAt.IsZero())
	}
	if !validMark(p.Current) || p.Current.Attempt != p.Attempt || p.Current.Stage == registry.CreateStageAllocated {
		return invalid()
	}
	if p.LastCompleted != nil && (!validMark(*p.LastCompleted) || p.LastCompleted.CompletedAt == nil) {
		return invalid()
	}
	switch p.Condition {
	case "active", "tearing_down":
		if p.Outcome != nil || p.Current.CompletedAt != nil || p.Current.Stage == registry.CreateStageReady {
			return invalid()
		}
	case "succeeded":
		if p.Outcome == nil || p.Outcome.Phase != p.Condition || p.Outcome.Sandbox == nil || p.Outcome.Sandbox.ID == "" || p.Outcome.Code != "" || p.Outcome.Detail != "" || p.Current.Stage != registry.CreateStageReady || p.Current.CompletedAt == nil || p.LastCompleted == nil || !reflect.DeepEqual(p.Current, *p.LastCompleted) {
			return invalid()
		}
	case "failed":
		if p.Outcome == nil || p.Outcome.Phase != p.Condition || p.Outcome.Sandbox != nil || p.Outcome.Status < 400 || p.Outcome.Status > 599 || p.Outcome.Code == "" || p.Current.Stage == registry.CreateStageReady || p.Current.CompletedAt != nil {
			return invalid()
		}
	default:
		return invalid()
	}
	return nil
}
