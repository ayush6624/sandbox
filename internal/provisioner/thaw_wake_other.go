//go:build !linux

package provisioner

import "fmt"

func WakeThawAgent(tap string) error {
	return fmt.Errorf("thaw wake on %s requires linux", tap)
}
