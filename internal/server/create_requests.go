package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

func (s *Server) Place(ctx context.Context, _ createops.Spec, _ int) (createops.Placement, error) {
	if err := ctx.Err(); err != nil {
		return createops.Placement{}, err
	}
	return createops.Placement{Worker: createops.Worker{HostID: s.hostID(), RegistryID: s.reg.RegistryID(), CreateProgress: true}, Release: func([]createops.Outcome) {}}, nil
}

func (s *Server) handleCreateCommand(w http.ResponseWriter, r *http.Request) {
	var command createops.Command
	r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&command); err != nil {
		httpError(w, 400, err)
		return
	}
	worker := createops.Worker{HostID: s.hostID(), RegistryID: command.RegistryID}
	results, err := s.executeCreateCommand(r.Context(), worker, command, registry.CreateProgressGateway)
	if err != nil {
		httpError(w, 503, err)
		return
	}
	writeJSON(w, 200, results)
}

// Execute identifies each member independently; retries never infer identities
// from the order of a compact fanout response. The registry is replay authority.
func (s *Server) Execute(ctx context.Context, worker createops.Worker, command createops.Command) ([]createops.Outcome, error) {
	return s.executeCreateCommand(ctx, worker, command, registry.CreateProgressLocal)
}

func (s *Server) executeCreateCommand(ctx context.Context, worker createops.Worker, command createops.Command, target registry.CreateProgressTarget) ([]createops.Outcome, error) {
	if worker.RegistryID != s.reg.RegistryID() || command.RegistryID != s.reg.RegistryID() {
		return nil, errors.New("assigned worker registry no longer exists on this host")
	}
	if len(command.Members) < 1 || len(command.Members) > 8 {
		return nil, errors.New("create command must have between 1 and 8 members")
	}
	seen := make(map[string]bool, len(command.Members))
	for _, member := range command.Members {
		if member.ID == "" || len(member.ID) > 255 || seen[member.ID] {
			return nil, errors.New("create command member identities must be nonempty, unique, and at most 255 bytes")
		}
		seen[member.ID] = true
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Minute)
	defer cancel()
	results := make([]createops.Outcome, len(command.Members))
	errs := make([]error, len(command.Members))
	runBounded(8, len(command.Members), func(i int) {
		results[i], errs[i] = s.executeCreateMember(ctx, command.Members[i], command.SnapshotPeer, command.Owner, target)
	})
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}

func (s *Server) executeCreateMember(ctx context.Context, member createops.Member, peer string, owner *registry.CreateProgressOwner, target registry.CreateProgressTarget) (createops.Outcome, error) {
	encoded, err := json.Marshal(member.Spec)
	if err != nil {
		return createops.Outcome{}, err
	}
	intent := registry.CreateIntent{ID: member.ID, Hash: sha256.Sum256(encoded), SourceType: member.Spec.Source.Type, SourceID: member.Spec.Source.ID, Metadata: member.Spec.Metadata, DefaultVcpus: s.cfg.VMTemplate.Vcpus, DefaultMemMIB: s.cfg.VMTemplate.MemMIB}
	if owner != nil {
		intent.ProgressOwner = *owner
		intent.ProgressTarget = target
	}
	if intent.SourceType == "" {
		intent.SourceType = "default"
	}
	lock := s.createRequests.acquire(member.ID)
	lock.Lock()
	defer lock.Unlock()
	if err := ctx.Err(); err != nil {
		return createops.Outcome{}, err
	}
	result, err := s.reg.CreateRequest(ctx, intent)
	if err == nil {
		return s.createOutcome(ctx, member.ID, result)
	}
	if errors.Is(err, registry.ErrCreateRequestConflict) {
		return createops.Outcome{ID: member.ID, Failure: &createops.Failure{Status: 409, Code: "create_request_conflict", Detail: err.Error()}}, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return createops.Outcome{}, err
	}

	progress, progressErr := s.reg.BeginCreateProgress(ctx, intent)
	if errors.Is(progressErr, registry.ErrCreateRequestConflict) {
		return createops.Outcome{ID: member.ID, Failure: &createops.Failure{Status: 409, Code: "create_request_conflict", Detail: progressErr.Error()}}, nil
	}
	if progressErr != nil {
		return createops.Outcome{}, progressErr
	}
	intent.ProgressAttempt = progress.Attempt
	err = s.validateCreateSpec(member.Spec)
	status := http.StatusBadRequest
	code := "sandbox_create_failed"
	if err == nil {
		if err = s.acquireCreate(ctx); err != nil {
			return createops.Outcome{}, err
		}
		_, err = s.createRequested(ctx, member.Spec, intent, peer)
		s.releaseCreate()
		status = http.StatusInternalServerError
		if errors.Is(err, registry.ErrPoolExhausted) {
			status = http.StatusServiceUnavailable
		} else if errors.Is(err, errSnapshotNotFound) || errors.Is(err, errSnapshotDeleted) {
			status = http.StatusNotFound
			code = "source_not_found"
		}
	}
	if err != nil {
		// A cancelled response may have hidden an allocation. Read its durable
		// result below, using a short independent context to record preallocation
		// failure even when the network caller has already disconnected.
		recordCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ctx.Err() != nil {
			// Cancellation before allocation leaves no committed worker outcome.
			// The coordinator must retry the same assignment, not retain a failure
			// caused only by losing its HTTP connection.
			result, readErr := s.reg.CreateRequest(recordCtx, intent)
			if readErr == nil {
				return s.createOutcome(recordCtx, member.ID, result)
			}
			if errors.Is(readErr, sql.ErrNoRows) {
				return createops.Outcome{}, ctx.Err()
			}
			return createops.Outcome{}, readErr
		}
		detail := err.Error()
		if status == http.StatusNotFound {
			detail = "The requested snapshot or template source was not found."
		}
		if status >= 500 {
			log.Printf("create request %s: %v", member.ID, err)
			detail = "Sandbox creation failed."
			if status == http.StatusServiceUnavailable {
				detail = "Worker capacity is unavailable."
			}
		}
		if recordErr := s.reg.FailCreateRequest(recordCtx, intent, status, code, detail); recordErr != nil {
			if errors.Is(recordErr, registry.ErrCreateRequestConflict) {
				return createops.Outcome{ID: member.ID, Failure: &createops.Failure{Status: 409, Code: "create_request_conflict", Detail: recordErr.Error()}}, nil
			}
			return createops.Outcome{}, recordErr
		}
		ctx = recordCtx
	}
	result, err = s.reg.CreateRequest(ctx, intent)
	if err != nil {
		return createops.Outcome{}, err
	}
	return s.createOutcome(ctx, member.ID, result)
}

func (s *Server) createOutcome(ctx context.Context, id string, result registry.CreateRequestResult) (createops.Outcome, error) {
	switch result.Phase {
	case "succeeded":
		sb := s.effectiveResources(*result.Sandbox)
		current, err := s.reg.Get(ctx, sb.ID)
		if err != nil && !errors.Is(err, sql.ErrNoRows) {
			return createops.Outcome{}, err
		}
		routable := err == nil && (current.Status == registry.StatusRunning || current.Status == registry.StatusHibernated)
		return createops.Outcome{ID: id, Sandbox: &sb, Routable: routable}, nil
	case "failed":
		return createops.Outcome{ID: id, Failure: &createops.Failure{Status: result.Status, Code: result.Code, Detail: result.Detail}}, nil
	default:
		return createops.Outcome{}, fmt.Errorf("create request %s has an unresolved allocation", id)
	}
}

func (s *Server) validateCreateSpec(spec createops.Spec) error {
	if err := validateName(spec.Name); err != nil {
		return err
	}
	if spec.Lifecycle.TTLSeconds < 0 || spec.Lifecycle.IdleTimeoutSeconds < -1 {
		return errors.New("ttl_seconds must be nonnegative and idle_timeout_seconds must be >= -1")
	}
	if spec.Resources != nil {
		if err := s.validateResources(spec.Resources.VCPU, spec.Resources.MemoryMIB); err != nil {
			return err
		}
	}
	switch spec.Source.Type {
	case "", "default":
		if spec.Source.ID != "" {
			return errors.New("default source cannot have an id")
		}
	case "snapshot", "template":
		if spec.Source.ID == "" {
			return errors.New("snapshot or template source requires an id")
		}
		if spec.Resources != nil && (spec.Resources.VCPU > 0 || spec.Resources.MemoryMIB > 0) && !(spec.Source.Type == "template" && spec.Source.ID == "default") {
			return errors.New("snapshot resources cannot be overridden")
		}
	default:
		return errors.New("unknown create source type")
	}
	return nil
}

func (s *Server) createRequested(ctx context.Context, spec createops.Spec, intent registry.CreateIntent, peer string) (registry.Sandbox, error) {
	if err := s.reg.AdvanceCreateProgress(ctx, intent, registry.CreateStageSource); err != nil {
		return registry.Sandbox{}, fmt.Errorf("record source preparation: %w", err)
	}
	var expires *time.Time
	if spec.Lifecycle.TTLSeconds > 0 {
		value := time.Now().Add(time.Duration(spec.Lifecycle.TTLSeconds) * time.Second)
		expires = &value
	}
	idle := spec.Lifecycle.IdleTimeoutSeconds
	fromSnapshot := spec.Source.Type == "snapshot" || (spec.Source.Type == "template" && spec.Source.ID != "default")
	var snap *registry.Snapshot
	if fromSnapshot {
		op := s.snapshotLock(spec.Source.ID)
		op.RLock()
		defer op.RUnlock()
		value, err := s.ensureSnapshotLocalFrom(ctx, spec.Source.ID, peer)
		if err != nil {
			return registry.Sandbox{}, err
		}
		if value.Role == registry.SnapshotRoleBase || value.Golden {
			return registry.Sandbox{}, errors.New("internal base cannot be used as a public create source")
		}
		if value.MemPath, err = s.materializeMem(ctx, value); err != nil {
			return registry.Sandbox{}, err
		}
		snap = &value
	} else if spec.Resources == nil || (spec.Resources.VCPU == 0 && spec.Resources.MemoryMIB == 0) {
		snap = s.golden.Load()
	}
	if snap != nil {
		if ready, ok, err := s.claimWarmForTemplate(ctx, snap.ID, spec.Name, expires, idle, intent); err != nil {
			return registry.Sandbox{}, err
		} else if ok {
			return ready, nil
		}
		// Once this call allocates, any failure is terminal for this request. Do
		// not take the legacy hot-to-cold fallback and allocate a second sandbox.
		return s.createFromSnapshot(ctx, *snap, spec.Name, expires, idle, intent)
	}
	vcpus, mem := s.cfg.VMTemplate.Vcpus, s.cfg.VMTemplate.MemMIB
	if spec.Resources != nil {
		vcpus, mem = spec.Resources.VCPU, spec.Resources.MemoryMIB
	}
	return s.createCold(ctx, spec.Name, expires, idle, vcpus, mem, "", intent)
}
