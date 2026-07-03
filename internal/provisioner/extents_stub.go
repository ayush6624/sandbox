//go:build !linux

package provisioner

import (
	"errors"
	"os"
)

// DiffExtents on non-Linux returns the whole file as one range (no FIEMAP).
// Only reachable in dev builds — GCS snapshot durability runs on Linux hosts.
func (p *Provisioner) DiffExtents(clonePath, basePath string) ([]Range, error) {
	fi, err := os.Stat(clonePath)
	if err != nil {
		return nil, err
	}
	if fi.Size() == 0 {
		return nil, nil
	}
	return []Range{{Off: 0, Len: fi.Size()}}, nil
}

// OverlaySparse requires SEEK_DATA/SEEK_HOLE semantics we only wire up on
// Linux; diff-snapshot materialization never runs in dev builds.
func (p *Provisioner) OverlaySparse(src, dst string) error {
	return errors.New("sparse overlay not supported on this platform")
}
