package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/registry"
)

func handoffRecordObj(id, generation string) string {
	return "handoff/" + id + "/" + generation + "/record.json"
}
func handoffStateObj(id, generation string) string {
	return "handoff/" + id + "/" + generation + "/state.sz"
}
func handoffRootfsObj(id, generation string) string {
	return "handoff/" + id + "/" + generation + "/rootfs.sz"
}

// Every journal pins its source, including completed backups still serving a
// destination. Retirement waits until both consumers finish or safe expiry.
func (s *Server) pendingHandoffCount(ctx context.Context) (int, error) {
	jobs, err := s.reg.ListHibernationHandoffs(ctx)
	return len(jobs), err
}

// The persistent unfinished job pins these paths. Do not hold the generation
// read lock across upload: a waiting DELETE writer would block VM fault reads.
func (s *Server) backupHandoff(ctx context.Context, generation string) error {
	return s.reg.WithHibernationHandoff(ctx, generation, func(attempt *registry.HandoffAttempt) error {
		job, err := attempt.Job(ctx)
		if err != nil {
			return err
		}
		if !job.BackupComplete {
			if err := s.uploadHandoff(ctx, job); err != nil {
				if !errors.Is(err, errHandoffSourceMissing) {
					return err
				}
				if proofErr := s.verifyOwnedHandoffBackup(ctx, job); proofErr != nil {
					return errors.Join(err, proofErr)
				}
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := attempt.CompleteBackup(ctx); err != nil {
				return err
			}
		}
		return s.cleanupHandoffAttempt(ctx, attempt, generation, time.Now())
	})
}

func (s *Server) uploadHandoff(ctx context.Context, job registry.HibernationHandoff) error {
	generation := job.Generation
	op := s.snapshotLock("hib-peer:" + generation)
	op.RLock()
	d, err := s.readRetainedHibernation(generation)
	op.RUnlock()
	if err != nil {
		if os.IsNotExist(err) {
			return fmt.Errorf("%w: %v", errHandoffSourceMissing, err)
		}
		return err
	}
	if d.Record.ID != job.SandboxID || d.Record.Generation != generation {
		return errors.New("handoff journal identity mismatch")
	}
	if err := d.validateStorage(); err != nil {
		return err
	}
	expected, err := handoffJobDescriptor(job)
	if err != nil {
		return err
	}
	if d.Manifest.Storage != nil || expected != nil && expected.Manifest.Storage != nil {
		local, err := json.Marshal(d)
		if err != nil {
			return err
		}
		offered, err := json.Marshal(expected)
		if err != nil {
			return err
		}
		if !bytes.Equal(local, offered) {
			return errors.New("retained handoff descriptor differs from journal offer")
		}
	}
	dir := filepath.Join(s.hibPeerDir(), generation)
	if d.Manifest.Storage != nil {
		for _, name := range []string{"mem.bin", "state.bin", "rootfs.ext4"} {
			info, err := os.Stat(filepath.Join(dir, name))
			if os.IsNotExist(err) {
				return fmt.Errorf("%w: %v", errHandoffSourceMissing, err)
			}
			if err != nil {
				return err
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("handoff source %s is not a regular file", name)
			}
		}
		return s.backupOwnedHandoff(ctx, job, d, dir)
	}
	return s.backupLegacyHandoff(ctx, job, d, dir)
}

func (s *Server) backupLegacyHandoff(ctx context.Context, job registry.HibernationHandoff, d *retainedHibernation, dir string) error {
	f, err := os.Open(filepath.Join(dir, "mem.bin"))
	if err != nil {
		return err
	}
	defer f.Close()
	seen := make(map[string]bool)
	buf := make([]byte, min(d.Manifest.ChunkSize, d.Manifest.MemSize))
	for idx, entry := range d.Manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := buf[:d.Manifest.chunkLen(uint64(idx))]
		if _, err := io.ReadFull(f, raw); err != nil {
			return err
		}
		if entry.Hash == chunkZeroHash {
			if !allZero(raw) {
				return fmt.Errorf("zero handoff chunk %d changed", idx)
			}
			continue
		}
		if !chunkMatches(raw, uint64(len(raw)), entry.Hash) {
			return fmt.Errorf("handoff chunk %d failed content verification", idx)
		}
		if seen[entry.Hash] {
			continue
		}
		seen[entry.Hash] = true
		if s.chunkPresent(ctx, entry.Hash) {
			continue
		}
		compressed, err := gzipBytes(raw)
		if err != nil {
			return err
		}
		if err := s.blob.PutBytes(ctx, chunkObj(entry.Hash), compressed); err != nil {
			return err
		}
		s.markChunkUploaded(entry.Hash)
	}
	if _, err := s.blob.PutSparse(ctx, handoffStateObj(job.SandboxID, job.Generation), filepath.Join(dir, "state.bin")); err != nil {
		return err
	}
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if d.Record.RootfsForm == rootfsFormDiff {
		_, err = s.blob.PutRanges(ctx, handoffRootfsObj(job.SandboxID, job.Generation), rootfs, d.RootfsRanges)
	} else {
		_, err = s.blob.PutSparse(ctx, handoffRootfsObj(job.SandboxID, job.Generation), rootfs)
	}
	if err != nil {
		return err
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if err := s.blob.PutBytes(ctx, handoffRecordObj(job.SandboxID, job.Generation), data); err != nil {
		return err
	}
	return nil
}

func (s *Server) backupOwnedHandoff(ctx context.Context, job registry.HibernationHandoff, d *retainedHibernation, dir string) error {
	receipt := ownedBackupReceipt{Version: 1}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	if len(data) > ownedBackupMetadataLimit {
		return errors.New("handoff descriptor exceeds backup metadata limit")
	}
	receipt.DescriptorSHA256 = ownedBackupDescriptorDigest(data)
	addReceipt := func(r chunkstore.PayloadReceipt) {
		receipt.Payloads = append(receipt.Payloads, ownedBackupPayloadReceipt{Name: r.Name(), Generation: r.Generation()})
	}
	_, payloads := s.chunkStorage()
	w, err := payloads.OpenWriter(ctx, d.Manifest.Storage.SetID, handoffPublisherID(job.Generation))
	if err != nil {
		return err
	}
	suspended := false
	defer func() {
		if !suspended {
			_ = w.Suspend(context.Background())
		}
	}()

	f, err := os.Open(filepath.Join(dir, "mem.bin"))
	if err != nil {
		return err
	}
	defer f.Close()
	seen := make(map[string]bool)
	buf := make([]byte, min(d.Manifest.ChunkSize, d.Manifest.MemSize))
	for idx, entry := range d.Manifest.Chunks {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw := buf[:d.Manifest.chunkLen(uint64(idx))]
		if _, err := io.ReadFull(f, raw); err != nil {
			return err
		}
		if entry.Hash == chunkZeroHash {
			if !allZero(raw) {
				return fmt.Errorf("zero handoff chunk %d changed", idx)
			}
			continue
		}
		if !chunkMatches(raw, uint64(len(raw)), entry.Hash) {
			return fmt.Errorf("handoff chunk %d failed content verification", idx)
		}
		if seen[entry.Hash] {
			continue
		}
		seen[entry.Hash] = true
		compressed, err := gzipBytes(raw)
		if err != nil {
			return err
		}
		r, err := w.PutBytes(ctx, "chunks/"+entry.Hash, compressed)
		if err != nil {
			return err
		}
		addReceipt(r)
	}
	r, err := w.PutSparse(ctx, "state.sz", filepath.Join(dir, "state.bin"))
	if err != nil {
		return err
	}
	addReceipt(r)
	rootfs := filepath.Join(dir, "rootfs.ext4")
	if d.Record.RootfsForm == rootfsFormDiff {
		r, err = w.PutRanges(ctx, "rootfs.sz", rootfs, d.RootfsRanges)
	} else {
		r, err = w.PutSparse(ctx, "rootfs.sz", rootfs)
	}
	if err != nil {
		return err
	}
	addReceipt(r)
	if err := s.writeOwnedBackupReceipt(ctx, w, d, &receipt); err != nil {
		return err
	}
	if _, err := w.PutBytes(ctx, "record.json", data); err != nil {
		return err
	}
	if err := w.Suspend(context.Background()); err != nil {
		return err
	}
	suspended = true
	return nil
}

func (s *Server) cleanupHandoffGeneration(ctx context.Context, generation string, now time.Time) error {
	return s.reg.WithHibernationHandoff(ctx, generation, func(attempt *registry.HandoffAttempt) error {
		return s.cleanupHandoffAttempt(ctx, attempt, generation, now)
	})
}

func (s *Server) cleanupHandoffAttempt(ctx context.Context, attempt *registry.HandoffAttempt, generation string, now time.Time) error {
	job, err := attempt.Job(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	if !job.Published || !job.BackupComplete {
		return nil
	}
	descriptor, err := handoffJobDescriptor(job)
	if err != nil {
		return err
	}
	if descriptor != nil && descriptor.Manifest.Storage != nil {
		current, revision, err := s.readHandoff(ctx, job.SandboxID)
		if err != nil {
			return err
		}
		if current != nil && revision != job.ExpectedRevision && (current.Generation != generation || current.Phase == handoffDestroyed) {
			if err := s.retireHandoffStorage(ctx, descriptor); err != nil {
				return err
			}
		}
		catalog, _ := s.chunkStorage()
		if err := catalog.ReleasePublisher(ctx, descriptor.Manifest.Storage.SetID, handoffPublisherID(generation)); err != nil {
			return err
		}
	}
	op := s.snapshotLock("hib-peer:" + generation)
	op.Lock()
	defer op.Unlock()
	if !job.CacheReady {
		d, err := s.readRetainedHibernation(generation)
		if err != nil && !os.IsNotExist(err) {
			return err
		}
		if err == nil && now.Unix() < d.Ref.ExpiresAtUnix {
			return nil
		}
	}
	if err := os.RemoveAll(filepath.Join(s.hibPeerDir(), generation)); err != nil {
		return err
	}
	return attempt.Remove(ctx)
}

func handoffJobDescriptor(job registry.HibernationHandoff) (*retainedHibernation, error) {
	offer, err := decodeHandoff(job.Offer, job.SandboxID)
	if err != nil {
		return nil, err
	}
	if offer.Generation != job.Generation || offer.Phase != handoffOffered {
		return nil, errors.New("handoff journal offer identity mismatch")
	}
	if offer.Descriptor == nil {
		return nil, nil
	}
	if err := offer.Descriptor.validateStorage(); err != nil {
		return nil, err
	}
	return offer.Descriptor, nil
}

func (s *Server) runHandoffBackups(ctx context.Context) {
	type completion struct {
		generation string
		err        error
	}
	done := make(chan completion, 2)
	active := make(map[string]bool)
	retryAt := make(map[string]time.Time)
	var workers sync.WaitGroup
	defer workers.Wait()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	schedule := func() {
		jobs, err := s.reg.ListHibernationHandoffs(ctx)
		if err != nil {
			fmt.Fprintf(os.Stderr, "handoff backup journal: %v\n", err)
			return
		}
		present := make(map[string]bool, len(jobs))
		for _, job := range jobs {
			present[job.Generation] = true
		}
		for generation := range retryAt {
			if !present[generation] {
				delete(retryAt, generation)
			}
		}
		for _, job := range jobs {
			if len(active) >= 2 {
				break
			}
			if active[job.Generation] || time.Now().Before(retryAt[job.Generation]) {
				continue
			}
			active[job.Generation] = true
			workers.Add(1)
			go func(job registry.HibernationHandoff) {
				defer workers.Done()
				attempt, cancel := context.WithTimeout(ctx, uploadTimeout)
				defer cancel()
				var publicationErr error
				if !job.Published {
					publicationErr = s.publishHandoffOffer(attempt, job)
				}
				err := s.backupHandoff(attempt, job.Generation)
				if err == nil {
					err = publicationErr
				}
				done <- completion{job.Generation, err}
			}(job)
		}
	}
	schedule()
	for {
		select {
		case <-ctx.Done():
			return
		case result := <-done:
			delete(active, result.generation)
			if errors.Is(result.err, registry.ErrHandoffBusy) {
				retryAt[result.generation] = time.Now().Add(time.Second)
			} else if result.err != nil {
				fmt.Fprintf(os.Stderr, "handoff %s backup: %v\n", result.generation, result.err)
				retryAt[result.generation] = time.Now().Add(10 * time.Second)
			} else {
				retryAt[result.generation] = time.Now().Add(time.Minute)
			}
		case <-ticker.C:
			schedule()
		}
	}
}
