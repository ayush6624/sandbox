package registry

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
)

func TestWarmClaimsAreTemplateExact(t *testing.T) {
	r, err := Open(filepath.Join(t.TempDir(), "registry.db"), Pools{
		TapPrefix: "fc", TapMax: 4, GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.13",
		PortMin: 5200, PortMax: 5203,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	ctx := context.Background()
	if _, err := r.CreateWarmForTemplate(ctx, "warm-a", "/tmp/a.ext4", "", "template-a", 0, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.CreateWarmForTemplate(ctx, "warm-b", "/tmp/b.ext4", "", "template-b", 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkWarmReady(ctx, "warm-a"); err != nil {
		t.Fatal(err)
	}
	if err := r.MarkWarmReady(ctx, "warm-b"); err != nil {
		t.Fatal(err)
	}

	claimed, err := r.ClaimWarmForTemplate(ctx, "template-b", "claimed-b", nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if claimed.ID != "warm-b" {
		t.Fatalf("claimed %s, want warm-b", claimed.ID)
	}
	if _, err := r.ClaimWarmForTemplate(ctx, "template-b", "", nil, 0); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("second template-b claim = %v, want sql.ErrNoRows", err)
	}
	counts, err := r.WarmCountByTemplate(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if counts["template-a"] != 1 || counts["template-b"] != 0 {
		t.Fatalf("warm counts = %#v", counts)
	}
}
