package main

import (
	"strings"
	"testing"

	"github.com/ayush6624/sandbox/internal/config"
	"github.com/ayush6624/sandbox/internal/registry"
)

func TestValidateWarmPoolConfig(t *testing.T) {
	base := config.Config{Pools: registry.Pools{
		TapPrefix: "fc", TapMax: 4,
		GuestIPMin: "172.16.0.10", GuestIPMax: "172.16.0.13",
	}}
	for _, tc := range []struct {
		name string
		edit func(*config.Config)
		want string
	}{
		{name: "disabled"},
		{name: "valid", edit: func(c *config.Config) { c.WarmPoolSize = 3 }},
		{name: "negative", edit: func(c *config.Config) { c.WarmPoolSize = -1 }, want: "must be >= 0"},
		{name: "without hot create", edit: func(c *config.Config) {
			c.WarmPoolSize, c.DisableHotCreate = 1, true
		}, want: "requires hot create"},
		{name: "consumes all slots", edit: func(c *config.Config) { c.WarmPoolSize = 4 }, want: "must be smaller"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			if tc.edit != nil {
				tc.edit(&cfg)
			}
			err := validateWarmPoolConfig(cfg)
			if tc.want == "" && err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)) {
				t.Fatalf("error=%v, want containing %q", err, tc.want)
			}
		})
	}
}
