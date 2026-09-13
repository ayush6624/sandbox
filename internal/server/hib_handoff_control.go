package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

const (
	handoffControlVersion = 1
	handoffLocal          = "local"
	handoffOffered        = "offered"
	handoffClaimed        = "claimed"
	handoffRunning        = "running"
	handoffDestroyed      = "destroyed"
)

type handoffControl struct {
	Version      int                  `json:"version"`
	Generation   string               `json:"generation"`
	Phase        string               `json:"phase"`
	SourceHostID string               `json:"source_host_id"`
	HostID       string               `json:"host_id,omitempty"`
	RegistryID   string               `json:"registry_id,omitempty"`
	ClaimID      string               `json:"claim_id,omitempty"`
	Record       hibRecord            `json:"record"`
	Descriptor   *retainedHibernation `json:"descriptor,omitempty"`
}

type handoffClaim struct {
	Control  handoffControl
	Revision int64
}

func handoffControlObj(id string) string { return "handoff/" + id + "/control.json" }

func decodeHandoff(data []byte, id string) (*handoffControl, error) {
	var c handoffControl
	if err := json.Unmarshal(data, &c); err != nil {
		return nil, err
	}
	if c.Version != handoffControlVersion || !validHibPeerGeneration(c.Generation) || c.Record.ID != id || c.SourceHostID == "" {
		return nil, errors.New("invalid handoff identity")
	}
	if c.Record.Generation != "" && c.Record.Generation != c.Generation {
		return nil, errors.New("handoff record generation mismatch")
	}
	switch c.Phase {
	case handoffLocal:
		if c.HostID == "" || c.RegistryID == "" || c.ClaimID != "" {
			return nil, errors.New("invalid local handoff owner")
		}
	case handoffOffered:
		if c.ClaimID != "" {
			return nil, errors.New("offered handoff has a claim")
		}
	case handoffClaimed, handoffRunning:
		if c.HostID == "" || c.RegistryID == "" || !validHibPeerGeneration(c.ClaimID) {
			return nil, errors.New("invalid handoff claim")
		}
	case handoffDestroyed:
	default:
		return nil, fmt.Errorf("unknown handoff phase %q", c.Phase)
	}
	if c.Descriptor != nil {
		if c.Descriptor.Record.ID != id || c.Descriptor.Ref.Generation != c.Generation {
			return nil, errors.New("handoff descriptor identity mismatch")
		}
		record, _ := json.Marshal(c.Record)
		described, _ := json.Marshal(c.Descriptor.Record)
		if !bytes.Equal(record, described) {
			return nil, errors.New("handoff descriptor record mismatch")
		}
		if err := c.Descriptor.validateStorage(); err != nil {
			return nil, err
		}
		digest, err := hibPeerManifestDigest(&c.Descriptor.Manifest)
		if err != nil || digest != c.Descriptor.Ref.ManifestSHA256 {
			return nil, errors.New("handoff descriptor manifest mismatch")
		}
	}
	return &c, nil
}

// Absence is nil,0,nil. A read error never licenses the legacy adoption path.
func (s *Server) readHandoff(ctx context.Context, id string) (*handoffControl, int64, error) {
	if s.blob == nil {
		return nil, 0, nil
	}
	data, revision, err := s.blob.GetBytesGen(ctx, handoffControlObj(id))
	if errors.Is(err, gcsblob.ErrNotExist) {
		return nil, 0, nil
	}
	if err != nil {
		return nil, 0, err
	}
	control, err := decodeHandoff(data, id)
	return control, revision, err
}

func (s *Server) ownsHandoff(c *handoffControl) bool {
	return c != nil && c.HostID == s.hostID() && c.RegistryID == s.reg.RegistryID()
}

func sameHandoff(a, b *handoffControl) bool {
	left, _ := json.Marshal(a)
	right, _ := json.Marshal(b)
	return bytes.Equal(left, right)
}

func (s *Server) casHandoff(ctx context.Context, next handoffControl, revision int64) (int64, error) {
	data, err := json.Marshal(next)
	if err != nil {
		return 0, err
	}
	if _, err := decodeHandoff(data, next.Record.ID); err != nil {
		return 0, err
	}
	written, err := s.blob.PutBytesIfGenerationMatch(ctx, handoffControlObj(next.Record.ID), data, revision)
	if err == nil {
		return written, nil
	}
	// A lost response can hide our successful write; recognize only this exact
	// transition. Reading someone else's newer claim never grants permission.
	current, currentRevision, readErr := s.readHandoff(ctx, next.Record.ID)
	if readErr == nil && current != nil && sameHandoff(current, &next) {
		return currentRevision, nil
	}
	if errors.Is(err, gcsblob.ErrPreconditionFailed) {
		return 0, ErrOwnerContended
	}
	return 0, err
}

// Reserve before freezing so a stale legacy-record adopter competes on the
// same control key. Existing live rights are retained, never refreshed/stolen.
func (s *Server) reserveLocalHandoff(ctx context.Context, id string) error {
	if s.blob == nil {
		return errors.New("handoff requires a snapshot bucket")
	}
	current, _, err := s.readHandoff(ctx, id)
	if err != nil {
		return err
	}
	if current != nil {
		if s.ownsHandoff(current) && (current.Phase == handoffLocal || current.Phase == handoffRunning) {
			return nil
		}
		return ErrOwnerContended
	}
	local := handoffControl{Version: handoffControlVersion, Generation: uuid.NewString(), Phase: handoffLocal, SourceHostID: s.hostID(), HostID: s.hostID(), RegistryID: s.reg.RegistryID(), Record: hibRecord{Version: hibRecordVersion, ID: id}}
	_, err = s.casHandoff(ctx, local, 0)
	return err
}

func (s *Server) prepareHandoffOffer(ctx context.Context, id string, desc *retainedHibernation) (registry.HibernationHandoff, error) {
	if desc == nil || desc.Record.ID != id || !validHibPeerGeneration(desc.Ref.Generation) {
		return registry.HibernationHandoff{}, errors.New("invalid handoff descriptor")
	}
	current, revision, err := s.readHandoff(ctx, id)
	if err != nil {
		return registry.HibernationHandoff{}, err
	}
	if !s.ownsHandoff(current) || (current.Phase != handoffLocal && current.Phase != handoffRunning) {
		return registry.HibernationHandoff{}, ErrOwnerContended
	}
	offer := handoffControl{Version: handoffControlVersion, Generation: desc.Ref.Generation, Phase: handoffOffered, SourceHostID: s.hostID(), HostID: s.hostID(), RegistryID: s.reg.RegistryID(), Record: desc.Record, Descriptor: desc}
	data, err := json.Marshal(offer)
	if err != nil {
		return registry.HibernationHandoff{}, err
	}
	return registry.HibernationHandoff{Generation: offer.Generation, SandboxID: id, Offer: data, ExpectedRevision: revision}, nil
}

// Replays only the journal's original offer. Backup completion never calls this
// with a newly manufactured offer or changes a running generation's authority.
func (s *Server) publishHandoffOffer(ctx context.Context, job registry.HibernationHandoff) error {
	offer, err := decodeHandoff(job.Offer, job.SandboxID)
	if err != nil {
		return err
	}
	if offer.Generation != job.Generation || offer.Phase != handoffOffered || offer.SourceHostID != s.hostID() {
		return errors.New("invalid journal handoff offer")
	}
	current, revision, err := s.readHandoff(ctx, job.SandboxID)
	if err != nil {
		return err
	}
	// Published settles this journal's publication obligation. A replaced control
	// also settles it: the old expected revision can never publish again, and
	// retaining that failed obligation would pin a completed backup forever.
	// GCS generations are opaque unique tokens, not an ordered counter.
	if current != nil && (current.Generation == job.Generation || revision != job.ExpectedRevision) {
		if current.Generation != job.Generation || current.Phase == handoffDestroyed {
			if err := s.retireHandoffStorage(ctx, offer.Descriptor); err != nil {
				return err
			}
		}
		return s.reg.MarkHibernationHandoffPublished(ctx, job.Generation)
	}
	if offer.Descriptor != nil {
		if err := s.ensureHandoffStorage(ctx, offer.Descriptor); err != nil {
			return err
		}
	}
	if err := s.queueReplacedHandoff(ctx, current, revision); err != nil {
		return err
	}
	if _, err := s.casHandoff(ctx, *offer, job.ExpectedRevision); err != nil {
		return err
	}
	s.retireReplacedHandoff(ctx, current, offer.Generation)
	return s.reg.MarkHibernationHandoffPublished(ctx, job.Generation)
}

func (s *Server) claimHandoff(ctx context.Context, id string) (*handoffClaim, error) {
	if s.blob == nil {
		return nil, errors.New("handoff requires a snapshot bucket")
	}
	control, revision, err := s.readHandoff(ctx, id)
	if err != nil {
		return nil, err
	}
	if control == nil {
		rec, err := s.fetchHibRecord(ctx, id)
		if err != nil {
			return nil, err
		}
		control = &handoffControl{Version: handoffControlVersion, Generation: uuid.NewString(), Phase: handoffOffered, SourceHostID: s.hostID(), Record: *rec}
	}
	// A canceled request may have committed its claim without receiving the
	// response. CLAIMED cannot execute a VM; resume only this registry's claim.
	if control.Phase == handoffClaimed && s.ownsHandoff(control) {
		return &handoffClaim{Control: *control, Revision: revision}, nil
	}
	return s.claimOfferedHandoff(ctx, control, revision)
}

func (s *Server) claimOfferedHandoff(ctx context.Context, control *handoffControl, revision int64) (*handoffClaim, error) {
	if control.Phase != handoffOffered {
		return nil, ErrOwnerContended
	}
	next := *control
	next.Phase, next.HostID, next.RegistryID, next.ClaimID = handoffClaimed, s.hostID(), s.reg.RegistryID(), uuid.NewString()
	written, err := s.casHandoff(ctx, next, revision)
	if err != nil {
		return nil, err
	}
	return &handoffClaim{Control: next, Revision: written}, nil
}

// Local rights are sufficient for a normal wake; an offered generation still
// requires the same exclusive claim as a remote adoption.
func (s *Server) claimLocalHandoff(ctx context.Context, id string) (*handoffClaim, error) {
	control, revision, err := s.readHandoff(ctx, id)
	if err != nil {
		return nil, err
	}
	if control == nil {
		return nil, nil
	}
	if s.ownsHandoff(control) && (control.Phase == handoffLocal || control.Phase == handoffRunning) {
		return nil, nil
	}
	if control.Phase == handoffClaimed && s.ownsHandoff(control) {
		return &handoffClaim{Control: *control, Revision: revision}, nil
	}
	if control.Phase != handoffOffered || !s.ownsHandoff(control) {
		return nil, ErrOwnerContended
	}
	return s.claimOfferedHandoff(ctx, control, revision)
}

// Authorization is committed before VM execution. No lease expiration or later
// reader can overwrite these rights while the guest may still be executing.
func (s *Server) authorizeHandoffRun(ctx context.Context, claim *handoffClaim) error {
	if claim == nil {
		return nil
	}
	if !s.ownsHandoff(&claim.Control) || (claim.Control.Phase != handoffClaimed && claim.Control.Phase != handoffRunning) {
		return ErrOwnerContended
	}
	next := claim.Control
	next.Phase = handoffRunning
	revision, err := s.casHandoff(ctx, next, claim.Revision)
	if err != nil {
		return err
	}
	claim.Control, claim.Revision = next, revision
	return nil
}

// Caller must have stopped the attempt's VM before reopening. This is not a
// timeout-based takeover; only the exact failed claimant can perform it.
func (s *Server) reopenHandoff(ctx context.Context, claim *handoffClaim) error {
	if claim == nil {
		return nil
	}
	if !s.ownsHandoff(&claim.Control) || (claim.Control.Phase != handoffClaimed && claim.Control.Phase != handoffRunning) {
		return ErrOwnerContended
	}
	next := claim.Control
	next.Phase, next.ClaimID = handoffOffered, ""
	revision, err := s.casHandoff(ctx, next, claim.Revision)
	if err != nil {
		return err
	}
	claim.Control, claim.Revision = next, revision
	return nil
}

func (s *Server) destroyHandoff(ctx context.Context, id string) error {
	if s.blob == nil {
		return nil
	}
	current, revision, err := s.readHandoff(ctx, id)
	if err != nil {
		return err
	}
	if current == nil {
		return nil
	}
	if current.Phase == handoffDestroyed {
		return s.retireHandoffStorage(ctx, current.Descriptor)
	}
	if !s.ownsHandoff(current) {
		return ErrOwnerContended
	}
	next := *current
	next.Phase, next.ClaimID = handoffDestroyed, ""
	if err := s.queueReplacedHandoff(ctx, current, revision); err != nil {
		return err
	}
	_, err = s.casHandoff(ctx, next, revision)
	if err == nil {
		err = s.retireHandoffStorage(ctx, next.Descriptor)
	}
	return err
}

// Ordinary durable publication for a managed local guest creates a new offered
// generation. It cannot publish over a remote claim or a transferred source.
func (s *Server) publishLocalHibernation(ctx context.Context, rec *hibRecord) error {
	if rec == nil || rec.ID == "" || rec.Version != hibRecordVersion || rec.Generation != "" {
		return errors.New("ordinary hibernation must publish a new local checkpoint")
	}
	current, revision, err := s.readHandoff(ctx, rec.ID)
	if err != nil || current == nil {
		return err
	}
	if !s.ownsHandoff(current) || (current.Phase != handoffLocal && current.Phase != handoffRunning) {
		return ErrOwnerContended
	}
	next := handoffControl{Version: handoffControlVersion, Generation: uuid.NewString(), Phase: handoffOffered, SourceHostID: s.hostID(), HostID: s.hostID(), RegistryID: s.reg.RegistryID(), Record: *rec}
	if err := s.queueReplacedHandoff(ctx, current, revision); err != nil {
		return err
	}
	_, err = s.casHandoff(ctx, next, revision)
	if err == nil {
		s.retireReplacedHandoff(ctx, current, next.Generation)
	}
	return err
}
