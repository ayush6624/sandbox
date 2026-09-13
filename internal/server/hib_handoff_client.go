package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/vm"
)

func (s *Server) reconstructHandoff(ctx context.Context, desc *retainedHibernation, rootfsPath string) (staged stagedHibernation, err error) {
	rec := desc.Record
	rec.Peer = &desc.Ref
	if rec.Generation != desc.Ref.Generation || !validHibPeerGeneration(rec.Generation) {
		return stagedHibernation{}, fmt.Errorf("handoff generation mismatch")
	}
	if err := desc.validateStorage(); err != nil {
		return stagedHibernation{}, err
	}
	if rec.MemMIB <= 0 || desc.Manifest.MemSize != uint64(rec.MemMIB)<<20 {
		return stagedHibernation{}, fmt.Errorf("handoff memory size mismatch")
	}
	staged, err = s.reconstructPeerHibernation(ctx, &rec, rootfsPath)
	if err == nil {
		return staged, nil
	}
	closeReader, acquireErr := s.acquireChunkReader(ctx, &desc.Manifest)
	if acquireErr != nil {
		return stagedHibernation{}, acquireErr
	}
	releaseReader := true
	defer func() {
		if releaseReader && closeReader != nil {
			err = errors.Join(err, closeReader())
		}
	}()
	s.met.hibPeerFallbacks.Add(1)
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(rec.ID))
	_ = os.Remove(rootfsPath)
	data, cloudErr := s.blob.GetBytes(ctx, desc.artifactObject("record.json"))
	if cloudErr != nil {
		return stagedHibernation{}, fmt.Errorf("peer unavailable (%v), generation backup incomplete: %w", err, cloudErr)
	}
	if desc.Manifest.Storage != nil && len(data) == 0 {
		return stagedHibernation{}, fmt.Errorf("generation backup incomplete: record upload pending")
	}
	var durable retainedHibernation
	if err := json.Unmarshal(data, &durable); err != nil {
		return stagedHibernation{}, err
	}
	want, _ := json.Marshal(desc)
	got, _ := json.Marshal(durable)
	if !bytes.Equal(want, got) {
		return stagedHibernation{}, fmt.Errorf("handoff backup descriptor mismatch")
	}
	if err := durable.validateStorage(); err != nil {
		return stagedHibernation{}, err
	}
	mem, state, _, err := s.cfg.Provisioner.SnapshotPaths(hibID(rec.ID))
	if err != nil {
		return stagedHibernation{}, err
	}
	if rec.RootfsForm == rootfsFormDiff {
		base, err := s.ensureBaseRootfsLocal(ctx, rec.RootfsBaseID)
		if err != nil {
			return stagedHibernation{}, err
		}
		if err := provisioner.CloneFile(base, rootfsPath); err != nil {
			return stagedHibernation{}, err
		}
	}
	if err := s.blob.GetSparse(ctx, durable.artifactObject("state.sz"), state); err != nil {
		return stagedHibernation{}, err
	}
	if err := s.blob.GetSparse(ctx, durable.artifactObject("rootfs.sz"), rootfsPath); err != nil {
		return stagedHibernation{}, err
	}
	load := newChunkLoad(&durable.Manifest, s.memoryChunkCache(), func(hash string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		return s.blob.GetBytes(ctx, durable.Manifest.chunkObject(hash))
	})
	staged = stagedHibernation{StatePath: state}
	if s.cfg.UFFDRestore {
		prefetch := s.cfg.UFFDChunkPrefetch
		if prefetch <= 0 {
			prefetch = defaultChunkPrefetch
		}
		staged.Chunks = &vm.UFFDChunkSource{Total: durable.Manifest.MemSize, ChunkSize: durable.Manifest.ChunkSize, Prefetch: uint64(prefetch), Load: load, Prewarm: durable.WorkingSet, Close: closeReader}
	} else {
		if err := materializeMemoryChunks(mem, &durable.Manifest, load); err != nil {
			return stagedHibernation{}, err
		}
		staged.MemPath = mem
	}
	if staged.Chunks != nil {
		releaseReader = false
	}
	return staged, nil
}
