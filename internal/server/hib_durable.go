package server

// Durable hibernation records (roadmap B4a). A full hibernation freeze already
// chunks its mem image to GCS (uffd_chunks.go); B4a makes the REST of a
// hibernated sandbox durable too — device state, rootfs, and a registry record —
// so a DIFFERENT host can reconstruct and wake it (B4b/B4c). Purely additive:
// every upload is best-effort and a failure just leaves the sandbox
// host-local-wakeable, exactly as before.
//
// Bucket layout (additive to uffd_chunks.go + snapshot_gcs.go):
//
//	hib/<id>/mem.diff.sz   DIFF freeze only: dirty-page mem overlay vs the golden base mem
//	hib/<id>/state.sz      FC device-state file (sparse stream)
//	hib/<id>/rootfs.sz     rootfs overlay vs the golden base rootfs (diff), or full-sparse (cold-boot)
//	hib/<id>/record.json   the registry record + durability pointers; written LAST = commit marker
//	hib/<id>/owner         ownership fence {host,epoch}, CAS-written (B4b)
//
// A full freeze's mem lives in chunks/ + manifest.json (served lazily by the GCS
// chunk source); a diff freeze's mem lives in mem.diff.sz and rebases onto the
// durable base on the far host. record.json.MemForm says which.

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"time"

	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

const hibRecordVersion = 1

// Mem/rootfs durability forms recorded in the hibernation record.
const (
	memFormChunked = "chunked" // full freeze: mem in chunks/ + manifest.json
	memFormDiff    = "diff"    // diff freeze: mem in hib/<id>/mem.diff.sz, rebased on the base
	rootfsFormDiff = "diff"    // rootfs in hib/<id>/rootfs.sz as extents vs the base rootfs
	rootfsFormFull = "full"    // rootfs in hib/<id>/rootfs.sz as a full sparse stream (cold-boot)
)

func hibStateObj(id string) string   { return "hib/" + id + "/state.sz" }
func hibRootfsObj(id string) string  { return "hib/" + id + "/rootfs.sz" }
func hibMemDiffObj(id string) string { return "hib/" + id + "/mem.diff.sz" }
func hibRecordObj(id string) string  { return "hib/" + id + "/record.json" }
func hibOwnerObj(id string) string   { return "hib/" + id + "/owner" }

// hibRecord is the durable, host-independent description of a hibernated
// sandbox: everything a far host needs to reconstruct a local row (via a
// CreateRestore-shaped insert with a fresh tap/IP from ITS pools) and locate
// the mem/state/rootfs it must pull. Written last as the commit marker — a
// sandbox is cross-host-wakeable iff its record.json exists.
type hibRecord struct {
	Version int    `json:"version"`
	ID      string `json:"id"`
	Name    string `json:"name,omitempty"`
	// Guest resources baked into the snapshot (a restore/clone can't override
	// them; they're reported truthfully to clients).
	Vcpus  int64 `json:"vcpus"`
	MemMIB int64 `json:"mem_mib"`
	// Lifecycle fields carried across the move.
	CreatedAtUnix     int64  `json:"created_at_unix"`
	ExpiresAtUnix     *int64 `json:"expires_at_unix,omitempty"`
	HibernateAfterSec int    `json:"hibernate_after_sec,omitempty"`
	// BaseSnapshotID keeps the sandbox diff-snapshottable after a move (unchanged
	// meaning: the golden it was cloned from).
	BaseSnapshotID string `json:"base_snapshot_id,omitempty"`
	// Mem durability: MemForm=chunked → read manifest.json; MemForm=diff → pull
	// mem.diff.sz and rebase onto MemBaseID's base mem.
	MemForm   string `json:"mem_form"`
	MemBaseID string `json:"mem_base_id,omitempty"`
	// Rootfs durability: RootfsForm=diff → reflink RootfsBaseID's base rootfs and
	// overlay rootfs.sz; RootfsForm=full → rootfs.sz IS the whole (sparse) rootfs.
	RootfsForm   string `json:"rootfs_form"`
	RootfsBaseID string `json:"rootfs_base_id,omitempty"`
	// Explicit guest ports to re-expose on the far host. Host ports are not
	// carried; the adopting host allocates fresh ones from its own pool.
	GuestPorts       []int `json:"guest_ports,omitempty"`
	LegacyGuestPorts []int `json:"extra_guest_ports,omitempty"` // read old records
}

func unixPtr(t *time.Time) *int64 {
	if t == nil {
		return nil
	}
	u := t.Unix()
	return &u
}

// buildHibRecord assembles the durable record from a sandbox row + the freeze's
// durability decisions. Pure (no I/O) so it's unit-testable; the orchestrator
// fills the *Form/*BaseID fields from what it actually uploaded.
func buildHibRecord(sb registry.Sandbox, ports []registry.PortMapping,
	memForm, memBaseID, rootfsForm, rootfsBaseID string) hibRecord {
	rec := hibRecord{
		Version:           hibRecordVersion,
		ID:                sb.ID,
		Name:              sb.Name,
		Vcpus:             sb.Vcpus,
		MemMIB:            sb.MemMIB,
		CreatedAtUnix:     sb.CreatedAt.Unix(),
		ExpiresAtUnix:     unixPtr(sb.ExpiresAt),
		HibernateAfterSec: sb.HibernateAfterSec,
		BaseSnapshotID:    sb.BaseSnapshotID,
		MemForm:           memForm,
		MemBaseID:         memBaseID,
		RootfsForm:        rootfsForm,
		RootfsBaseID:      rootfsBaseID,
	}
	for _, pm := range ports {
		rec.GuestPorts = append(rec.GuestPorts, pm.GuestPort)
	}
	return rec
}

// uploadHibernation makes a just-frozen sandbox durable in GCS so any host can
// wake it (roadmap B4a). Runs in the background like uploadSnapshot; a failure at
// any step logs and leaves the sandbox host-local-wakeable (the record.json
// commit marker is never written on a partial upload, so it's simply not
// cross-host-adoptable — never half-adoptable).
//
// memPath is the local mem file (a full snapshot, or a sparse diff when
// snapType==Diff); memDiffBaseID is the golden id a diff mem rebases onto ("" for
// full). The chunk upload for a full mem is done here (uploadMemChunks); a diff
// mem is uploaded whole as a sparse overlay.
func (s *Server) uploadHibernation(id string, sb registry.Sandbox, memPath, statePath, snapType, memDiffBaseID string, workingSet []uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	defer cancel()
	t0 := time.Now()

	// --- mem ---
	var memForm, memBaseID string
	if snapType == vm.SnapshotDiff {
		memForm, memBaseID = memFormDiff, memDiffBaseID
		if _, err := s.blob.PutSparse(ctx, hibMemDiffObj(id), memPath); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] durable hibernate aborted: upload diff mem: %v\n", id, err)
			return
		}
	} else {
		memForm = memFormChunked
		if err := s.uploadMemChunks(ctx, id, memPath, roundChunkSize(s.cfg.UFFDChunkBytes), workingSet); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] durable hibernate aborted: %v\n", id, err)
			return
		}
	}

	// --- device state ---
	if _, err := s.blob.PutSparse(ctx, hibStateObj(id), statePath); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] durable hibernate aborted: upload state: %v\n", id, err)
		return
	}

	// --- rootfs (diff vs the golden base when we have one durable, else full) ---
	rootfsForm, rootfsBaseID, err := s.uploadHibRootfs(ctx, id, sb, snapType, memDiffBaseID)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] durable hibernate aborted: upload rootfs: %v\n", id, err)
		return
	}

	// --- record (commit marker, written LAST) ---
	ports, err := s.reg.Ports(ctx, id)
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] durable hibernate aborted: list ports: %v\n", id, err)
		return
	}
	rec := buildHibRecord(sb, ports, memForm, memBaseID, rootfsForm, rootfsBaseID)
	meta, err := json.Marshal(rec)
	if err == nil {
		err = s.blob.PutBytes(ctx, hibRecordObj(id), meta)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "[%s] durable hibernate: write record: %v\n", id, err)
		return
	}
	fmt.Fprintf(os.Stderr, "[%s] durable hibernation record written to gs://%s (mem=%s rootfs=%s) in %s\n",
		id, s.blob.Bucket(), memForm, rootfsForm, time.Since(t0).Round(time.Millisecond))
}

// uploadHibRootfs uploads the sandbox rootfs, as a diff overlay against the
// golden base rootfs when one is durable (the common hot-created-clone case —
// tens of MiB), else as a full sparse stream (cold-boot: no base to diff). It
// returns the recorded form + base id.
func (s *Server) uploadHibRootfs(ctx context.Context, id string, sb registry.Sandbox, snapType, memDiffBaseID string) (form, baseID string, err error) {
	// Prefer the same golden the mem diffs against (already durable); otherwise
	// the sandbox's clone base, if it's a golden we've uploaded.
	baseID = memDiffBaseID
	if baseID == "" && sb.BaseSnapshotID != "" && s.baseUploaded(sb.BaseSnapshotID) {
		baseID = sb.BaseSnapshotID
	}
	if baseID != "" {
		base, gerr := s.reg.GetSnapshot(ctx, baseID)
		if gerr != nil || !base.Golden {
			baseID = "" // base vanished or isn't golden — fall back to full
		} else if berr := s.ensureBaseUploaded(ctx, base); berr != nil {
			baseID = "" // couldn't guarantee the base is durable — fall back to full
		} else if ranges, derr := s.cfg.Provisioner.DiffExtents(sb.RootfsPath, base.RootfsPath); derr == nil {
			if _, perr := s.blob.PutRanges(ctx, hibRootfsObj(id), sb.RootfsPath, toBlobRanges(ranges)); perr != nil {
				return "", "", perr
			}
			return rootfsFormDiff, baseID, nil
		}
		// DiffExtents failed — fall through to a full upload (correctness over size).
	}
	if _, perr := s.blob.PutSparse(ctx, hibRootfsObj(id), sb.RootfsPath); perr != nil {
		return "", "", perr
	}
	return rootfsFormFull, "", nil
}
