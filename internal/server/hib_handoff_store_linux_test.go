//go:build linux

package server

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestPrepareLocalHandoffRequiresNoCloudPayloadOrMetadata(t *testing.T) {
	s, _, sb, files := seedPeerRelease(t)
	// A nil cloud client makes an accidental fetch fail immediately. The local
	// snapshot and registry are sufficient to capture a planned handoff.
	s.blob = nil
	_, state, _, err := s.cfg.Provisioner.SnapshotPaths(hibID(sb.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(state), "working-set.json"), []byte(`[0,1,9999]`), 0600); err != nil {
		t.Fatal(err)
	}
	d, err := s.prepareLocalHandoff(context.Background(), sb)
	if err != nil {
		t.Fatal(err)
	}
	if d.Record.Generation != d.Ref.Generation || d.Manifest.Version != 2 || len(d.WorkingSet) != 1 || d.WorkingSet[0] != 0 {
		t.Fatalf("local descriptor = %+v", d)
	}
	for path, want := range files {
		got, err := os.ReadFile(path)
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("preparation touched original capture %s: %v", path, err)
		}
	}
	if _, err := s.reg.Get(context.Background(), sb.ID); err != nil {
		t.Fatalf("preparation released row before journal commit: %v", err)
	}
	if err := s.removePeerHibernation(d.Ref.Generation); err != nil {
		t.Fatal(err)
	}
}
