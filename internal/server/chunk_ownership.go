package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sort"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/registry"
)

type chunkStorageRef struct {
	SetID  string `json:"set_id"`
	RootID string `json:"root_id"`
}

func (m *chunkManifest) chunkObject(hash string) string {
	if m.Storage != nil {
		return chunkstore.DataPrefix(m.Storage.SetID) + "chunks/" + hash
	}
	return chunkObj(hash)
}

func (d *retainedHibernation) artifactObject(name string) string {
	if d.Manifest.Storage != nil {
		return chunkstore.DataPrefix(d.Manifest.Storage.SetID) + name
	}
	return "handoff/" + d.Record.ID + "/" + d.Ref.Generation + "/" + name
}

func (d *retainedHibernation) validateStorage() error {
	if err := d.Manifest.validate(); err != nil {
		return err
	}
	ref := d.Manifest.Storage
	if ref != nil && (ref.SetID != d.Ref.Generation || ref.SetID != d.Record.Generation || ref.RootID != handoffControlObj(d.Record.ID)) {
		return fmt.Errorf("handoff storage identity mismatch")
	}
	return nil
}

type chunkOwnership struct {
	once           sync.Once
	catalog        *chunkstore.Store
	payloads       *chunkstore.Payloads
	mu             sync.Mutex
	readers        map[*chunkstore.Reader]chunkReaderState
	readerCursor   string
	recoveryCursor string
	ownerCursor    string
	rootCursor     chunkStorageRef
}

func (s *Server) chunkStorage() (*chunkstore.Store, *chunkstore.Payloads) {
	o := &s.chunkOwnership
	o.once.Do(func() {
		o.catalog = chunkstore.New(s.blob)
		o.payloads = chunkstore.NewPayloads(o.catalog, s.blob)
		o.mu.Lock()
		o.readers = make(map[*chunkstore.Reader]chunkReaderState)
		o.mu.Unlock()
	})
	return o.catalog, o.payloads
}

func handoffPublisherID(generation string) string { return "handoff-backup:" + generation }

// The journal is committed before this method. Attaching the root precedes the
// control CAS, so a destination can acquire a reader as soon as it sees an offer.
func (s *Server) ensureHandoffStorage(ctx context.Context, d *retainedHibernation) error {
	if err := d.validateStorage(); err != nil {
		return err
	}
	if d.Manifest.Storage == nil {
		return nil
	}
	catalog, _ := s.chunkStorage()
	ref := d.Manifest.Storage
	publisher := handoffPublisherID(ref.SetID)
	if err := catalog.Begin(ctx, ref.SetID, publisher); err != nil {
		return err
	}
	return catalog.AttachRoot(ctx, ref.SetID, publisher, ref.RootID)
}

// Call only after the control CAS has fenced this root's publication and reopen
// rights. A failed release stays queued. The catalog retains protection across
// process death; owner death reconciliation is a separate collection gate.
func (s *Server) retireHandoffStorage(ctx context.Context, d *retainedHibernation) error {
	if d == nil || d.Manifest.Storage == nil {
		return nil
	}
	if err := d.validateStorage(); err != nil {
		return err
	}
	ref := d.Manifest.Storage
	release := registry.ChunkRootRelease{SetID: ref.SetID, RootID: ref.RootID, SandboxID: d.Record.ID, Fenced: true}
	if err := s.reg.QueueChunkRootRelease(ctx, release); err != nil {
		return err
	}
	return s.releaseChunkRoot(ctx, release)
}

// Queue before the replacement request, whose successful response may be lost.
// Same-generation claims and reopen operations still need the previous root.
func (s *Server) queueReplacedHandoff(ctx context.Context, previous *handoffControl, revision int64) error {
	if previous == nil || previous.Descriptor == nil || previous.Descriptor.Manifest.Storage == nil {
		return nil
	}
	ref := previous.Descriptor.Manifest.Storage
	return s.reg.QueueChunkRootRelease(ctx, registry.ChunkRootRelease{SetID: ref.SetID, RootID: ref.RootID, SandboxID: previous.Record.ID, Revision: revision})
}

func (s *Server) releaseChunkRoot(ctx context.Context, release registry.ChunkRootRelease) error {
	catalog, _ := s.chunkStorage()
	if !release.Fenced {
		current, revision, err := s.readHandoff(ctx, release.SandboxID)
		if err != nil {
			return err
		}
		if current == nil || revision == release.Revision || (current.Generation == release.SetID && current.Phase != handoffDestroyed) {
			return nil
		}
	}
	if err := catalog.RetireRoot(ctx, release.SetID, release.RootID); err != nil && !errors.Is(err, chunkstore.ErrNotFound) {
		return err
	}
	return s.reg.RemoveChunkRootRelease(ctx, release.SetID, release.RootID)
}

func (s *Server) retireReplacedHandoff(ctx context.Context, previous *handoffControl, generation string) {
	if previous == nil || previous.Generation == generation {
		return
	}
	if err := s.retireHandoffStorage(ctx, previous.Descriptor); err != nil {
		fmt.Fprintf(os.Stderr, "retire replaced handoff %s: %v\n", previous.Generation, err)
	}
}

func (s *Server) retryChunkRoots(ctx context.Context) {
	o := &s.chunkOwnership
	roots, err := s.reg.ListChunkRootReleases(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read chunk root retirement journal: %v\n", err)
		return
	}
	o.mu.Lock()
	cursor := o.rootCursor
	o.mu.Unlock()
	start := sort.Search(len(roots), func(i int) bool {
		return roots[i].SetID > cursor.SetID || roots[i].SetID == cursor.SetID && roots[i].RootID > cursor.RootID
	})
	for i := range roots {
		if ctx.Err() != nil {
			return
		}
		release := roots[(start+i)%len(roots)]
		// Advance before I/O so a timed-out first entry cannot starve the rest.
		o.mu.Lock()
		o.rootCursor = chunkStorageRef{SetID: release.SetID, RootID: release.RootID}
		o.mu.Unlock()
		if err := s.releaseChunkRoot(ctx, release); err != nil {
			fmt.Fprintf(os.Stderr, "retire chunk root %s: %v\n", release.SetID, err)
		}
	}
}

func (s *Server) runChunkReleases(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.retryChunkReleases(ctx)
		}
	}
}
