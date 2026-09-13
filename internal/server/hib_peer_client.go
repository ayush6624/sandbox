package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/vm"
)

type hibPeerSource struct {
	s                *Server
	endpoint         string
	manifest         chunkManifest
	load             func(uint64) ([]byte, error)
	unavailable      atomic.Bool
	generation       string
	workingSet       []uint64
	backupObject     string
	backupDescriptor []byte
	closeReader      func() error
	closeOnce        sync.Once
	closeErr         error
}

func (p *hibPeerSource) Close() error {
	p.closeOnce.Do(func() {
		if p.closeReader != nil {
			p.closeErr = p.closeReader()
		}
	})
	return p.closeErr
}

// peerHydrationTask owns the reader used by background hydration. Run releases
// it only after hydrateAndAcknowledge has joined all of its load workers.
type peerHydrationTask struct {
	peer     *hibPeerSource
	closeFn  func() error
	closeErr error
	once     sync.Once
}

func newPeerHydrationTask(peer *hibPeerSource, closeFn func() error) *peerHydrationTask {
	return &peerHydrationTask{peer: peer, closeFn: closeFn}
}

func (t *peerHydrationTask) Run(ctx context.Context) (err error) {
	if t == nil {
		return nil
	}
	defer func() { err = errors.Join(err, t.Close()) }()
	if t.peer == nil {
		return errors.New("peer hydration has no source")
	}
	return t.peer.hydrateAndAcknowledge(ctx)
}

func (t *peerHydrationTask) Close() error {
	if t == nil {
		return nil
	}
	t.once.Do(func() {
		if t.closeFn != nil {
			t.closeErr = t.closeFn()
		}
	})
	return t.closeErr
}

func (s *Server) openHibernationPeer(ctx context.Context, rec *hibRecord) (*hibPeerSource, error) {
	ref := rec.Peer
	if ref == nil || s.workerCredentials == nil || (rec.Generation == "" && ref.ExpiresAtUnix <= time.Now().Unix()) {
		return nil, fmt.Errorf("peer unavailable or expired")
	}
	base, err := snapshotPeerBase(ref.URL)
	if err != nil {
		return nil, err
	}
	if !validHibPeerGeneration(ref.Generation) {
		return nil, fmt.Errorf("invalid peer generation")
	}
	p := &hibPeerSource{s: s, endpoint: base + "/internal/v1/hibernations/" + ref.Generation, generation: rec.Generation}
	descriptorCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	resp, err := p.request(descriptorCtx, http.MethodGet, "")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var retained retainedHibernation
	if err := json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&retained); err != nil {
		return nil, err
	}
	if retained.Ref != *ref {
		return nil, fmt.Errorf("peer generation reference mismatch")
	}
	want, got := *rec, retained.Record
	want.Peer, got.Peer = nil, nil
	wantBytes, _ := json.Marshal(want)
	gotBytes, _ := json.Marshal(got)
	if !bytes.Equal(wantBytes, gotBytes) {
		return nil, fmt.Errorf("peer checkpoint record mismatch")
	}
	if err := retained.validateStorage(); err != nil {
		return nil, err
	}
	digest, err := hibPeerManifestDigest(&retained.Manifest)
	if err != nil {
		return nil, err
	}
	if digest != ref.ManifestSHA256 {
		return nil, fmt.Errorf("peer manifest digest mismatch")
	}
	if rec.MemMIB > 0 && retained.Manifest.MemSize != uint64(rec.MemMIB)<<20 {
		return nil, fmt.Errorf("peer memory size mismatch")
	}
	closeReader, err := s.acquireChunkReader(ctx, &retained.Manifest)
	if err != nil {
		return nil, err
	}
	p.closeReader = closeReader
	p.manifest = retained.Manifest
	p.workingSet = retained.WorkingSet
	if p.generation != "" {
		p.backupObject = retained.artifactObject("record.json")
		p.backupDescriptor, _ = json.Marshal(retained)
	}
	p.load = newChunkLoadWithPeer(&p.manifest, s.memoryChunkCache(), p.chunk, func(hash string) ([]byte, error) {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if p.generation != "" {
			return p.fetchPendingChunk(ctx, hash)
		}
		return s.blob.GetBytes(ctx, p.manifest.chunkObject(hash))
	})
	return p, nil
}

func (p *hibPeerSource) request(ctx context.Context, method, suffix string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, p.endpoint+suffix, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+p.s.workerCredentials.Outbound())
	resp, err := peerSnapshotClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		_ = resp.Body.Close()
		return nil, fmt.Errorf("peer %s: HTTP %d", suffix, resp.StatusCode)
	}
	return resp, nil
}

func (p *hibPeerSource) chunk(hash string, size uint64) ([]byte, error) {
	if p.unavailable.Load() {
		return nil, fmt.Errorf("peer no longer available")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	resp, err := p.request(ctx, http.MethodGet, "/chunks/"+hash)
	var raw []byte
	if err == nil {
		raw, err = io.ReadAll(io.LimitReader(resp.Body, int64(size)+1))
		_ = resp.Body.Close()
		if err == nil && !chunkMatches(raw, size, hash) {
			err = fmt.Errorf("peer chunk failed content verification")
		}
	}
	if err != nil {
		if p.generation == "" {
			p.unavailable.Store(true)
		}
		p.s.met.hibPeerFallbacks.Add(1)
		return nil, err
	}
	p.s.met.hibPeerChunks.Add(1)
	p.s.met.hibPeerChunkBytes.Add(int64(len(raw)))
	return raw, nil
}

// A pending backup may not have this hash yet. Retry the source and cloud
// independently; a transient peer error does not abandon the only complete copy.
func (p *hibPeerSource) fetchPendingChunk(ctx context.Context, hash string) ([]byte, error) {
	var size uint64
	for idx, chunk := range p.manifest.Chunks {
		if chunk.Hash == hash {
			size = p.manifest.chunkLen(uint64(idx))
			break
		}
	}
	for {
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		data, err := p.s.blob.GetBytes(attempt, p.manifest.chunkObject(hash))
		cancel()
		if err == nil && (p.manifest.Storage == nil || len(data) != 0) {
			return data, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if raw, err := p.chunk(hash, size); err == nil {
			return gzipBytes(raw)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// Peer artifacts are staged apart from ordinary reconstruction. A truncated
// sparse stream must never leave bytes in holes of the cloud fallback image.
func (s *Server) reconstructPeerHibernation(ctx context.Context, rec *hibRecord, rootfsPath string) (staged stagedHibernation, err error) {
	p, err := s.openHibernationPeer(ctx, rec)
	if err != nil {
		return stagedHibernation{}, err
	}
	releasePrimary := true
	defer func() {
		if releasePrimary {
			err = errors.Join(err, p.Close())
		}
		if err != nil {
			err = errors.Join(err, staged.Close())
		}
	}()
	if p.manifest.MemSize%(1<<20) != 0 {
		return stagedHibernation{}, fmt.Errorf("peer memory is not a whole MiB")
	}
	if rec.MemMIB == 0 {
		rec.MemMIB = int64(p.manifest.MemSize >> 20)
	}
	memPath, statePath, _, err := s.cfg.Provisioner.SnapshotPaths(hibID(rec.ID))
	if err != nil {
		return stagedHibernation{}, err
	}
	stateTmp, rootfsTmp := statePath+".peer.tmp", rootfsPath+".peer.tmp"
	_ = os.Remove(stateTmp)
	_ = os.Remove(rootfsTmp)
	defer os.Remove(stateTmp)
	defer os.Remove(rootfsTmp)
	if rec.RootfsForm == rootfsFormDiff && rec.RootfsBaseID != "" {
		base, err := s.ensureBaseRootfsLocal(ctx, rec.RootfsBaseID)
		if err != nil {
			return stagedHibernation{}, err
		}
		if err := provisioner.CloneFile(base, rootfsTmp); err != nil {
			return stagedHibernation{}, err
		}
	}
	ctx, cancel := context.WithTimeout(ctx, peerSnapshotTimeout)
	defer cancel()
	for _, artifact := range []struct{ name, path string }{{"state", stateTmp}, {"rootfs", rootfsTmp}} {
		n, err := s.fetchPeerSparse(ctx, p.endpoint+"/"+artifact.name, artifact.path)
		if err != nil {
			return stagedHibernation{}, err
		}
		s.met.hibPeerArtifacts.Add(1)
		s.met.hibPeerArtifactBytes.Add(n)
	}
	if err := os.Rename(stateTmp, statePath); err != nil {
		return stagedHibernation{}, err
	}
	if err := os.Rename(rootfsTmp, rootfsPath); err != nil {
		return stagedHibernation{}, err
	}
	staged = stagedHibernation{StatePath: statePath, Peer: p}
	if s.cfg.UFFDRestore {
		prefetch := s.cfg.UFFDChunkPrefetch
		if prefetch <= 0 {
			prefetch = defaultChunkPrefetch
		}
		workingSet := p.workingSet
		if p.generation == "" {
			workingSet = s.fetchWorkingSet(ctx, rec.ID, uint64(len(p.manifest.Chunks)))
		}
		staged.Chunks = &vm.UFFDChunkSource{Total: p.manifest.MemSize, ChunkSize: p.manifest.ChunkSize, Prefetch: uint64(prefetch), Load: p.load, Prewarm: workingSet, Close: p.Close}
	} else {
		if err := materializeMemoryChunks(memPath, &p.manifest, p.load); err != nil {
			return stagedHibernation{}, err
		}
		staged.MemPath = memPath
	}
	closeHydration, err := s.acquireChunkReader(ctx, &p.manifest)
	if err != nil {
		return stagedHibernation{}, err
	}
	staged.hydration = newPeerHydrationTask(p, closeHydration)
	if staged.Chunks != nil {
		releasePrimary = false
	}
	return staged, nil
}

func (p *hibPeerSource) hydrateAndAcknowledge(ctx context.Context) error {
	cache := p.s.memoryChunkCache()
	release, err := cache.reserve(&p.manifest)
	if err != nil {
		return fmt.Errorf("peer hydration: %w", err)
	}
	defer release()
	work := make(chan uint64)
	var workers sync.WaitGroup
	var failed atomic.Bool
	for i := 0; i < 8; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for idx := range work {
				if ctx.Err() != nil {
					failed.Store(true)
					continue
				}
				if _, err := p.load(idx); err != nil {
					failed.Store(true)
					continue
				}
				// Cache writes are best-effort for faults, but an acknowledgment
				// requires a verified copy that later faults can actually reopen.
				e := p.manifest.Chunks[idx]
				if cache.get(e.Hash, p.manifest.chunkLen(idx)) == nil {
					failed.Store(true)
				}
			}
		}()
	}
	seen := make(map[string]bool)
	for idx, entry := range p.manifest.Chunks {
		if entry.Hash == chunkZeroHash || seen[entry.Hash] {
			continue
		}
		seen[entry.Hash] = true
		select {
		case work <- uint64(idx):
		case <-ctx.Done():
			failed.Store(true)
		}
		if ctx.Err() != nil {
			break
		}
	}
	close(work)
	workers.Wait()
	if failed.Load() || ctx.Err() != nil {
		return fmt.Errorf("peer hydration incomplete")
	}
	if err := p.waitForBackup(ctx); err != nil {
		return fmt.Errorf("peer hydration backup: %w", err)
	}
	resp, err := p.request(ctx, http.MethodDelete, "")
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if p.generation == "" {
		p.unavailable.Store(true)
	}
	return nil
}

// A full pending image stays pinned until its cloud commit is visible. CacheReady
// therefore never precedes the durable fallback needed after those pins release.
func (p *hibPeerSource) waitForBackup(ctx context.Context) error {
	if p.generation == "" {
		return nil
	}
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		data, err := p.s.blob.GetBytes(ctx, p.backupObject)
		if err == nil && (p.manifest.Storage == nil || len(data) != 0) {
			var durable retainedHibernation
			if err := json.Unmarshal(data, &durable); err != nil {
				return err
			}
			if err := durable.validateStorage(); err != nil {
				return err
			}
			got, _ := json.Marshal(durable)
			if !bytes.Equal(got, p.backupDescriptor) {
				return fmt.Errorf("handoff backup descriptor mismatch")
			}
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

func (s *Server) startPeerHydration(id string, task *peerHydrationTask) {
	if task == nil {
		return
	}
	v, ok := s.machines.Load(id)
	if !ok {
		if err := task.Close(); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] close unused peer hydration source: %v\n", id, err)
		}
		return
	}
	ctx, cancel := context.WithTimeout(s.vmCtx, 2*time.Minute)
	go func() { _ = vm.Wait(ctx, v.(*vm.Machine)); cancel() }()
	go func() {
		defer cancel()
		if err := task.Run(ctx); err != nil {
			s.met.hibPeerHydrateFailures.Add(1)
			fmt.Fprintf(os.Stderr, "[%s] peer hydration: %v\n", id, err)
			return
		}
		s.met.hibPeerHydrated.Add(1)
	}()
}
