package server

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/ayush6624/sandbox/internal/provisioner"
	"github.com/ayush6624/sandbox/internal/registry"
	"github.com/ayush6624/sandbox/internal/vm"
)

func TestGoldenManifestRequiresMatchingIsolationSignature(t *testing.T) {
	dir := t.TempDir()
	s := &Server{cfg: Config{
		Provisioner: &provisioner.Provisioner{SnapshotDir: dir},
		VMTemplate:  vm.RunOptions{Jailer: &vm.JailerConfig{}},
	}}
	snap := registry.Snapshot{ID: "golden-1"}
	for name, tc := range map[string]struct {
		signature string
		want      bool
	}{
		"matching jailed layout": {"jailer-v1", true},
		"legacy missing layout":  {"", false},
		"direct layout":          {"direct-v1", false},
	} {
		t.Run(name, func(t *testing.T) {
			body, err := json.Marshal(goldenManifest{Snapshot: snap, IsolationSignature: tc.signature})
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, "golden.json"), body, 0600); err != nil {
				t.Fatal(err)
			}
			if got := s.goldenManifestMatches(snap); got != tc.want {
				t.Fatalf("goldenManifestMatches = %v, want %v", got, tc.want)
			}
		})
	}
}
