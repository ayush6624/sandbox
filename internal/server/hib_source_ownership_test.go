package server

import (
	"bytes"
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/ayush6624/sandbox/internal/vm"
)

func TestStagedHibernationTransfersOrClosesEachOwner(t *testing.T) {
	for _, tc := range []struct {
		name          string
		takeChunks    bool
		takeHydration bool
	}{
		{"close both", false, false},
		{"transfer chunks", true, false},
		{"transfer hydration", false, true},
		{"transfer both", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var chunkCloses, hydrationCloses atomic.Int32
			staged := stagedHibernation{
				Chunks: &vm.UFFDChunkSource{Close: func() error {
					chunkCloses.Add(1)
					return nil
				}},
				hydration: newPeerHydrationTask(nil, func() error {
					hydrationCloses.Add(1)
					return nil
				}),
			}
			chunks := (*vm.UFFDChunkSource)(nil)
			if tc.takeChunks {
				chunks = staged.takeChunks()
			}
			hydration := (*peerHydrationTask)(nil)
			if tc.takeHydration {
				hydration = staged.takeHydration()
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
			if err := staged.Close(); err != nil {
				t.Fatal(err)
			}
			if chunks != nil {
				if err := chunks.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if hydration != nil {
				if err := hydration.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if chunkCloses.Load() != 1 || hydrationCloses.Load() != 1 {
				t.Fatalf("close counts chunks=%d hydration=%d", chunkCloses.Load(), hydrationCloses.Load())
			}
		})
	}
}

func TestPeerHydrationTaskClosesAfterCancelledLoadJoins(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		raw := bytes.Repeat([]byte{42}, 4096)
		hash := testChunkHash(raw)
		s := metricsTestServer(t)
		s.cfg.UFFDChunkCacheBytes = 4096
		cache := s.memoryChunkCache()
		entered := make(chan struct{})
		unblock := make(chan struct{})
		var unblockOnce sync.Once
		releaseLoad := func() { unblockOnce.Do(func() { close(unblock) }) }
		t.Cleanup(releaseLoad)
		p := &hibPeerSource{
			s: s,
			manifest: chunkManifest{
				MemSize: 4096, ChunkSize: 4096,
				Chunks: []chunkEntry{{Hash: hash}},
			},
			load: func(uint64) ([]byte, error) {
				close(entered)
				<-unblock
				cache.put(hash, raw)
				return raw, nil
			},
		}
		closed := make(chan struct{})
		task := newPeerHydrationTask(p, func() error { close(closed); return nil })
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan error, 1)
		go func() { done <- task.Run(ctx) }()
		<-entered
		cancel()
		synctest.Wait()
		select {
		case <-closed:
			t.Fatal("hydration owner closed while a load was blocked")
		default:
		}
		releaseLoad()
		if err := <-done; err == nil {
			t.Fatal("cancelled hydration unexpectedly succeeded")
		}
		select {
		case <-closed:
		default:
			t.Fatal("hydration owner did not close after workers joined")
		}
	})
}

func TestStartPeerHydrationWithoutMachineClosesTask(t *testing.T) {
	var closes atomic.Int32
	task := newPeerHydrationTask(nil, func() error { closes.Add(1); return nil })
	new(Server).startPeerHydration("missing", task)
	if closes.Load() != 1 {
		t.Fatalf("close count=%d, want 1", closes.Load())
	}
}
