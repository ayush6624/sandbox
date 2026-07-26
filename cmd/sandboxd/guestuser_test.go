package main

import (
	"slices"
	"testing"
)

func TestGuestEnvironmentReplacesRootIdentity(t *testing.T) {
	env := guestEnvironment([]string{
		"PATH=/usr/bin",
		"HOME=/root",
		"USER=root",
		"LOGNAME=root",
		"KEEP=yes",
	})
	for _, want := range []string{
		"HOME=/home/sandbox",
		"USER=sandbox",
		"LOGNAME=sandbox",
		"KEEP=yes",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("guest environment missing %q: %v", want, env)
		}
	}
	for _, forbidden := range []string{"HOME=/root", "USER=root", "LOGNAME=root"} {
		if slices.Contains(env, forbidden) {
			t.Errorf("guest environment retained %q: %v", forbidden, env)
		}
	}
}
