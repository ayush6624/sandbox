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
	"path/filepath"
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
	// Labels and provenance travel with the sandbox. They are descriptive, but
	// dropping them on a move is not cosmetic: metadata is the only attribution
	// the billing ledger carries, so a sandbox adopted onto another host would
	// bill the rest of its life unattributable. Absent in records written before
	// this field existed, which restores as "no labels" — exactly what those
	// records could express.
	Metadata   map[string]string `json:"metadata,omitempty"`
	SourceType string            `json:"source_type,omitempty"`
	SourceID   string            `json:"source_id,omitempty"`
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
	GuestPorts       []int           `json:"guest_ports,omitempty"`
	URLGuestPorts    []int           `json:"url_guest_ports,omitempty"`
	PublicPorts      []hibPublicPort `json:"public_ports,omitempty"`
	LegacyGuestPorts []int           `json:"extra_guest_ports,omitempty"` // read old records
}

type hibPublicPort struct {
	GuestPort  int `json:"guest_port"`
	PublicPort int `json:"public_port"`
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
		Metadata:          sb.Metadata,
		SourceType:        sb.SourceType,
		SourceID:          sb.SourceID,
		MemForm:           memForm,
		MemBaseID:         memBaseID,
		RootfsForm:        rootfsForm,
		RootfsBaseID:      rootfsBaseID,
	}
	for _, pm := range ports {
		if pm.HostPort == 0 {
			rec.URLGuestPorts = append(rec.URLGuestPorts, pm.GuestPort)
		} else {
			rec.GuestPorts = append(rec.GuestPorts, pm.GuestPort)
		}
		if pm.PublicPort != 0 {
			rec.PublicPorts = append(rec.PublicPorts, hibPublicPort{
				GuestPort: pm.GuestPort, PublicPort: pm.PublicPort,
			})
		}
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
// full). rootfsPath is an immutable copy captured while the guest was paused;
// the live rootfs may be writable again by the time this goroutine runs.
func (s *Server) uploadHibernation(ctx context.Context, id string, sb registry.Sandbox, memPath, statePath, rootfsPath, snapType, memDiffBaseID string, workingSet []uint64) {
	t0 := time.Now()

	// A superseded record must be gone before this generation's payloads land
	// under the same (stable) object names, or a far host could pair the OLD
	// record.json with NEW payloads. The freeze itself no longer waits on that
	// delete, so retry it here — and if it still fails, publish nothing. That is
	// this function's normal failure mode: the sandbox stays host-local-wakeable.
	if s.hibNotAdoptable(id) {
		if err := s.deleteDurableObject(ctx, hibRecordObj(id)); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] durable hibernate aborted: superseded record still present: %v\n", id, err)
			return
		}
	}

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
		// Stamp the manifest as belonging to THIS frozen generation, so the wake
		// path may fault the guest's RAM in from it (hibChunkMarker).
		if err := os.WriteFile(hibChunkMarker(memPath), nil, 0o644); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] durable hibernate: stamp chunk generation: %v\n", id, err)
		}
	}

	// --- device state ---
	if _, err := s.blob.PutSparse(ctx, hibStateObj(id), statePath); err != nil {
		fmt.Fprintf(os.Stderr, "[%s] durable hibernate aborted: upload state: %v\n", id, err)
		return
	}

	// --- rootfs (diff vs the golden base when we have one durable, else full) ---
	rootfsForm, rootfsBaseID, err := s.uploadHibRootfs(ctx, id, sb, rootfsPath, snapType, memDiffBaseID)
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
		// Every payload for THIS generation is published, so the record about to
		// be written is current. Clear any not-adoptable marker first: clearing
		// after the write would leave a window where drainHibInvalidations could
		// delete a perfectly valid record, while clearing before it only risks a
		// no-op delete of an object that does not exist yet.
		s.clearHibNotAdoptable(id)
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
func (s *Server) uploadHibRootfs(ctx context.Context, id string, sb registry.Sandbox, rootfsPath, snapType, memDiffBaseID string) (form, baseID string, err error) {
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
		} else if ranges, derr := s.cfg.Provisioner.DiffExtents(rootfsPath, base.RootfsPath); derr == nil {
			if _, perr := s.blob.PutRanges(ctx, hibRootfsObj(id), rootfsPath, toBlobRanges(ranges)); perr != nil {
				return "", "", perr
			}
			return rootfsFormDiff, baseID, nil
		}
		// DiffExtents failed — fall through to a full upload (correctness over size).
	}
	if _, perr := s.blob.PutSparse(ctx, hibRootfsObj(id), rootfsPath); perr != nil {
		return "", "", perr
	}
	return rootfsFormFull, "", nil
}

// startHibernationUpload publishes one frozen generation. It registers the
// cancellable job before launching it, so a wake/destroy that starts
// immediately after hibernate returns cannot miss the uploader.
func (s *Server) startHibernationUpload(id string, sb registry.Sandbox, memPath, statePath, rootfsPath, snapType, memDiffBaseID string, workingSet []uint64) {
	ctx, cancel := context.WithTimeout(context.Background(), uploadTimeout)
	up := &backgroundUpload{cancel: cancel, done: make(chan struct{})}
	s.hibUpMu.Lock()
	s.hibUploads[id] = up
	s.hibUpMu.Unlock()
	go func() {
		defer cancel()
		defer func() {
			close(up.done)
			s.hibUpMu.Lock()
			if s.hibUploads[id] == up {
				delete(s.hibUploads, id)
			}
			s.hibUpMu.Unlock()
		}()
		s.uploadHibernation(ctx, id, sb, memPath, statePath, rootfsPath, snapType, memDiffBaseID, workingSet)
	}()
}

// cancelHibernationUpload prevents any later object/record write and waits
// until the uploader has stopped touching the local frozen files.
func (s *Server) cancelHibernationUpload(id string) {
	s.hibUpMu.Lock()
	up := s.hibUploads[id]
	s.hibUpMu.Unlock()
	if up == nil {
		return
	}
	up.cancel()
	<-up.done
}

// --- deferred invalidation (local not-adoptable markers) ---
//
// A durable generation stops being adoptable the instant its sandbox becomes a
// live mutable VM again (wake, adopt) or ceases to exist (destroy). The signal
// for that is the absence of record.json — but making a VM lifecycle step WAIT
// on an object-store delete is how a GCS blip during a Nomad task stop turned
// "hibernate every sandbox" into "destroy every sandbox". So the delete is
// best-effort, and the intent is recorded locally FIRST, in a marker file on the
// same persistent disk the sandbox's artifacts live on:
//
//	<SnapshotDir>/hib-invalidate/<id>   this host's durable record for <id> is
//	                                    stale; delete it when GCS is reachable
//
// The marker is authoritative for every decision THIS host makes (fetchHibRecord
// refuses a marked record, so neither adopt nor release can act on one), and
// drainHibInvalidations retries the delete until it lands — at startup too, so a
// destroy that outlived the process still finishes. What the marker cannot do is
// stop a DIFFERENT host from adopting a stale record in the window before the
// retry succeeds; that residual window is the price of not gating VM lifecycle on
// GCS, and it is bounded by object-store availability rather than unbounded.

// hibInvalidateDir holds one marker file per pending record invalidation. It
// lives beside the per-sandbox hibernation directories but is never a snapshot
// id itself (those are UUIDs), and reconcile's snapshot-dir sweep only touches
// hib-lineage-* entries. Empty when there is no snapshot directory to write in.
func (s *Server) hibInvalidateDir() string {
	if s.cfg.Provisioner == nil {
		return ""
	}
	return filepath.Join(s.cfg.Provisioner.SnapshotDir, "hib-invalidate")
}

func (s *Server) hibInvalidatePath(id string) string {
	dir := s.hibInvalidateDir()
	if dir == "" {
		return ""
	}
	return filepath.Join(dir, id)
}

// markHibNotAdoptable records that id's durable record must not be trusted. It
// is written BEFORE the delete is attempted so a crash in between leaves the
// safe answer ("stale") rather than the unsafe one.
func (s *Server) markHibNotAdoptable(id string) error {
	path := s.hibInvalidatePath(id)
	if path == "" {
		return fmt.Errorf("no snapshot directory to record the invalidation in")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	return os.WriteFile(path, nil, 0o644)
}

// clearHibNotAdoptable drops the marker: either the record is gone, or a fresh
// generation is about to become the current one.
func (s *Server) clearHibNotAdoptable(id string) {
	path := s.hibInvalidatePath(id)
	if path == "" {
		return
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		fmt.Fprintf(os.Stderr, "[%s] clear hibernation invalidation marker: %v\n", id, err)
	}
}

// hibNotAdoptable reports whether a pending invalidation makes id's durable
// record untrustworthy on this host.
func (s *Server) hibNotAdoptable(id string) bool {
	path := s.hibInvalidatePath(id)
	if path == "" {
		return false
	}
	_, err := os.Stat(path)
	return err == nil
}

// drainHibInvalidations retries the deletes that an unreachable object store
// deferred. Ids with an upload in flight are skipped: that uploader clears the
// marker just before it writes a NEW record.json, and deleting between those two
// steps would strip the durability of a generation that is perfectly current.
func (s *Server) drainHibInvalidations(ctx context.Context) {
	dir := s.hibInvalidateDir()
	if !s.durabilityEnabled() || dir == "" {
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if !os.IsNotExist(err) {
			fmt.Fprintf(os.Stderr, "list pending hibernation invalidations: %v\n", err)
		}
		return
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		id := entry.Name()
		s.hibUpMu.Lock()
		uploading := s.hibUploads[id] != nil
		s.hibUpMu.Unlock()
		if uploading || !s.hibNotAdoptable(id) {
			continue
		}
		if err := s.deleteDurableObject(ctx, hibRecordObj(id)); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] deferred durable record invalidation still failing: %v\n", id, err)
			continue
		}
		s.clearHibNotAdoptable(id)
		fmt.Fprintf(os.Stderr, "[%s] deferred durable hibernation record invalidation completed\n", id)
	}
}

// durabilityEnabled reports whether this server has an object store at all.
func (s *Server) durabilityEnabled() bool { return s.blob != nil || s.deleteObject != nil }

// deleteDurableObject removes one object from the durability store. Indirected
// through a field because s.blob is a concrete *gcsblob.Client rather than an
// interface, and the object-store-outage behaviour of the invalidation paths is
// exactly what needs test coverage (same idiom as Server.bootAge).
func (s *Server) deleteDurableObject(ctx context.Context, object string) error {
	if s.deleteObject != nil {
		return s.deleteObject(ctx, object)
	}
	return s.blob.Delete(ctx, object)
}

// resetHibernationDurability removes a previous generation before a new freeze
// is allowed to start. The local not-adoptable marker goes first and record.json
// next, so any partial cleanup is still safely non-adoptable. Chunks are
// content-addressed and intentionally shared.
//
// It returns an error describing what could not be deleted, but only /release
// treats that as fatal (hibernateMode.strictDurability): every other caller
// freezes anyway. A VM's fate must not depend on the object store — shutdownAll
// destroys anything it cannot freeze, so a transient GCS error here used to cost
// every running sandbox on the host.
func (s *Server) resetHibernationDurability(ctx context.Context, id string) error {
	if !s.durabilityEnabled() {
		return nil
	}
	markErr := s.markHibNotAdoptable(id)
	var failed error
	for _, object := range []string{
		hibRecordObj(id),
		hibManifestObj(id),
		hibWorkingSetObj(id),
		hibMemDiffObj(id),
		hibStateObj(id),
		hibRootfsObj(id),
	} {
		if err := s.deleteDurableObject(ctx, object); err != nil && failed == nil {
			failed = fmt.Errorf("delete stale %s: %w", object, err)
		}
	}
	if failed != nil {
		if markErr != nil {
			return fmt.Errorf("%w (and the local not-adoptable marker could not be written: %v)", failed, markErr)
		}
		return failed
	}
	// Nothing stale remains; the marker would otherwise make the freeze's own
	// upload look untrustworthy.
	s.clearHibNotAdoptable(id)
	return nil
}

// invalidateHibernationRecord makes the cross-host commit marker non-adoptable.
// The current manifest may still accelerate the imminent same-host UFFD wake, so
// only record.json is targeted.
//
// Either mechanism alone is sufficient, so this fails ONLY when both are
// impossible — at which point non-resurrection genuinely cannot be guaranteed and
// aborting the caller is the safe answer. In particular an unreachable object
// store no longer fails it: callers are wake, adopt, and destroy, and a
// hibernated sandbox that cannot be deleted (or an expired one the TTL reaper
// retries every 10 s forever) is a far worse outcome than a deferred delete that
// drainHibInvalidations completes.
func (s *Server) invalidateHibernationRecord(ctx context.Context, id string) error {
	if !s.durabilityEnabled() {
		return nil
	}
	// Marker first: a crash between the two leaves the safe answer ("stale").
	markErr := s.markHibNotAdoptable(id)
	err := s.deleteDurableObject(ctx, hibRecordObj(id))
	if err == nil {
		s.clearHibNotAdoptable(id)
		return nil
	}
	if markErr == nil {
		fmt.Fprintf(os.Stderr, "[%s] durable hibernation record delete deferred (marked not adoptable locally): %v\n", id, err)
		return nil
	}
	return fmt.Errorf("cannot make the durable record for %s non-adoptable (store: %w; local marker: %v)", id, err, markErr)
}

// deleteHibernationObjects is called only after the uploader has been joined.
// The record is already gone synchronously; these payload deletions are cleanup.
func (s *Server) deleteHibernationObjects(id string) {
	if s.blob == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, object := range []string{
		hibManifestObj(id),
		hibWorkingSetObj(id),
		hibMemDiffObj(id),
		hibStateObj(id),
		hibRootfsObj(id),
		hibOwnerObj(id),
	} {
		if err := s.blob.Delete(ctx, object); err != nil {
			fmt.Fprintf(os.Stderr, "[%s] durable hibernate cleanup %s: %v\n", id, object, err)
		}
	}
}
