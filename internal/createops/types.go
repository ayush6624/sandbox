// Package createops defines the durable create-command boundary shared by the
// public API coordinator, gateway placement, and worker allocation executor.
package createops

import (
	"context"
	"errors"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
)

// Operation is the durable public command and its retained member outcomes.
// Status is pending, running, succeeded, partially_succeeded, or failed.
type Operation struct {
	ID             string
	RequestID      string
	Type           string
	Status         string
	Requested      int
	MaxParallelism int
	Members        []StoredMember
	CreatedAt      time.Time
	CompletedAt    *time.Time
}

type ListQuery struct {
	Limit    int
	BeforeID string
	Offset   int
}

type OperationPage struct {
	Operations []Operation
	NextID     string
}

var ErrInvalidPage = errors.New("invalid operation page")

type StoredMember struct {
	Member
	Worker       *Worker
	Outcome      *Outcome
	Progress     *registry.CreateProgress
	Coordination *Coordination
}

type Coordination struct {
	Phase     string
	UpdatedAt time.Time
}

// Acceptance is returned atomically for an idempotency key. ResponseBody is
// the original accepted representation, including after the operation ends.
type Acceptance struct {
	Operation      Operation
	Created        bool
	ResponseStatus int
	ResponseBody   []byte
}

type AcceptRequest struct {
	ID             string
	RequestID      string
	Scope          string
	BodyHash       []byte
	Type           string
	MaxParallelism int
	Members        []Member
	ResponseStatus int
	ResponseBody   []byte
}

var ErrIdempotencyConflict = errors.New("idempotency key was reused with a different request")

// Spec is the normalized public create request persisted before dispatch.
type Spec struct {
	Source    Source
	Name      string
	Resources *Resources
	Lifecycle Lifecycle
	Metadata  map[string]string
}

type Source struct {
	Type string
	ID   string
}

type Resources struct {
	VCPU      int64
	MemoryMIB int64
}

type Lifecycle struct {
	TTLSeconds         int
	IdleTimeoutSeconds int
}

// Worker identifies one durable worker registry. HostID alone is insufficient:
// a replacement can reuse it with a fresh registry database.
type Worker struct {
	HostID     string
	RegistryID string
	// CreateProgress is retained at assignment so upgrades cannot add ownership
	// to an existing anonymous worker request.
	CreateProgress bool
}

// Member is a stable operation member identity, never a caller-selected
// sandbox ID. Workers link this ID to either a cold allocation or warm claim.
type Member struct {
	ID    string
	Index int
	Spec  Spec
}

type Command struct {
	Owner      *registry.CreateProgressOwner `json:"Owner,omitempty"`
	RegistryID string
	Members    []Member
	// SnapshotPeer is a transient local-cache hint. It is never persisted as
	// part of Spec and does not affect command identity.
	SnapshotPeer string
}

type Failure struct {
	Status int
	Code   string
	Detail string
}

type Outcome struct {
	ID      string
	Sandbox *registry.Sandbox
	Failure *Failure
	// Routable is a current worker observation, separate from the retained
	// historical create result. Replay after deletion must not publish a route.
	Routable bool
}

// Placement owns a new capacity reservation. Release must be called exactly
// once after Execute returns; replaying a selected member has no Placement.
type Placement struct {
	Worker  Worker
	Release func([]Outcome)
}

// Executor keeps placement and allocation ownership outside apiv1. The
// coordinator persists Placement.Worker before Execute and never re-places a
// member whose worker was already recorded.
type Executor interface {
	Place(context.Context, Spec, int) (Placement, error)
	Execute(context.Context, Worker, Command) ([]Outcome, error)
}

// Store serializes durable acceptance, selected-worker fencing, and retained
// outcomes. A caller must assign before Execute; assigned members are replayed
// only through their recorded worker.
type Store interface {
	CoordinatorID() string
	IngestProgress(context.Context, Worker, registry.CreateProgress) (int64, error)
	Accept(context.Context, AcceptRequest) (Acceptance, error)
	Get(context.Context, string) (Operation, error)
	List(context.Context, ListQuery) (OperationPage, error)
	Pending(context.Context) ([]Operation, error)
	SetCoordination(context.Context, string, []string, string) error
	Assign(context.Context, string, []string, Worker) error
	Record(context.Context, string, []Outcome) (Operation, error)
	SetFinalResponse(context.Context, string, int, []byte) error
	FinalResponse(context.Context, string) (int, []byte, bool, error)
	Close() error
}
