package server

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestBuildHibRecordDiffFreeze(t *testing.T) {
	exp := time.Unix(1_800_000_000, 0)
	sb := registry.Sandbox{
		ID:                "sb-1",
		Name:              "devbox",
		Vcpus:             2,
		MemMIB:            1024,
		CreatedAt:         time.Unix(1_700_000_000, 0),
		ExpiresAt:         &exp,
		HibernateAfterSec: 300,
		BaseSnapshotID:    "golden-abc",
		// host-side identity must NOT leak into the record:
		TapDevice: "fc-tap-1", GuestIP: "172.16.0.5", RootfsPath: "/opt/fc/rootfs-sb-1.ext4",
	}
	extras := []registry.PortMapping{
		{GuestPort: 8080, HostPort: 41001},
		{GuestPort: 5432, HostPort: 41002},
		{GuestPort: 22, PublicPort: 20000},
	}

	rec := buildHibRecord(sb, extras, memFormDiff, "golden-abc", rootfsFormDiff, "golden-abc")

	if rec.Version != hibRecordVersion || rec.ID != "sb-1" || rec.Name != "devbox" {
		t.Fatalf("identity fields wrong: %+v", rec)
	}
	if rec.Vcpus != 2 || rec.MemMIB != 1024 {
		t.Fatalf("resources wrong: %+v", rec)
	}
	if rec.MemForm != memFormDiff || rec.MemBaseID != "golden-abc" {
		t.Fatalf("mem form wrong: %+v", rec)
	}
	if rec.RootfsForm != rootfsFormDiff || rec.RootfsBaseID != "golden-abc" {
		t.Fatalf("rootfs form wrong: %+v", rec)
	}
	if len(rec.GuestPorts) != 2 || rec.GuestPorts[0] != 8080 || rec.GuestPorts[1] != 5432 {
		t.Fatalf("guest ports wrong: %v", rec.GuestPorts)
	}
	if len(rec.URLGuestPorts) != 1 || rec.URLGuestPorts[0] != 22 ||
		len(rec.PublicPorts) != 1 || rec.PublicPorts[0] != (hibPublicPort{GuestPort: 22, PublicPort: 20000}) {
		t.Fatalf("public ports wrong: url=%v raw=%v", rec.URLGuestPorts, rec.PublicPorts)
	}
	if rec.ExpiresAtUnix == nil || *rec.ExpiresAtUnix != exp.Unix() {
		t.Fatalf("expiry not carried: %+v", rec.ExpiresAtUnix)
	}
	if rec.CreatedAtUnix != sb.CreatedAt.Unix() {
		t.Fatalf("created_at not carried: %d", rec.CreatedAtUnix)
	}

	// Host-side identity must be absent from the serialized record — the adopting
	// host allocates fresh tap/IP/port from its own pools.
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	for _, leak := range []string{"fc-tap-1", "172.16.0.5", "41000", "41001", "rootfs-sb-1"} {
		if strings.Contains(string(b), leak) {
			t.Fatalf("record leaked host-side identity %q: %s", leak, b)
		}
	}
}

func TestBuildHibRecordFullColdBoot(t *testing.T) {
	sb := registry.Sandbox{
		ID:        "sb-2",
		Vcpus:     1,
		MemMIB:    512,
		CreatedAt: time.Unix(1_700_000_000, 0),
		// no BaseSnapshotID (cold boot), no expiry
	}
	rec := buildHibRecord(sb, nil, memFormChunked, "", rootfsFormFull, "")

	if rec.MemForm != memFormChunked || rec.MemBaseID != "" {
		t.Fatalf("mem form wrong: %+v", rec)
	}
	if rec.RootfsForm != rootfsFormFull || rec.RootfsBaseID != "" {
		t.Fatalf("rootfs form wrong: %+v", rec)
	}
	if rec.ExpiresAtUnix != nil {
		t.Fatalf("expected no expiry, got %+v", rec.ExpiresAtUnix)
	}
	if len(rec.GuestPorts) != 0 {
		t.Fatalf("expected no ports, got %v", rec.GuestPorts)
	}

	// Round-trip: a chunked-mem cold-boot record must survive JSON byte-for-byte
	// (the marker the far host reads back must decode to the same record).
	b, err := json.Marshal(rec)
	if err != nil {
		t.Fatal(err)
	}
	var got hibRecord
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	b2, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(b2) {
		t.Fatalf("round-trip mismatch:\n got %s\nwant %s", b2, b)
	}
}

// --- deferred durability invalidation (object-store outage) ---

// erroringStore builds a server whose durability store rejects every delete,
// with a real on-disk snapshot directory for the not-adoptable markers.
func erroringStore(t *testing.T, fail *bool) *Server {
	t.Helper()
	s := &Server{cfg: Config{Provisioner: &provisioner.Provisioner{SnapshotDir: t.TempDir()}}}
	s.hibUploads = map[string]*backgroundUpload{}
	s.deleteObject = func(context.Context, string) error {
		if *fail {
			return errors.New("gcs unavailable")
		}
		return nil
	}
	return s
}

// An unreachable object store must not fail a wake or a destroy. It used to:
// invalidateHibernationRecord propagated the delete error, so a GCS blip made a
// hibernated sandbox undeletable (and the TTL reaper retried it every 10 s), and
// resetHibernationDurability's error reached shutdownAll, which reads a failed
// hibernate as licence to DESTROY every running sandbox on the host.
func TestDurabilityOutageDoesNotGateVMLifecycle(t *testing.T) {
	failing := true
	s := erroringStore(t, &failing)
	ctx := context.Background()

	if err := s.invalidateHibernationRecord(ctx, "sb-1"); err != nil {
		t.Fatalf("record invalidation must not fail on an object-store error: %v", err)
	}
	// ...but the generation must be recorded as non-adoptable, or a stale record
	// could be mistaken for a live one.
	if !s.hibNotAdoptable("sb-1") {
		t.Fatal("deferred invalidation left no local not-adoptable marker")
	}
	if _, err := s.fetchHibRecord(ctx, "sb-1"); err == nil {
		t.Fatal("a marked-stale record must not be adoptable/releasable on this host")
	}

	// The freeze-time reset still reports the failure — that is what the strict
	// /release path keys on — but it, too, marks the generation locally so the
	// best-effort callers can safely ignore it.
	if err := s.resetHibernationDurability(ctx, "sb-2"); err == nil {
		t.Fatal("resetHibernationDurability must report an object-store failure")
	}
	if !s.hibNotAdoptable("sb-2") {
		t.Fatal("failed durability reset left no local not-adoptable marker")
	}
}

// The deferred deletes must actually happen once the store recovers: after a
// destroy there is no local row left, so a surviving record.json would let some
// other host adopt — resurrect — a sandbox the user deleted.
func TestDrainHibInvalidationsCompletesOnceStoreRecovers(t *testing.T) {
	failing := true
	s := erroringStore(t, &failing)
	ctx := context.Background()

	if err := s.invalidateHibernationRecord(ctx, "sb-1"); err != nil {
		t.Fatal(err)
	}
	s.drainHibInvalidations(ctx)
	if !s.hibNotAdoptable("sb-1") {
		t.Fatal("drain cleared the marker while the store was still failing")
	}

	failing = false
	s.drainHibInvalidations(ctx)
	if s.hibNotAdoptable("sb-1") {
		t.Fatal("drain did not complete the deferred invalidation after recovery")
	}
}

// A drain must never delete the commit marker of a generation currently being
// published: uploadHibernation clears the marker immediately before writing
// record.json, so a concurrent drain could otherwise strip the durability of a
// perfectly current freeze.
func TestDrainHibInvalidationsSkipsInFlightUpload(t *testing.T) {
	failing := false
	s := erroringStore(t, &failing)
	deletes := 0
	s.deleteObject = func(context.Context, string) error { deletes++; return nil }

	if err := s.markHibNotAdoptable("sb-1"); err != nil {
		t.Fatal(err)
	}
	s.hibUploads["sb-1"] = &backgroundUpload{cancel: func() {}, done: make(chan struct{})}
	s.drainHibInvalidations(context.Background())
	if deletes != 0 {
		t.Fatalf("drain deleted a record with an upload in flight (%d deletes)", deletes)
	}
	if !s.hibNotAdoptable("sb-1") {
		t.Fatal("drain cleared the marker of an in-flight upload")
	}
}

// Without a durability store there is nothing to invalidate, and no marker may
// be left lying around to make a future adopt refuse.
func TestInvalidationIsANoOpWithoutADurabilityStore(t *testing.T) {
	s := &Server{cfg: Config{Provisioner: &provisioner.Provisioner{SnapshotDir: t.TempDir()}}}
	if err := s.invalidateHibernationRecord(context.Background(), "sb-1"); err != nil {
		t.Fatalf("no-store invalidation: %v", err)
	}
	if s.hibNotAdoptable("sb-1") {
		t.Fatal("marker written with no durability store configured")
	}
}

// The chunk-generation stamp must live in the per-freeze hibernation directory
// (so CleanupSnapshot takes it with the generation it describes) and must resolve
// identically for a rebased diff mem, which materializeHibMem writes beside the
// diff under a different file name. If it ever moved outside that directory, a
// superseded hib/<id>/manifest.json — the object name is stable across freezes —
// could supply a woken guest's pages.
func TestHibChunkMarkerIsScopedToOneFreeze(t *testing.T) {
	freeze := filepath.Join(t.TempDir(), "hib-sb-1")
	mem := filepath.Join(freeze, "mem.bin")
	rebasedDiff := filepath.Join(freeze, "mem.full.bin")

	if got := filepath.Dir(hibChunkMarker(mem)); got != freeze {
		t.Fatalf("chunk marker escaped the freeze directory: %s", got)
	}
	if hibChunkMarker(mem) != hibChunkMarker(rebasedDiff) {
		t.Fatalf("rebased diff mem resolves to a different chunk marker: %s vs %s",
			hibChunkMarker(rebasedDiff), hibChunkMarker(mem))
	}
	if hibChunkMarker(mem) == hibDiffMarker(mem) {
		t.Fatal("chunk and diff-base markers must be distinct files")
	}
}
