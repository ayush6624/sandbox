package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestOwnedHandoffStorageRequiresChunkBucket(t *testing.T) {
	for _, tc := range []struct {
		name, data     string
		valid, enabled bool
	}{
		{"default", `{}`, true, false},
		{"bucket missing", `{"owned_handoff_storage":true,"uffd_chunk_gcs":true}`, false, false},
		{"chunks disabled", `{"owned_handoff_storage":true,"snapshot_bucket":"test"}`, false, false},
		{"enabled", `{"owned_handoff_storage":true,"uffd_chunk_gcs":true,"snapshot_bucket":"test"}`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.json")
			if err := os.WriteFile(path, []byte(tc.data), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := Load(path)
			if (err == nil) != tc.valid {
				t.Fatalf("config validity: %v", err)
			}
			if err == nil && cfg.OwnedHandoffStorage != tc.enabled {
				t.Fatal("incorrect writing gate")
			}
		})
	}
}
