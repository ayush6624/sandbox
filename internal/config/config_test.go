package config

import "testing"

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
