//go:build !linux

package main

// raiseNoFileLimit is a no-op off Linux; the server only runs VMs on Linux.
func raiseNoFileLimit() {}
