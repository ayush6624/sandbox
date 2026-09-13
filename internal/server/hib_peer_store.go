package server

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/google/uuid"
)

const hibPeerRetention = 30 * time.Minute

type hibPeerRef struct {
	URL            string `json:"url"`
	Generation     string `json:"generation"`
	ManifestSHA256 string `json:"manifest_sha256"`
	ExpiresAtUnix  int64  `json:"expires_at_unix"`
}

type retainedHibernation struct {
	Record       hibRecord       `json:"record"`
	Manifest     chunkManifest   `json:"manifest"`
	Ref          hibPeerRef      `json:"ref"`
	RootfsRanges []gcsblob.Range `json:"rootfs_ranges,omitempty"`
	WorkingSet   []uint64        `json:"working_set,omitempty"`
}

func validHibPeerGeneration(generation string) bool {
	id, err := uuid.Parse(generation)
	return err == nil && id.String() == generation
}

func (s *Server) hibPeerDir() string {
	return filepath.Join(s.cfg.Provisioner.SnapshotDir, "hib-peer")
}

func hibPeerManifestDigest(m *chunkManifest) (string, error) {
	b, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:]), nil
}

func (s *Server) privateAdvertiseURL() (string, error) {
	addr := s.cfg.AdvertiseAddr
	if addr == "" {
		scheme := "http://"
		if s.cfg.ManagementTransport == "tls" {
			scheme = "https://"
		}
		addr = scheme + s.cfg.ListenAddr
	}
	return snapshotPeerBase(addr)
}

// retainHibernation runs under wakeLock after the durability uploader is joined.
// The returned hint is optional; the caller publishes it only after row removal.
func (s *Server) retainHibernation(ctx context.Context, rec *hibRecord) (*hibPeerRef, error) {
	peerURL, err := s.privateAdvertiseURL()
	if err != nil {
		return nil, err
	}
	if rec.MemForm != memFormChunked {
		return nil, errors.New("peer retention requires chunked memory")
	}
	m, err := s.fetchChunkManifest(ctx, rec.ID)
	if err != nil {
		return nil, err
	}
	digest, err := hibPeerManifestDigest(m)
	if err != nil {
		return nil, err
	}
	mem, state, rootfs, err := s.cfg.Provisioner.SnapshotPaths(hibID(rec.ID))
	if err != nil {
		return nil, err
	}
	if b, err := os.ReadFile(hibDiffMarker(mem)); err == nil {
		mem, err = s.materializeHibMem(ctx, mem, strings.TrimSpace(string(b)))
		if err != nil {
			return nil, err
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	fi, err := os.Stat(mem)
	if err != nil {
		return nil, err
	}
	if uint64(fi.Size()) != m.MemSize {
		return nil, errors.New("retained memory size does not match manifest")
	}

	ref := hibPeerRef{URL: peerURL, Generation: uuid.NewString(), ManifestSHA256: digest, ExpiresAtUnix: time.Now().Add(hibPeerRetention).Unix()}
	desc := retainedHibernation{Record: *rec, Manifest: *m, Ref: ref}
	desc.Record.Peer = nil
	if rec.RootfsForm == rootfsFormDiff {
		if rec.RootfsBaseID == "" {
			return nil, errors.New("retained rootfs diff has no base")
		}
		base, err := s.ensureBaseRootfsLocal(ctx, rec.RootfsBaseID)
		if err != nil {
			return nil, err
		}
		ranges, err := s.cfg.Provisioner.DiffExtents(rootfs, base)
		if err != nil {
			return nil, err
		}
		desc.RootfsRanges = toBlobRanges(ranges)
	} else if rec.RootfsForm != rootfsFormFull {
		return nil, errors.New("unsupported retained rootfs form")
	}

	if err := s.persistRetainedHibernation(ctx, &desc, mem, state, rootfs); err != nil {
		return nil, err
	}
	return &ref, nil
}

func syncHibPeerPath(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

// Caller holds the generation read or write lock.
func (s *Server) readRetainedHibernation(generation string) (*retainedHibernation, error) {
	f, err := os.Open(filepath.Join(s.hibPeerDir(), generation, "descriptor.json"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	var d retainedHibernation
	if err := json.NewDecoder(io.LimitReader(f, 8<<20)).Decode(&d); err != nil {
		return nil, err
	}
	if d.Ref.Generation != generation {
		return nil, errors.New("retained generation mismatch")
	}
	if err := d.Manifest.validate(); err != nil {
		return nil, err
	}
	digest, err := hibPeerManifestDigest(&d.Manifest)
	if err != nil {
		return nil, err
	}
	if digest != d.Ref.ManifestSHA256 {
		return nil, errors.New("retained manifest digest mismatch")
	}
	return &d, nil
}

func (s *Server) removePeerHibernation(generation string) error {
	if !validHibPeerGeneration(generation) {
		return errors.New("invalid hibernation generation")
	}
	op := s.snapshotLock("hib-peer:" + generation)
	op.Lock()
	if s.reg != nil {
		_, err := s.reg.GetHibernationHandoff(context.Background(), generation)
		if err == nil {
			err = s.reg.AckHibernationHandoffCache(context.Background(), generation)
			op.Unlock()
			if err != nil {
				return err
			}
			err = s.cleanupHandoffGeneration(context.Background(), generation, time.Now())
			if errors.Is(err, registry.ErrHandoffBusy) {
				return nil
			}
			return err
		}
		if !errors.Is(err, sql.ErrNoRows) {
			op.Unlock()
			return err
		}
	}
	defer op.Unlock()
	return os.RemoveAll(filepath.Join(s.hibPeerDir(), generation))
}

func (s *Server) hasRetainedHibernation(id string) bool {
	entries, _ := os.ReadDir(s.hibPeerDir())
	for _, entry := range entries {
		if !entry.IsDir() || !validHibPeerGeneration(entry.Name()) {
			continue
		}
		op := s.snapshotLock("hib-peer:" + entry.Name())
		op.RLock()
		d, err := s.readRetainedHibernation(entry.Name())
		op.RUnlock()
		if err == nil && d.Record.ID == id && time.Now().Unix() < d.Ref.ExpiresAtUnix {
			return true
		}
	}
	return false
}

// Only startup calls this: a release can drop its row and crash before removing
// ordinary artifacts. Retained copies are independent, so keep them available.
func (s *Server) reconcileReleasedHibernationArtifacts(ctx context.Context) {
	if s.cfg.Provisioner == nil || s.reg == nil {
		return
	}
	entries, err := os.ReadDir(s.hibPeerDir())
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "reconcile peer hibernations: %v\n", err)
		}
		return
	}
	ids := make(map[string]bool)
	for _, entry := range entries {
		if !entry.IsDir() || !validHibPeerGeneration(entry.Name()) {
			continue
		}
		op := s.snapshotLock("hib-peer:" + entry.Name())
		op.RLock()
		d, err := s.readRetainedHibernation(entry.Name())
		op.RUnlock()
		if err == nil && validHibPeerGeneration(d.Record.ID) {
			ids[d.Record.ID] = true
		}
	}
	// Release takes wakeLock before a generation lock. Descriptor reads above
	// must finish before taking wakeLock, including for an expired generation.
	for id := range ids {
		if ctx.Err() != nil {
			return
		}
		mu := s.wakeLock(id)
		mu.Lock()
		_, err := s.reg.Get(ctx, id)
		if errors.Is(err, sql.ErrNoRows) {
			if err := s.cfg.Provisioner.CleanupSnapshot(hibID(id)); err != nil {
				fmt.Fprintf(os.Stderr, "reconcile released %s snapshot: %v\n", id, err)
			}
			if err := s.cfg.Provisioner.RemoveRootfs(s.cfg.Provisioner.RootfsPathFor(id)); err != nil && !os.IsNotExist(err) {
				fmt.Fprintf(os.Stderr, "reconcile released %s rootfs: %v\n", id, err)
			}
		}
		mu.Unlock()
	}
}

// A scrape may overlap cache creation or cleanup; these gauges are a disk
// inventory, and logical bytes deliberately include sparse holes.
func (s *Server) peerHibernationStats() (count, logicalBytes int64) {
	if s.cfg.Provisioner == nil {
		return 0, 0
	}
	entries, _ := os.ReadDir(s.hibPeerDir())
	for _, entry := range entries {
		if !entry.IsDir() || !validHibPeerGeneration(entry.Name()) {
			continue
		}
		count++
		for _, name := range []string{"mem.bin", "state.bin", "rootfs.ext4"} {
			if info, err := os.Stat(filepath.Join(s.hibPeerDir(), entry.Name(), name)); err == nil && info.Mode().IsRegular() {
				logicalBytes += info.Size()
			}
		}
	}
	return count, logicalBytes
}

func (s *Server) sweepPeerHibernations(now time.Time) {
	if s.cfg.Provisioner == nil {
		return
	}
	entries, err := os.ReadDir(s.hibPeerDir())
	if err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "peer hibernation sweep: %v\n", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		generation := strings.TrimSuffix(entry.Name(), ".tmp")
		if !validHibPeerGeneration(generation) {
			continue
		}
		op := s.snapshotLock("hib-peer:" + generation)
		op.Lock()
		if s.reg != nil && entry.Name() == generation {
			_, journalErr := s.reg.GetHibernationHandoff(context.Background(), generation)
			if journalErr == nil {
				op.Unlock()
				if err := s.cleanupHandoffGeneration(context.Background(), generation, now); err != nil && !errors.Is(err, registry.ErrHandoffBusy) {
					fmt.Fprintf(os.Stderr, "handoff cleanup %s: %v\n", generation, err)
				}
				continue
			}
			if !errors.Is(journalErr, sql.ErrNoRows) {
				op.Unlock()
				continue
			}
		}
		remove := entry.Name() != generation
		if !remove {
			d, err := s.readRetainedHibernation(generation)
			remove = err != nil || now.Unix() >= d.Ref.ExpiresAtUnix
		}
		if remove {
			if err := os.RemoveAll(filepath.Join(s.hibPeerDir(), entry.Name())); err != nil {
				fmt.Fprintf(os.Stderr, "peer hibernation sweep %s: %v\n", generation, err)
			}
		}
		op.Unlock()
	}
}

func (s *Server) peerHibernationLoop(ctx context.Context) {
	s.sweepPeerHibernations(time.Now())
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sweepPeerHibernations(now)
		}
	}
}

func (s *Server) handlePeerHibernation(w http.ResponseWriter, r *http.Request) {
	controller := http.NewResponseController(w)
	_ = controller.SetWriteDeadline(time.Now().Add(peerSnapshotTimeout))
	defer controller.SetWriteDeadline(time.Time{})
	generation := r.PathValue("generation")
	if !validHibPeerGeneration(generation) {
		http.NotFound(w, r)
		return
	}
	if r.Method == http.MethodDelete {
		if err := s.removePeerHibernation(generation); err != nil {
			httpError(w, 500, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
		return
	}
	op := s.snapshotLock("hib-peer:" + generation)
	op.RLock()
	defer op.RUnlock()
	d, err := s.readRetainedHibernation(generation)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if time.Now().Unix() >= d.Ref.ExpiresAtUnix {
		pending := false
		if d.Record.Generation != "" && s.reg != nil {
			job, err := s.reg.GetHibernationHandoff(r.Context(), generation)
			pending = err == nil && !job.BackupComplete
		}
		if !pending {
			http.NotFound(w, r)
			return
		}
	}
	dir := filepath.Join(s.hibPeerDir(), generation)
	if hash := r.PathValue("hash"); hash != "" {
		if len(hash) != 64 {
			http.NotFound(w, r)
			return
		}
		if _, err := hex.DecodeString(hash); err != nil {
			http.NotFound(w, r)
			return
		}
		for idx, chunk := range d.Manifest.Chunks {
			if hash != chunk.Hash || hash == chunkZeroHash {
				continue
			}
			start := int64(uint64(idx) * d.Manifest.ChunkSize)
			n := int64(min(d.Manifest.ChunkSize, d.Manifest.MemSize-uint64(start)))
			f, err := os.Open(filepath.Join(dir, "mem.bin"))
			if err != nil {
				http.NotFound(w, r)
				return
			}
			defer f.Close()
			w.Header().Set("Content-Type", "application/octet-stream")
			w.Header().Set("Content-Length", strconv.FormatInt(n, 10))
			written, err := io.CopyN(w, io.NewSectionReader(f, start, n), n)
			if err != nil {
				fmt.Fprintf(os.Stderr, "peer hibernation %s chunk stream: %v\n", generation, err)
				return
			}
			s.met.hibPeerServes.Add(1)
			s.met.hibPeerServeBytes.Add(written)
			return
		}
		http.NotFound(w, r)
		return
	}
	artifact := r.PathValue("artifact")
	if artifact == "" {
		writeJSON(w, http.StatusOK, d)
		return
	}
	var payload int64
	w.Header().Set("Content-Type", "application/vnd.sandbox.sparse")
	switch artifact {
	case "state":
		payload, err = gcsblob.WriteSparse(w, filepath.Join(dir, "state.bin"))
	case "rootfs":
		if d.Record.RootfsForm == rootfsFormDiff {
			payload, err = gcsblob.WriteRanges(w, filepath.Join(dir, "rootfs.ext4"), d.RootfsRanges)
		} else {
			payload, err = gcsblob.WriteSparse(w, filepath.Join(dir, "rootfs.ext4"))
		}
	default:
		http.NotFound(w, r)
		return
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "peer hibernation %s %s stream: %v\n", generation, artifact, err)
		return
	}
	s.met.hibPeerServes.Add(1)
	s.met.hibPeerServeBytes.Add(payload)
}
