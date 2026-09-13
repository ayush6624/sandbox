//go:build linux

package vm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestUFFDEntryPointsConsumeInvalidExternalSource(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(RunOptions) error
	}{
		{"clone", func(opts RunOptions) error {
			_, _, err := StartCloneUFFD(context.Background(), opts, CloneParams{})
			return err
		}},
		{"restore", func(opts RunOptions) error {
			_, _, err := RestoreUFFD(context.Background(), opts, "", "")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes atomic.Int32
			source := &UFFDChunkSource{
				ChunkSize: 4096,
				Load:      func(uint64) ([]byte, error) { return nil, nil },
				Close:     func() error { closes.Add(1); return nil },
			}
			if err := tc.run(RunOptions{UFFDChunks: source}); err == nil {
				t.Fatal("invalid source unexpectedly succeeded")
			}
			if closes.Load() != 1 {
				t.Fatalf("external close count=%d, want 1", closes.Load())
			}
		})
	}
}

func TestUFFDEntryPointsReleaseExternalSourceWhenPreparationFails(t *testing.T) {
	for _, tc := range []struct {
		name string
		run  func(RunOptions) error
	}{
		{"clone", func(opts RunOptions) error {
			_, _, err := StartCloneUFFD(context.Background(), opts, CloneParams{})
			return err
		}},
		{"restore", func(opts RunOptions) error {
			_, _, err := RestoreUFFD(context.Background(), opts, "unused", "unused")
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var closes atomic.Int32
			source := &UFFDChunkSource{
				Total: 4096, ChunkSize: 4096,
				Load:  func(uint64) ([]byte, error) { return make([]byte, 4096), nil },
				Close: func() error { closes.Add(1); return nil },
			}
			prepareErr := errors.New("prepare failed")
			opts := RunOptions{
				UFFDChunks: source,
				Launcher: ProcessLauncherFunc(func(context.Context, LaunchRequest) (PreparedLaunch, error) {
					return PreparedLaunch{}, prepareErr
				}),
			}
			if err := tc.run(opts); !errors.Is(err, prepareErr) {
				t.Fatalf("error=%v, want %v", err, prepareErr)
			}
			if closes.Load() != 1 {
				t.Fatalf("external close count=%d, want 1", closes.Load())
			}
		})
	}
}
