package apiv1

import (
	"context"
	"log"
	"sync"
	"time"

	"github.com/ayush6624/sandbox/internal/createops"
	"github.com/ayush6624/sandbox/internal/registry"
)

type createChunk struct {
	members []createops.Member
	worker  *createops.Worker
}

func (h *Handler) executeOperation(ctx context.Context, op createops.Operation) {
	if op.CompletedAt != nil {
		h.finishSingle(ctx, op)
		return
	}
	size := 1
	if len(op.Members) > 0 {
		source := op.Members[0].Spec.Source
		if _, snapshot := normalizeSource(Source{Type: source.Type, ID: source.ID}); snapshot {
			size = min(fanoutChunk, max(1, op.MaxParallelism))
		}
	}
	var unassigned []createops.Member
	selected := map[createops.Worker][]createops.Member{}
	for _, member := range op.Members {
		if member.Outcome != nil {
			continue
		}
		if member.Worker == nil {
			unassigned = append(unassigned, member.Member)
		} else {
			selected[*member.Worker] = append(selected[*member.Worker], member.Member)
		}
	}
	var chunks []createChunk
	appendChunks := func(members []createops.Member, worker *createops.Worker) {
		for offset := 0; offset < len(members); offset += size {
			chunks = append(chunks, createChunk{members: members[offset:min(offset+size, len(members))], worker: worker})
		}
	}
	// Resume accepted assignments before requesting new capacity.
	for worker, members := range selected {
		appendChunks(members, &worker)
	}
	appendChunks(unassigned, nil)
	parallel := max(1, op.MaxParallelism/size)
	sem := make(chan struct{}, parallel)
	var running sync.WaitGroup
	for _, chunk := range chunks {
		select {
		case sem <- struct{}{}:
		case <-ctx.Done():
			running.Wait()
			return
		}
		running.Add(1)
		go func(chunk createChunk) {
			defer running.Done()
			defer func() { <-sem }()
			if chunk.worker != nil {
				h.executeMembers(ctx, op.ID, *chunk.worker, chunk.members, nil)
				return
			}
			ids := make([]string, len(chunk.members))
			for i, member := range chunk.members {
				ids[i] = member.ID
			}
			if err := h.createOps.SetCoordination(ctx, op.ID, ids, "placing"); err != nil {
				log.Printf("create operation %s: persist placement progress: %v", op.ID, err)
				return
			}
			placement, err := h.executor.Place(ctx, chunk.members[0].Spec, len(chunk.members))
			if err != nil {
				if ctx.Err() == nil {
					h.recordFailures(ctx, op.ID, chunk.members, 503, "placement_failed", "No worker capacity became available before the placement deadline.")
				}
				return
			}
			if err := h.createOps.Assign(ctx, op.ID, ids, placement.Worker); err != nil {
				placement.Release(nil)
				log.Printf("create operation %s: persist assignment: %v", op.ID, err)
				return
			}
			h.executeMembers(ctx, op.ID, placement.Worker, chunk.members, placement.Release)
		}(chunk)
	}
	running.Wait()
	completed, err := h.createOps.Get(ctx, op.ID)
	if err == nil {
		h.finishSingle(ctx, completed)
	}
}

func (h *Handler) executeMembers(parent context.Context, operationID string, worker createops.Worker, members []createops.Member, release func([]createops.Outcome)) {
	ctx, cancel := context.WithTimeout(parent, 2*time.Minute)
	defer cancel()
	var owner *registry.CreateProgressOwner
	if worker.CreateProgress {
		owner = &registry.CreateProgressOwner{CoordinatorID: h.createOps.CoordinatorID(), OperationID: operationID}
	}
	outcomes, err := h.executor.Execute(ctx, worker, createops.Command{RegistryID: worker.RegistryID, Members: members, Owner: owner})
	if release != nil {
		release(outcomes)
	}
	if err != nil {
		if parent.Err() == nil {
			ids := make([]string, len(members))
			for i, member := range members {
				ids[i] = member.ID
			}
			if err := h.createOps.SetCoordination(parent, operationID, ids, "retrying"); err != nil {
				log.Printf("create operation %s: persist retry progress: %v", operationID, err)
			}
			log.Printf("create operation %s: assigned worker retry pending: %v", operationID, err)
		}
		return
	}
	if _, err := h.createOps.Record(ctx, operationID, outcomes); err != nil {
		log.Printf("create operation %s: persist results: %v", operationID, err)
	}
}

func (h *Handler) recordFailures(ctx context.Context, id string, members []createops.Member, status int, code, detail string) {
	outcomes := make([]createops.Outcome, len(members))
	for i, member := range members {
		outcomes[i] = createops.Outcome{ID: member.ID, Failure: &createops.Failure{Status: status, Code: code, Detail: detail}}
	}
	if _, err := h.createOps.Record(ctx, id, outcomes); err != nil {
		log.Printf("create operation %s: persist placement failure: %v", id, err)
	}
}
