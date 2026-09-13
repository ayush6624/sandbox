package server

import (
	"context"
	"encoding/json"
	"fmt"
	"os"

	"github.com/ayush6624/sandbox/internal/registry"
)

// Caller holds the lifecycle lock and has confirmed cloud durability. Peer
// retention is a cache: its failure must not undo an otherwise valid release.
func (s *Server) releaseHibernation(ctx context.Context, sb registry.Sandbox) (*hibPeerRef, error) {
	s.cancelHibernationUpload(sb.ID)
	data, generation, err := s.blob.GetBytesGen(ctx, hibRecordObj(sb.ID))
	if err != nil {
		return nil, fmt.Errorf("read committed hibernation: %w", err)
	}
	var rec hibRecord
	if err := json.Unmarshal(data, &rec); err != nil {
		return nil, err
	}
	if rec.ID != sb.ID || rec.Version != hibRecordVersion {
		return nil, fmt.Errorf("committed hibernation identity mismatch")
	}
	peer, peerErr := s.retainHibernation(ctx, &rec)
	if peerErr != nil {
		fmt.Fprintf(os.Stderr, "[%s] peer retention unavailable (%v); release uses GCS\n", sb.ID, peerErr)
	}
	if err := s.reg.Destroy(ctx, sb.ID); err != nil {
		if peer != nil {
			_ = s.removePeerHibernation(peer.Generation)
		}
		return nil, fmt.Errorf("drop local row: %w", err)
	}
	s.pf.CloseSandbox(sb.ID)
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(sb.ID))
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	s.act.forget(sb.ID)
	if peer != nil {
		rec.Peer = peer
		data, err := json.Marshal(rec)
		if err == nil {
			_, err = s.blob.PutBytesIfGenerationMatch(ctx, hibRecordObj(sb.ID), data, generation)
		}
		if err != nil {
			// An adopter may already have invalidated this record. Never put an
			// old generation back; the release already has its durable fallback.
			_ = s.removePeerHibernation(peer.Generation)
			fmt.Fprintf(os.Stderr, "[%s] peer hint not published: %v\n", sb.ID, err)
			peer = nil
		}
	}
	fmt.Fprintf(os.Stderr, "[%s] released from this host (durable in GCS; peer=%t)\n", sb.ID, peer != nil)
	return peer, nil
}
