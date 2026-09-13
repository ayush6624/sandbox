//go:build !linux

package vm

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
)

func TestUnsupportedUFFDEntryPointsConsumeExternalSource(t *testing.T) {
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
			source := &UFFDChunkSource{Close: func() error { closes.Add(1); return nil }}
			if err := tc.run(RunOptions{UFFDChunks: source}); !errors.Is(err, ErrLinuxOnly) {
				t.Fatalf("error=%v, want ErrLinuxOnly", err)
			}
			if closes.Load() != 1 {
				t.Fatalf("external close count=%d, want 1", closes.Load())
			}
		})
	}
}
