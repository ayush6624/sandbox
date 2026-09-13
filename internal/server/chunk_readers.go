package server

import (
	"context"
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/ayush6624/sandbox/internal/chunkstore"
	"github.com/ayush6624/sandbox/internal/registry"
)

type chunkReaderState uint8

const (
	chunkReaderActive chunkReaderState = iota
	chunkReaderRemoteRelease
	chunkReaderLocalRelease
)

const chunkReaderRecoveryPage = 32

func (s *Server) acquireChunkReader(ctx context.Context, m *chunkManifest) (func() error, error) {
	if err := m.validate(); err != nil {
		return nil, err
	}
	if m.Storage == nil {
		return nil, nil
	}
	if s.blob == nil {
		return nil, fmt.Errorf("owned chunks require a snapshot bucket")
	}
	owner, err := s.reg.ChunkReaderOwner(ctx)
	if err != nil {
		return nil, err
	}
	catalog, _ := s.chunkStorage()
	reader, err := catalog.PrepareReader(m.Storage.SetID, m.Storage.RootID, owner)
	if err != nil {
		return nil, err
	}
	o := &s.chunkOwnership
	o.mu.Lock()
	if len(o.readers) >= registry.MaxChunkReaders {
		o.mu.Unlock()
		return nil, registry.ErrChunkReaderCapacity
	}
	o.readers[reader] = chunkReaderActive
	o.mu.Unlock()
	if err := s.reg.QueueChunkReader(ctx, reader.Identity()); err != nil {
		o.mu.Lock()
		o.readers[reader] = chunkReaderLocalRelease
		o.mu.Unlock()
		return nil, err
	}
	if err := reader.Acquire(ctx); err != nil {
		o.mu.Lock()
		o.readers[reader] = chunkReaderRemoteRelease
		o.mu.Unlock()
		return nil, err
	}
	return func() error {
		o.mu.Lock()
		if _, exists := o.readers[reader]; exists {
			o.readers[reader] = chunkReaderRemoteRelease
		}
		o.mu.Unlock()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return s.releaseChunkReader(ctx, reader)
	}, nil
}

func (s *Server) releaseChunkReader(ctx context.Context, reader *chunkstore.Reader) error {
	o := &s.chunkOwnership
	o.mu.Lock()
	state, exists := o.readers[reader]
	o.mu.Unlock()
	if !exists {
		return nil
	}
	if state == chunkReaderActive {
		return fmt.Errorf("chunk reader has not drained")
	}
	if state == chunkReaderRemoteRelease {
		if err := reader.Close(ctx); err != nil {
			return err
		}
	}
	if err := s.reg.RemoveChunkReader(ctx, reader.Identity()); err != nil {
		return err
	}
	o.mu.Lock()
	delete(o.readers, reader)
	o.mu.Unlock()
	return nil
}

func (s *Server) retryChunkReaders(ctx context.Context) {
	o := &s.chunkOwnership
	o.mu.Lock()
	var pending []*chunkstore.Reader
	for reader, state := range o.readers {
		if state != chunkReaderActive {
			pending = append(pending, reader)
		}
	}
	cursor := o.readerCursor
	o.mu.Unlock()
	sort.Slice(pending, func(i, j int) bool { return pending[i].Identity().ReaderID < pending[j].Identity().ReaderID })
	start := sort.Search(len(pending), func(i int) bool { return pending[i].Identity().ReaderID > cursor })
	for i := 0; i < min(len(pending), chunkReaderRecoveryPage); i++ {
		if ctx.Err() != nil {
			return
		}
		reader := pending[(start+i)%len(pending)]
		o.mu.Lock()
		o.readerCursor = reader.Identity().ReaderID
		o.mu.Unlock()
		if err := s.releaseChunkReader(ctx, reader); err != nil {
			fmt.Fprintf(os.Stderr, "release chunk reader: %v\n", err)
		}
	}
}

func (s *Server) recoverChunkReaders(ctx context.Context) {
	o := &s.chunkOwnership
	o.mu.Lock()
	cursor := o.recoveryCursor
	o.mu.Unlock()
	readers, err := s.reg.ListChunkReaders(ctx, cursor, chunkReaderRecoveryPage)
	if err != nil {
		fmt.Fprintf(os.Stderr, "read chunk reader journal: %v\n", err)
		return
	}
	if len(readers) == 0 {
		o.mu.Lock()
		o.recoveryCursor = ""
		o.mu.Unlock()
		return
	}
	deadOwners := make(map[string]bool)
	catalog, _ := s.chunkStorage()
	for _, identity := range readers {
		if ctx.Err() != nil {
			return
		}
		o.mu.Lock()
		o.recoveryCursor = identity.ReaderID
		o.mu.Unlock()
		dead, checked := deadOwners[identity.OwnerID]
		if !checked {
			dead, err = s.reg.ConfirmChunkReaderOwnerDead(ctx, identity.OwnerID)
			deadOwners[identity.OwnerID] = dead && err == nil
			if err != nil {
				fmt.Fprintf(os.Stderr, "check chunk reader owner %s: %v\n", identity.OwnerID, err)
				continue
			}
		}
		if !dead {
			continue
		}
		reader, err := catalog.RecoverReader(identity)
		if err == nil {
			err = reader.Close(ctx)
		}
		if err == nil {
			err = s.reg.RemoveChunkReader(ctx, identity)
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "recover chunk reader %s: %v\n", identity.ReaderID, err)
		}
	}
}

func (s *Server) pruneChunkReaderOwners(ctx context.Context) {
	o := &s.chunkOwnership
	o.mu.Lock()
	cursor := o.ownerCursor
	o.mu.Unlock()
	next, err := s.reg.PruneChunkReaderOwners(ctx, cursor, chunkReaderRecoveryPage)
	o.mu.Lock()
	o.ownerCursor = next
	o.mu.Unlock()
	if err != nil {
		fmt.Fprintf(os.Stderr, "prune chunk reader owners: %v\n", err)
	}
}

func (s *Server) retryChunkReleases(ctx context.Context) {
	for _, retry := range []func(context.Context){s.retryChunkReaders, s.recoverChunkReaders, s.pruneChunkReaderOwners, s.retryChunkRoots} {
		if ctx.Err() != nil {
			return
		}
		attempt, cancel := context.WithTimeout(ctx, 2*time.Second)
		retry(attempt)
		cancel()
	}
}
