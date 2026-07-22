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

func NewMachine(_ context.Context, _ RunOptions, _ bool) (*Machine, RuntimeConfig, error) {
	return nil, RuntimeConfig{}, ErrLinuxOnly
}

func NewMachineFromSnapshot(_ context.Context, _ RunOptions, _, _ string, _ bool) (*Machine, RuntimeConfig, error) {
	return nil, RuntimeConfig{}, ErrLinuxOnly
}

func StartClone(_ context.Context, _ RunOptions, _ CloneParams) (*Machine, RuntimeConfig, error) {
	return nil, RuntimeConfig{}, ErrLinuxOnly
}

func RestoreUFFD(_ context.Context, _ RunOptions, _, _ string) (*Machine, RuntimeConfig, error) {
	return nil, RuntimeConfig{}, ErrLinuxOnly
}

func Start(_ context.Context, _ *Machine) error                    { return ErrLinuxOnly }
func StopForce(_ *Machine) error                                   { return ErrLinuxOnly }
func ShutdownGuest(_ context.Context, _ *Machine) error            { return ErrLinuxOnly }
func Wait(_ context.Context, _ *Machine) error                     { return ErrLinuxOnly }
func PID(_ *Machine) (int, error)                                  { return 0, ErrLinuxOnly }
func Pause(_ context.Context, _ *Machine) error                    { return ErrLinuxOnly }
func Resume(_ context.Context, _ *Machine) error                   { return ErrLinuxOnly }
func Snapshot(_ context.Context, _ *Machine, _, _, _ string) error { return ErrLinuxOnly }
func PushEpoch(_ context.Context, _ string) error                  { return ErrLinuxOnly }
func DiffCapable(_ *Machine) bool                                  { return false }
func SealUFFDRecording(_ *Machine)                                 {}
func UFFDWorkingSet(_ *Machine) []uint64                           { return nil }
