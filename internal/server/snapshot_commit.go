package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
)

var (
	errSnapshotDeleted   = errors.New("snapshot deleted")
	errSnapshotArtifacts = errors.New("snapshot artifacts unavailable")
)

// Both variants occupy meta.json, so create-only publication cannot overwrite
// a deletion even when the deleting worker differs from the capturing worker.
type snapshotCommit struct {
	registry.Snapshot
	DeletedAt *time.Time `json:"deleted_at,omitempty"`
}

func decodeSnapshotCommit(data []byte, id string) (registry.Snapshot, error) {
	var commit snapshotCommit
	if err := json.Unmarshal(data, &commit); err != nil {
		return registry.Snapshot{}, fmt.Errorf("decode snapshot commit: %w", err)
	}
	if commit.ID != id {
		return registry.Snapshot{}, errors.New("snapshot commit identity mismatch")
	}
	if commit.DeletedAt != nil {
		return registry.Snapshot{}, errSnapshotDeleted
	}
	if commit.MemPath == "" || commit.StatePath == "" || commit.RootfsPath == "" {
		return registry.Snapshot{}, errors.New("incomplete snapshot commit")
	}
	commit.Upload = nil
	return commit.Snapshot, nil
}

func (s *Server) snapshotCommitted(ctx context.Context, id string) (bool, error) {
	data, err := s.blob.GetBytes(ctx, snapObj(id, "meta.json"))
	if errors.Is(err, gcsblob.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	_, err = decodeSnapshotCommit(data, id)
	return err == nil, err
}

func (s *Server) tombstoneSnapshot(ctx context.Context, id string) error {
	// An unconditional tombstone PUT is idempotent. Publishers only create an
	// absent key; whichever write lands first, the final state is deleted.
	now := time.Now().UTC()
	data, err := json.Marshal(snapshotCommit{Snapshot: registry.Snapshot{ID: id}, DeletedAt: &now})
	if err != nil {
		return err
	}
	return s.blob.PutBytes(ctx, snapObj(id, "meta.json"), data)
}
