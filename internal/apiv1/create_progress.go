package apiv1

import (
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

type CreateProgress struct {
	Coordination CreateCoordination    `json:"coordination"`
	Worker       *CreateWorkerProgress `json:"worker,omitempty"`
}

type CreateCoordination struct {
	Phase     string     `json:"phase"`
	UpdatedAt *time.Time `json:"updated_at,omitempty"`
}

type CreateWorkerProgress struct {
	Attempt       int64                     `json:"attempt"`
	Sequence      int64                     `json:"sequence"`
	Condition     string                    `json:"condition"`
	Current       CreateStageMark           `json:"current"`
	LastCompleted *CreateCompletedStageMark `json:"last_completed,omitempty"`
	ObservedAt    time.Time                 `json:"observed_at"`
}

type CreateStageMark struct {
	Stage       string     `json:"stage"`
	Attempt     int64      `json:"attempt"`
	StartedAt   time.Time  `json:"started_at"`
	CompletedAt *time.Time `json:"completed_at,omitempty"`
}

type CreateCompletedStageMark struct {
	Stage       string    `json:"stage"`
	Attempt     int64     `json:"attempt"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
}

func publicCreateProgress(m createops.StoredMember) *CreateProgress {
	out := &CreateProgress{Coordination: CreateCoordination{Phase: "queued"}}
	if m.Outcome != nil {
		out.Coordination.Phase = "completed"
	} else if m.Worker != nil {
		out.Coordination.Phase = "assigned"
	}
	if m.Coordination != nil {
		out.Coordination.Phase = m.Coordination.Phase
		if !m.Coordination.UpdatedAt.IsZero() {
			updatedAt := m.Coordination.UpdatedAt
			out.Coordination.UpdatedAt = &updatedAt
		}
	}
	if p := m.Progress; p != nil {
		out.Worker = &CreateWorkerProgress{
			Attempt: p.Attempt, Sequence: p.Sequence, Condition: p.Condition,
			Current: publicCreateStageMark(p.Current), ObservedAt: p.ObservedAt,
		}
		if last := p.LastCompleted; last != nil && last.CompletedAt != nil {
			out.Worker.LastCompleted = &CreateCompletedStageMark{
				Stage: string(last.Stage), Attempt: last.Attempt,
				StartedAt: last.StartedAt, CompletedAt: *last.CompletedAt,
			}
		}
	}
	return out
}

func publicCreateStageMark(mark registry.CreateStageMark) CreateStageMark {
	return CreateStageMark{
		Stage: string(mark.Stage), Attempt: mark.Attempt,
		StartedAt: mark.StartedAt, CompletedAt: mark.CompletedAt,
	}
}
