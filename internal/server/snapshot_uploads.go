package server

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"time"
)

const snapshotUploadConcurrency = 2

func (s *Server) wakeSnapshotUploads() {
	select {
	case s.snapshotUploadWake <- struct{}{}:
	default:
	}
}

// SQLite owns pending work. The map holds only cancellation handles for the
// attempts running in this process; an abandoned uploading row is eligible again.
func (s *Server) runSnapshotUploads(ctx context.Context) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	defer func() {
		s.snapshotUpMu.Lock()
		active := make([]*backgroundUpload, 0, len(s.snapshotUploads))
		for _, up := range s.snapshotUploads {
			up.cancel()
			active = append(active, up)
		}
		s.snapshotUpMu.Unlock()
		for _, up := range active {
			<-up.done
		}
	}()
	for {
		if ctx.Err() != nil {
			return
		}
		ids, err := s.reg.DueSnapshotUploads(ctx, time.Now(), 2*snapshotUploadConcurrency)
		if err != nil && ctx.Err() == nil {
			fmt.Fprintf(os.Stderr, "snapshot upload queue: %v\n", err)
		}
		for _, id := range ids {
			if err := s.startSnapshotUpload(ctx, id); err != nil && ctx.Err() == nil {
				fmt.Fprintf(os.Stderr, "[snapshot %s] schedule upload: %v\n", id, err)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.snapshotUploadWake:
		}
	}
}

func (s *Server) startSnapshotUpload(parent context.Context, id string) error {
	op := s.snapshotLock(id)
	op.Lock()
	defer op.Unlock()
	if err := parent.Err(); err != nil {
		return err
	}
	s.snapshotUpMu.Lock()
	busy := s.snapshotUploads[id] != nil || len(s.snapshotUploads) >= snapshotUploadConcurrency
	s.snapshotUpMu.Unlock()
	if busy {
		return nil
	}
	snap, err := s.reg.GetSnapshot(parent, id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // deletion won after the due-work query
	}
	if err != nil {
		return err
	}
	job, err := s.reg.BeginSnapshotUpload(parent, id, time.Now())
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, uploadTimeout)
	up := &backgroundUpload{cancel: cancel, done: make(chan struct{})}
	s.snapshotUpMu.Lock()
	s.snapshotUploads[id] = up
	s.snapshotUpMu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			s.snapshotUpMu.Lock()
			delete(s.snapshotUploads, id)
			close(up.done)
			s.snapshotUpMu.Unlock()
			s.wakeSnapshotUploads()
		}()
		err := s.uploadSnapshot(ctx, snap)
		if err == nil {
			err = s.reg.CompleteSnapshotUpload(ctx, id)
		}
		if err == nil || ctx.Err() == context.Canceled {
			return
		}
		fmt.Fprintf(os.Stderr, "[snapshot %s] upload attempt %d: %v\n", id, job.Attempts, err)
		// An attempt deadline must not prevent recording the next attempt.
		recordCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
		defer stop()
		var recordErr error
		switch {
		case errors.Is(err, errSnapshotDeleted):
			recordErr = s.reg.FailSnapshotUpload(recordCtx, id, "Snapshot was deleted from object storage.")
		case errors.Is(err, errSnapshotArtifacts):
			recordErr = s.reg.FailSnapshotUpload(recordCtx, id, "Local snapshot artifacts are missing or invalid.")
		default:
			delay := min(time.Second<<min(max(job.Attempts-1, 0), 6), time.Minute)
			delay = delay*3/4 + time.Duration(rand.Int64N(int64(delay/4)+1))
			recordErr = s.reg.RetrySnapshotUpload(recordCtx, id, time.Now().Add(delay), "Snapshot upload failed; retry scheduled.")
		}
		if recordErr != nil {
			fmt.Fprintf(os.Stderr, "[snapshot %s] record upload attempt: %v\n", id, recordErr)
		}
	}()
	return nil
}

func (s *Server) cancelSnapshotUpload(id string) {
	s.snapshotUpMu.Lock()
	up := s.snapshotUploads[id]
	s.snapshotUpMu.Unlock()
	if up != nil {
		up.cancel()
		<-up.done
	}
}
