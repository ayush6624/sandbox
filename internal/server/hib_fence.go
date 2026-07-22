package server

// Cross-host ownership fence (roadmap B4b). Exactly one host may own a
// hibernated sandbox at a time. The fence lives at hib/<id>/owner as
// {host_id, epoch} and is written with a GCS compare-and-swap
// (gcsblob.PutBytesIfGenerationMatch), so two hosts that race to adopt the same
// sandbox — the drain case, where the losing owner may still be live — resolve
// to a single winner. A revived former owner that finds the fence naming a
// different host relinquishes its stale local row (reconcile).
//
// A normal freeze writes NO fence: absence means "nobody has taken this away",
// so the original owner keeps serving it via heartbeat routing. The fence comes
// into existence only on the first adopt (an ownership transfer), and its epoch
// strictly increases so a stale writer can never re-establish an old claim.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/ayush6624/sandbox/internal/gcsblob"
	"github.com/ayush6624/sandbox/internal/registry"
)

// ErrOwnerContended means another host won the ownership CAS. The caller aborts
// its adopt and lets the winner proceed; it is retryable (the winner may fail
// and release), not a hard error.
var ErrOwnerContended = errors.New("hibernation ownership contended (another host holds the fence)")

// hibOwner is the fence value at hib/<id>/owner.
type hibOwner struct {
	HostID string `json:"host_id"`
	Epoch  int64  `json:"epoch"`
}

// hostID is this server's stable fleet identity (mirrors heartbeat's fallback
// chain): configured HostID, else hostname, else the advertise/listen addr.
func (s *Server) hostID() string {
	if s.cfg.HostID != "" {
		return s.cfg.HostID
	}
	if h, _ := os.Hostname(); h != "" {
		return h
	}
	if s.cfg.AdvertiseAddr != "" {
		return s.cfg.AdvertiseAddr
	}
	return s.cfg.ListenAddr
}

// nextOwner is the pure fence transition: previous fence bytes (nil if absent) +
// this host's id → the fence to write, with a strictly higher epoch.
func nextOwner(prev []byte, myHostID string) hibOwner {
	var cur hibOwner
	if len(prev) > 0 {
		_ = json.Unmarshal(prev, &cur)
	}
	return hibOwner{HostID: myHostID, Epoch: cur.Epoch + 1}
}

// acquireOwner CAS-claims id for this host, bumping the epoch. Returns the new
// fence on success, ErrOwnerContended if another host won the race.
func (s *Server) acquireOwner(ctx context.Context, id string) (hibOwner, error) {
	prev, gen, err := s.blob.GetBytesGen(ctx, hibOwnerObj(id))
	if err != nil && !errors.Is(err, gcsblob.ErrNotExist) {
		return hibOwner{}, fmt.Errorf("read owner fence: %w", err)
	}
	next := nextOwner(prev, s.hostID())
	b, _ := json.Marshal(next)
	// gen is 0 when the object is absent (ErrNotExist) → create-only semantics.
	if _, err := s.blob.PutBytesIfGenerationMatch(ctx, hibOwnerObj(id), b, gen); err != nil {
		if errors.Is(err, gcsblob.ErrPreconditionFailed) {
			return hibOwner{}, ErrOwnerContended
		}
		return hibOwner{}, fmt.Errorf("cas owner fence: %w", err)
	}
	return next, nil
}

// readOwner returns the current fence for id. ok=false means no fence exists
// (nobody has adopted the sandbox away).
func (s *Server) readOwner(ctx context.Context, id string) (owner hibOwner, ok bool, err error) {
	b, _, err := s.blob.GetBytesGen(ctx, hibOwnerObj(id))
	if errors.Is(err, gcsblob.ErrNotExist) {
		return hibOwner{}, false, nil
	}
	if err != nil {
		return hibOwner{}, false, err
	}
	if err := json.Unmarshal(b, &owner); err != nil {
		return hibOwner{}, false, fmt.Errorf("decode owner fence: %w", err)
	}
	return owner, true, nil
}

// relinquishIfAdoptedAway drops a local hibernated row whose durable owner fence
// names a DIFFERENT host — the sandbox was adopted elsewhere while this host was
// down (or mid-drain). It returns true when it relinquished. Conservative: no
// blob, a fence read error, an absent fence, or a fence naming us all keep the
// row (we relinquish only on proof we lost ownership). GCS artifacts are left
// intact — they belong to the adopter now.
func (s *Server) relinquishIfAdoptedAway(ctx context.Context, sb registry.Sandbox) bool {
	if s.blob == nil {
		return false
	}
	owner, ok, err := s.readOwner(ctx, sb.ID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: read owner fence for %s (keeping row): %v\n", sb.ID, err)
		return false
	}
	if !ok || owner.HostID == s.hostID() {
		return false
	}
	fmt.Fprintf(os.Stderr, "reconcile: %s adopted away by host %s (epoch %d); relinquishing local row\n",
		sb.ID, owner.HostID, owner.Epoch)
	s.pf.CloseSandbox(sb.ID)
	_ = s.cfg.Provisioner.CleanupSnapshot(hibID(sb.ID))
	_ = s.cfg.Provisioner.RemoveRootfs(sb.RootfsPath)
	if err := s.reg.Destroy(ctx, sb.ID); err != nil {
		fmt.Fprintf(os.Stderr, "reconcile: relinquish %s: destroy row: %v\n", sb.ID, err)
	}
	return true
}
