package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestGuestSubnetBitsDefault(t *testing.T) {
	// Unset (0) must default to a /24 so existing configs are unchanged.
	var c Config
	c.Defaults()
	if c.GuestSubnetBits != 24 {
		t.Fatalf("GuestSubnetBits default = %d, want 24", c.GuestSubnetBits)
	}
}

func TestGuestSubnetBitsExplicitPreserved(t *testing.T) {
	// A widened subnet set in the config must survive Defaults().
	c := Config{GuestSubnetBits: 22}
	c.Defaults()
	if c.GuestSubnetBits != 22 {
		t.Fatalf("GuestSubnetBits = %d, want 22 (explicit value clobbered)", c.GuestSubnetBits)
	}
}

func TestLoadPlacementDelaySec(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"placement_delay_sec":210}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if cfg.PlacementDelaySec != 210 {
		t.Fatalf("PlacementDelaySec = %d, want 210", cfg.PlacementDelaySec)
	}
}

func TestLoadRejectsNegativePlacementDelay(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"placement_delay_sec":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "placement_delay_sec must be >= 0") {
		t.Fatalf("Load error = %v, want placement-delay validation", err)
	}
}
