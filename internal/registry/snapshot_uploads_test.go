package registry

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func capturedTestSnapshot(id string) Snapshot {
	return Snapshot{ID: id, SourceID: "source", CreatedAt: time.Now(), MemPath: "/snap/" + id + "/mem"}
}

func TestCapturedSnapshotAcceptanceIsAtomic(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if _, err := r.db.Exec(`CREATE TRIGGER reject_upload BEFORE INSERT ON snapshot_uploads
		BEGIN SELECT RAISE(ABORT, 'injected upload failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateCapturedSnapshot(ctx, capturedTestSnapshot("capture")); err == nil {
		t.Fatal("capture succeeded without its upload obligation")
	}
	if _, err := r.GetSnapshot(ctx, "capture"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("snapshot survived failed transaction: %v", err)
	}
	if _, err := r.db.Exec(`DROP TRIGGER reject_upload`); err != nil {
		t.Fatal(err)
	}
	if err := r.CreateCapturedSnapshot(ctx, capturedTestSnapshot("capture")); err != nil {
		t.Fatal(err)
	}
	snap, err := r.GetSnapshot(ctx, "capture")
	if err != nil || snap.Durability != "local" || snap.Upload == nil || snap.Upload.State != "pending" || snap.Upload.Attempts != 0 || snap.Upload.NextAttemptAt == nil {
		t.Fatalf("accepted snapshot = %+v, error %v", snap, err)
	}
	if err := r.CreateCapturedSnapshot(ctx, capturedTestSnapshot("capture")); err == nil {
		t.Fatal("duplicate capture overwrote the existing snapshot")
	}
}

func TestImportedSnapshotsNeverAcquireUploadIntent(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	for _, durability := range []string{"local", "durable"} {
		snap := capturedTestSnapshot(durability)
		snap.Durability = durability
		snap.Upload = &SnapshotUpload{State: "retrying", Attempts: 7, Error: "foreign job"}
		if err := r.CreateSnapshot(ctx, snap); err != nil {
			t.Fatal(err)
		}
		got, err := r.GetSnapshot(ctx, snap.ID)
		if err != nil || got.Upload != nil || got.Durability != durability {
			t.Fatalf("import = %+v, error %v", got, err)
		}
	}
	ids, err := r.DueSnapshotUploads(ctx, time.Now().Add(time.Hour), 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("foreign imports became jobs: %v, %v", ids, err)
	}
}

func TestSnapshotUploadRecoveryAndTransitions(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "registry.db")
	r, err := Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close() })
	now := time.Now().Add(time.Second).Truncate(time.Millisecond)
	if err := r.CreateCapturedSnapshot(ctx, capturedTestSnapshot("capture")); err != nil {
		t.Fatal(err)
	}
	first, err := r.BeginSnapshotUpload(ctx, "capture", now)
	if err != nil || first.State != "uploading" || first.Attempts != 1 {
		t.Fatalf("first attempt = %+v, %v", first, err)
	}
	if err := r.Close(); err != nil {
		t.Fatal(err)
	}
	r, err = Open(path, Pools{})
	if err != nil {
		t.Fatal(err)
	}
	ids, err := r.DueSnapshotUploads(ctx, now, 1)
	if err != nil || !reflect.DeepEqual(ids, []string{"capture"}) {
		t.Fatalf("interrupted attempt disappeared: %v, %v", ids, err)
	}
	second, err := r.BeginSnapshotUpload(ctx, "capture", now)
	if err != nil || second.Attempts != 2 {
		t.Fatalf("recovered attempt = %+v, %v", second, err)
	}
	next := now.Add(time.Minute)
	if err := r.RetrySnapshotUpload(ctx, "capture", next, "object store unavailable"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BeginSnapshotUpload(ctx, "capture", now); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claimed attempt before retry deadline: %v", err)
	}
	ids, err = r.DueSnapshotUploads(ctx, now, 10)
	if err != nil || len(ids) != 0 {
		t.Fatalf("early due attempts = %v, %v", ids, err)
	}
	snaps, err := r.ListSnapshots(ctx)
	if err != nil || len(snaps) != 1 || snaps[0].Upload == nil {
		t.Fatalf("list projection = %+v, %v", snaps, err)
	}
	status := snaps[0].Upload
	if status.State != "retrying" || status.Attempts != 2 || status.Error != "object store unavailable" || status.NextAttemptAt == nil || !status.NextAttemptAt.Equal(next) {
		t.Fatalf("retry projection = %+v", status)
	}
	if _, err := r.BeginSnapshotUpload(ctx, "capture", next); err != nil {
		t.Fatal(err)
	}
	if err := r.FailSnapshotUpload(ctx, "capture", "memory artifact missing"); err != nil {
		t.Fatal(err)
	}
	if _, err := r.BeginSnapshotUpload(ctx, "capture", next); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("claimed terminal failure: %v", err)
	}
	snap, err := r.GetSnapshot(ctx, "capture")
	if err != nil || snap.Upload == nil || snap.Upload.State != "failed" || snap.Upload.Error != "memory artifact missing" || snap.Upload.NextAttemptAt != nil || snap.Durability != "local" {
		t.Fatalf("failed projection = %+v, %v", snap, err)
	}
	if err := r.DeleteSnapshot(ctx, "capture"); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := r.rdb.QueryRow(`SELECT COUNT(*) FROM snapshot_uploads`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("delete did not cascade jobs: %d, %v", count, err)
	}
	if err := r.CompleteSnapshotUpload(ctx, "capture"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completion resurrected deleted snapshot: %v", err)
	}
}

func TestSnapshotUploadCompletionIsAtomic(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	if err := r.CreateCapturedSnapshot(ctx, capturedTestSnapshot("capture")); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteSnapshotUpload(ctx, "capture"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("completed a never-started upload: %v", err)
	}
	if _, err := r.BeginSnapshotUpload(ctx, "capture", time.Now().Add(time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, err := r.db.Exec(`CREATE TRIGGER reject_upload_completion BEFORE DELETE ON snapshot_uploads
		BEGIN SELECT RAISE(ABORT, 'injected completion failure'); END`); err != nil {
		t.Fatal(err)
	}
	if err := r.CompleteSnapshotUpload(ctx, "capture"); err == nil {
		t.Fatal("completion succeeded while job deletion failed")
	}
	snap, err := r.GetSnapshot(ctx, "capture")
	if err != nil || snap.Durability != "local" || snap.Upload == nil || snap.Upload.State != "uploading" {
		t.Fatalf("partial completion escaped rollback: %+v, %v", snap, err)
	}
	if _, err := r.db.Exec(`DROP TRIGGER reject_upload_completion`); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.CompleteSnapshotUpload(ctx, "capture"); err != nil {
			t.Fatal(err)
		}
	}
	snap, err = r.GetSnapshot(ctx, "capture")
	if err != nil || snap.Durability != "durable" || snap.Upload != nil {
		t.Fatalf("completion = %+v, %v", snap, err)
	}
}

func TestRetireGoldenPreservesReferencedBase(t *testing.T) {
	r, ctx := testRegistry(t), context.Background()
	base := capturedTestSnapshot("base")
	base.Golden, base.WarmTarget = true, 8
	if err := r.CreateCapturedSnapshot(ctx, base); err == nil {
		t.Fatal("golden acquired a user upload obligation")
	}
	if err := r.CreateSnapshot(ctx, base); err != nil {
		t.Fatal(err)
	}
	child := capturedTestSnapshot("child")
	child.Format, child.BaseID = FormatDiff, base.ID
	if err := r.CreateCapturedSnapshot(ctx, child); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := r.RetireGoldenSnapshot(ctx, base.ID); err != nil {
			t.Fatal(err)
		}
	}
	retired, err := r.GetSnapshot(ctx, base.ID)
	if err != nil || retired.Golden || retired.Role != SnapshotRoleBase || retired.WarmTarget != 0 || retired.MemPath != base.MemPath || retired.Upload != nil {
		t.Fatalf("retired base = %+v, %v", retired, err)
	}
	if _, err := r.GoldenSnapshot(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retired base selected as golden: %v", err)
	}
	if _, err := r.SetSnapshotWarmTarget(ctx, base.ID, 1); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("retired base accepted a warm target: %v", err)
	}
	if err := r.DeleteSnapshot(ctx, base.ID); !errors.Is(err, ErrSnapshotInUse) {
		t.Fatalf("referenced base lost its dependency guard: %v", err)
	}
	replacement := capturedTestSnapshot("replacement")
	replacement.Golden = true
	if err := r.CreateSnapshot(ctx, replacement); err != nil {
		t.Fatalf("retirement did not free golden selection: %v", err)
	}
	golden, err := r.GoldenSnapshot(ctx)
	if err != nil || golden.ID != replacement.ID {
		t.Fatalf("replacement golden = %+v, %v", golden, err)
	}
}
