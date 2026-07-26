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

func TestSecurityDefaults(t *testing.T) {
	var c Config
	c.Defaults()
	if c.DisableSeccomp {
		t.Fatal("DisableSeccomp default = true, want secure default false")
	}
	if c.AllowInterGuestNetwork {
		t.Fatal("AllowInterGuestNetwork default = true, want isolated guests")
	}
	if c.FirecrackerLogMaxBytes != DefaultFirecrackerLogMaxBytes {
		t.Fatalf("FirecrackerLogMaxBytes = %d, want %d", c.FirecrackerLogMaxBytes, DefaultFirecrackerLogMaxBytes)
	}
	if c.FirecrackerLogRetentionHours != DefaultFirecrackerLogRetentionHours {
		t.Fatalf("FirecrackerLogRetentionHours = %d, want %d", c.FirecrackerLogRetentionHours, DefaultFirecrackerLogRetentionHours)
	}
	if c.FirecrackerLogMaxFiles != DefaultFirecrackerLogMaxFiles {
		t.Fatalf("FirecrackerLogMaxFiles = %d, want %d", c.FirecrackerLogMaxFiles, DefaultFirecrackerLogMaxFiles)
	}
	if c.VMIsolation != "direct" {
		t.Fatalf("VMIsolation default = %q, want explicit development direct mode", c.VMIsolation)
	}
}

func TestJailerProfileDefaultsAndValidation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"vm_isolation":"jailer"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.JailerBin != "/usr/local/bin/jailer" || cfg.JailerChrootBase != "/mnt/sandbox-data/jailer" {
		t.Fatalf("jailer paths = %q, %q", cfg.JailerBin, cfg.JailerChrootBase)
	}
	if cfg.JailerUIDStart != 200000 || cfg.JailerGIDStart != 200000 || cfg.JailerIdentityCount != 4096 {
		t.Fatalf("jailer identity pool = %d:%d x%d", cfg.JailerUIDStart, cfg.JailerGIDStart, cfg.JailerIdentityCount)
	}
	if cfg.JailerMemoryOverheadMIB <= 0 || cfg.JailerPIDsMax <= 0 || cfg.JailerIOReadBPS <= 0 || cfg.JailerNoFile <= 0 {
		t.Fatalf("jailer resource defaults missing: %+v", cfg)
	}
}

func TestLoadRejectsInsecureJailerProfile(t *testing.T) {
	for _, body := range []string{
		`{"vm_isolation":"unknown"}`,
		`{"vm_isolation":"jailer","disable_seccomp":true}`,
		`{"vm_isolation":"jailer","jailer_cpu_weight":10001}`,
		`{"vm_isolation":"jailer","jailer_pids_max":-1}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("Load(%s) succeeded", body)
		}
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

func TestLoadRejectsNegativeFirecrackerLogLimit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	if err := os.WriteFile(path, []byte(`{"firecracker_log_max_bytes":-1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "firecracker_log_max_bytes must be >= 0") {
		t.Fatalf("Load error = %v, want log-limit validation", err)
	}
}

func TestLoadRejectsNegativeFirecrackerLogRetention(t *testing.T) {
	for _, body := range []string{
		`{"firecracker_log_retention_hours":-1}`,
		`{"firecracker_log_max_files":-1}`,
	} {
		path := filepath.Join(t.TempDir(), "config.json")
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("Load(%s) succeeded, want retention validation", body)
		}
	}
}
