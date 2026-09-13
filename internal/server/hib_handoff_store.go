package server

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

// The foreground only hashes immutable memory. Compression belongs to backup.
func buildRawChunkManifest(ctx context.Context, path string, chunkSize uint64) (*chunkManifest, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() <= 0 || info.Size()%4096 != 0 || chunkSize == 0 || chunkSize%4096 != 0 {
		return nil, errors.New("invalid raw memory geometry")
	}
	m := &chunkManifest{Version: chunkManifestRawVersion, Codec: chunkCodecRawSHA256, MemSize: uint64(info.Size()), ChunkSize: chunkSize}
	buf := make([]byte, min(chunkSize, m.MemSize))
	for offset := uint64(0); offset < m.MemSize; offset += chunkSize {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		raw := buf[:min(chunkSize, m.MemSize-offset)]
		if _, err := io.ReadFull(f, raw); err != nil {
			return nil, err
		}
		entry := chunkEntry{Hash: chunkZeroHash}
		if !allZero(raw) {
			hash := sha256.Sum256(raw)
			entry.Hash = hex.EncodeToString(hash[:])
		}
		m.Chunks = append(m.Chunks, entry)
	}
	return m, m.validate()
}

// Caller holds the lifecycle lock and has joined the ordinary upload. All data
// dependencies must be local; preparation never starts a cloud request.
func (s *Server) prepareLocalHandoff(ctx context.Context, sb registry.Sandbox) (*retainedHibernation, error) {
	peerURL, err := s.privateAdvertiseURL()
	if err != nil {
		return nil, err
	}
	mem, state, rootfs, err := s.cfg.Provisioner.SnapshotPaths(hibID(sb.ID))
	if err != nil {
		return nil, err
	}
	baseID := sb.BaseSnapshotID
	if marker, err := os.ReadFile(hibDiffMarker(mem)); err == nil {
		baseID = strings.TrimSpace(string(marker))
		op := s.snapshotLock(baseID)
		op.RLock()
		baseMem, _, err := s.localHandoffBase(ctx, baseID)
		if err != nil {
			op.RUnlock()
			return nil, fmt.Errorf("local memory base: %w", err)
		}
		full := filepath.Join(filepath.Dir(mem), "handoff.full.bin")
		err = provisioner.CloneFile(baseMem, full)
		op.RUnlock()
		if err != nil {
			return nil, err
		}
		defer os.Remove(full)
		if err := s.cfg.Provisioner.OverlaySparse(mem, full); err != nil {
			return nil, err
		}
		mem = full
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	m, err := buildRawChunkManifest(ctx, mem, roundChunkSize(s.cfg.UFFDChunkBytes))
	if err != nil {
		return nil, err
	}
	ports, err := s.reg.Ports(ctx, sb.ID)
	if err != nil {
		return nil, err
	}
	sequence, err := s.reg.LastUsageSeq(ctx, sb.ID)
	if err != nil {
		return nil, err
	}
	rec := buildHibRecord(s.effectiveResources(sb), ports, memFormChunked, "", rootfsFormFull, "", sequence)
	d := &retainedHibernation{Record: rec, Manifest: *m}
	if baseID != "" && s.baseUploaded(baseID) {
		op := s.snapshotLock(baseID)
		op.RLock()
		_, baseRootfs, baseErr := s.localHandoffBase(ctx, baseID)
		if baseErr == nil {
			if ranges, err := s.cfg.Provisioner.DiffExtents(rootfs, baseRootfs); err == nil {
				d.Record.RootfsForm, d.Record.RootfsBaseID = rootfsFormDiff, baseID
				d.RootfsRanges = toBlobRanges(ranges)
			}
		}
		op.RUnlock()
	}
	if data, err := os.ReadFile(filepath.Join(filepath.Dir(state), "working-set.json")); err == nil {
		var indices []uint64
		if json.Unmarshal(data, &indices) == nil {
			for _, idx := range indices {
				if idx < uint64(len(m.Chunks)) {
					d.WorkingSet = append(d.WorkingSet, idx)
				}
			}
		}
	}
	generation := uuid.NewString()
	if s.cfg.OwnedHandoffStorage {
		d.Manifest.Version = chunkManifestOwnedVersion
		d.Manifest.Storage = &chunkStorageRef{SetID: generation, RootID: handoffControlObj(sb.ID)}
	}
	digest, err := hibPeerManifestDigest(&d.Manifest)
	if err != nil {
		return nil, err
	}
	d.Ref = hibPeerRef{URL: peerURL, Generation: generation, ManifestSHA256: digest, ExpiresAtUnix: time.Now().Add(hibPeerRetention).Unix()}
	d.Record.Generation = d.Ref.Generation
	if err := s.persistRetainedHibernation(ctx, d, mem, state, rootfs); err != nil {
		return nil, err
	}
	return d, nil
}

func (s *Server) localHandoffBase(ctx context.Context, id string) (mem, rootfs string, err error) {
	if base, getErr := s.reg.GetSnapshot(ctx, id); getErr == nil {
		mem, rootfs = base.MemPath, base.RootfsPath
		if _, err := os.Stat(mem); err == nil {
			if _, err := os.Stat(rootfs); err == nil {
				return mem, rootfs, nil
			}
		}
	}
	mem, rootfs = s.baseCachePaths(id)
	if _, err = os.Stat(mem); err != nil {
		return "", "", err
	}
	if _, err = os.Stat(rootfs); err != nil {
		return "", "", err
	}
	return mem, rootfs, nil
}

func (s *Server) persistRetainedHibernation(ctx context.Context, d *retainedHibernation, mem, state, rootfs string) error {
	op := s.snapshotLock("hib-peer:" + d.Ref.Generation)
	op.Lock()
	defer op.Unlock()
	dir := filepath.Join(s.hibPeerDir(), d.Ref.Generation)
	tmp := dir + ".tmp"
	if err := os.MkdirAll(tmp, 0700); err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	for _, file := range []struct{ path, name string }{{mem, "mem.bin"}, {state, "state.bin"}, {rootfs, "rootfs.ext4"}} {
		if err := ctx.Err(); err != nil {
			return err
		}
		dst := filepath.Join(tmp, file.name)
		if err := provisioner.CloneFile(file.path, dst); err != nil {
			return err
		}
		if err := syncHibPeerPath(dst); err != nil {
			return err
		}
	}
	data, err := json.Marshal(d)
	if err != nil {
		return err
	}
	path := filepath.Join(tmp, "descriptor.json")
	if err := os.WriteFile(path, data, 0600); err != nil {
		return err
	}
	if err := syncHibPeerPath(path); err != nil {
		return err
	}
	if err := syncHibPeerPath(tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		return err
	}
	if err := syncHibPeerPath(s.hibPeerDir()); err != nil {
		_ = os.RemoveAll(dir)
		return err
	}
	return nil
}
