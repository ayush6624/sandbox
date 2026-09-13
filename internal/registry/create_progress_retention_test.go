package registry

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestAcknowledgedCreateProgressDoesNotDuplicateSandboxMetadata(t *testing.T) {
	ctx := context.Background()
	reg := testRegistry(t)
	intent := beginOwnedProgress(t, reg, "large-result", CreateProgressLocal)
	intent.Metadata = map[string]string{"large": strings.Repeat("x", 64<<10)}
	advanceProgress(t, reg, intent, CreateStageSource)
	allocateProgress(t, reg, intent)
	if err := reg.MarkRunning(ctx, "sb-"+intent.ID); err != nil {
		t.Fatal(err)
	}
	before := getProgress(t, reg, intent.ID)
	if before.Outcome == nil || before.Outcome.Sandbox == nil || before.Outcome.Sandbox.Metadata["large"] != intent.Metadata["large"] {
		t.Fatal("terminal observation lost the historical metadata")
	}
	if err := reg.AcknowledgeCreateProgress(ctx, intent.ID, before.Sequence); err != nil {
		t.Fatal(err)
	}
	if err := reg.Destroy(ctx, "sb-"+intent.ID); err != nil {
		t.Fatal(err)
	}
	if got := getProgress(t, reg, intent.ID); !reflect.DeepEqual(got, before) {
		t.Fatal("acknowledgement or sandbox deletion changed historical progress")
	}
	replayed, err := reg.CreateRequest(ctx, intent)
	if err != nil || !reflect.DeepEqual(replayed, *before.Outcome) {
		t.Fatalf("historical replay changed: %+v, %v", replayed, err)
	}
	var progressBytes int
	if err := reg.rdb.QueryRowContext(ctx, `SELECT COALESCE(SUM(length(snapshot)),0) FROM create_progress WHERE id=?`, intent.ID).Scan(&progressBytes); err != nil {
		t.Fatal(err)
	}
	if progressBytes > 2048 {
		t.Fatalf("acknowledged progress still retains the full sandbox outcome: %d bytes", progressBytes)
	}
	t.Logf("retained progress snapshot: %d bytes for a 64 KiB sandbox metadata result", progressBytes)
}
