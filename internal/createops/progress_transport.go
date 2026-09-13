package createops

import "github.com/ayush6624/sandbox/internal/registry"

type ProgressUpdate struct {
	Worker   Worker
	Progress registry.CreateProgress
}

type ProgressAcknowledgement struct {
	Sequence int64
}
