//go:build !linux

package vm

// SampleUsage has no non-Linux implementation: there is no cgroup v2 and no
// /proc to read. Callers already treat a sampling error as "keep the last
// heartbeat value", so metering degrades to allocated-only on a dev machine
// rather than failing a lifecycle operation.
func SampleUsage(m *Machine) (UsageSample, error) {
	return UsageSample{}, ErrLinuxOnly
}
