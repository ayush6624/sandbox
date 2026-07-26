//go:build !linux

package main

import "os/exec"

// sandboxd is deployed only inside a Linux guest. These stubs keep host-side
// unit tests buildable on development machines.
func configureGuestCommandProcess(_ *exec.Cmd) error {
	return nil
}

func withGuestFilesystem(fn func() error) error {
	return fn()
}
