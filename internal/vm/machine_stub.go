//go:build !linux

package vm

import (
	"context"
	"errors"
)

// ErrLinuxOnly is returned on non-Linux platforms.
var ErrLinuxOnly = errors.New("firecracker requires Linux with /dev/kvm")

// Machine is a placeholder on non-Linux platforms.
type Machine struct{}

// NewMachine returns ErrLinuxOnly on non-Linux platforms.
func NewMachine(_ context.Context, _ RunOptions) (*Machine, RuntimeConfig, error) {
	return nil, RuntimeConfig{}, ErrLinuxOnly
}

// Start returns ErrLinuxOnly on non-Linux platforms.
func Start(_ context.Context, _ *Machine) error { return ErrLinuxOnly }

// StopForce returns ErrLinuxOnly on non-Linux platforms.
func StopForce(_ *Machine) error { return ErrLinuxOnly }

// Wait returns ErrLinuxOnly on non-Linux platforms.
func Wait(_ context.Context, _ *Machine) error { return ErrLinuxOnly }
