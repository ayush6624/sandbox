package server

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	extras := []registry.PortMapping{{GuestPort: 8080, HostPort: 41001}, {GuestPort: 5432, HostPort: 41002}}

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
