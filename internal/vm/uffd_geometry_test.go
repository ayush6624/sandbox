package vm

import (
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
)

func TestBuildUFFDSourceValidatesExternalGeometry(t *testing.T) {
	load := func(uint64) ([]byte, error) { return make([]byte, 4096), nil }
	for _, tc := range []struct {
		name   string
		source UFFDChunkSource
		valid  bool
	}{
		{"aligned", UFFDChunkSource{Total: 8192, ChunkSize: 4096, Load: load}, true},
		{"empty", UFFDChunkSource{ChunkSize: 4096, Load: load}, false},
		{"partial page", UFFDChunkSource{Total: 4097, ChunkSize: 4096, Load: load}, false},
		{"zero chunk", UFFDChunkSource{Total: 8192, Load: load}, false},
		{"partial chunk", UFFDChunkSource{Total: 8192, ChunkSize: 4097, Load: load}, false},
		{"missing loader", UFFDChunkSource{Total: 8192, ChunkSize: 4096}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes atomic.Int32
			tc.source.Close = func() error { closes.Add(1); return nil }
			src, err := buildUFFDSource(RunOptions{UFFDChunks: &tc.source}, "not-a-local-file")
			if (err == nil) != tc.valid {
				t.Fatalf("valid=%v err=%v", tc.valid, err)
			}
			if src != nil {
				if err := src.close(); err != nil {
					t.Fatal(err)
				}
				if err := src.close(); err != nil {
					t.Fatal(err)
				}
			}
			if closes.Load() != 1 {
				t.Fatalf("external close count=%d, want 1", closes.Load())
			}
		})
	}
}

func TestExternalChunkSourceCloseWaitsForBackgroundLoads(t *testing.T) {
	for _, tc := range []struct {
		name  string
		start func(*chunkedSource)
	}{
		{"prefetch", func(cs *chunkedSource) {
			if _, err := cs.at(0, 4096); err != nil {
				t.Fatal(err)
			}
		}},
		{"prewarm", func(cs *chunkedSource) { cs.startPrewarm() }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				entered := make(chan struct{})
				release := make(chan struct{})
				closed := make(chan struct{})
				var enterOnce, releaseOnce sync.Once
				releaseLoad := func() { releaseOnce.Do(func() { close(release) }) }
				t.Cleanup(releaseLoad)
				source := &UFFDChunkSource{
					Total: 8192, ChunkSize: 4096, Prefetch: 1, Prewarm: []uint64{1},
					Load: func(idx uint64) ([]byte, error) {
						if idx == 1 {
							enterOnce.Do(func() { close(entered) })
							<-release
						}
						return make([]byte, 4096), nil
					},
					Close: func() error { close(closed); return nil },
				}
				page, err := buildUFFDSource(RunOptions{UFFDChunks: source}, "unused")
				if err != nil {
					t.Fatal(err)
				}
				cs := page.(*chunkedSource)
				tc.start(cs)
				<-entered

				done := make(chan error, 1)
				go func() { done <- cs.close() }()
				synctest.Wait()
				select {
				case <-closed:
					t.Fatal("external source closed before background load drained")
				default:
				}
				select {
				case err := <-done:
					t.Fatalf("source close returned before background load drained: %v", err)
				default:
				}

				releaseLoad()
				synctest.Wait()
				if err := <-done; err != nil {
					t.Fatal(err)
				}
				select {
				case <-closed:
				default:
					t.Fatal("external source was not closed after drain")
				}
			})
		})
	}
}

func TestUFFDPrewarmAndCloseDoNotUseReleasedSource(t *testing.T) {
	for range 1000 {
		var released atomic.Bool
		cs := newChunkedSource(8192, 4096, 1, func(uint64) ([]byte, error) {
			if released.Load() {
				t.Error("prewarm used released source")
			}
			return make([]byte, 4096), nil
		}, func() error { released.Store(true); return nil }, []uint64{0})
		var workers sync.WaitGroup
		workers.Add(2)
		go func() { defer workers.Done(); cs.startPrewarm() }()
		go func() { defer workers.Done(); cs.close() }()
		workers.Wait()
		cs.startPrewarm()
	}
}

func TestUFFDChunkLoaderPanicReleasesWaiters(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	cs := newChunkedSource(4096, 4096, 1, func(uint64) ([]byte, error) {
		close(entered)
		<-release
		panic("loader failure")
	}, nil, []uint64{0})
	cs.startPrewarm()
	<-entered
	done := make(chan error, 1)
	go func() { _, err := cs.at(0, 4096); done <- err }()
	close(release)
	if err := <-done; err == nil {
		t.Fatal("loader panic was not returned as a fault error")
	}
	if err := cs.close(); err != nil {
		t.Fatal(err)
	}
}
