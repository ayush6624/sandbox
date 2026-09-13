package vm

import (
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

// A reaped VMM can still have kernel teardown work keeping its cgroup busy.
// Removing the leaf, rather than ignoring its limit, releases the reservation.
func removeVMMCgroup(path string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := os.Remove(path)
		if err == nil || errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if !errors.Is(err, syscall.EBUSY) || !time.Now().Before(deadline) {
			return fmt.Errorf("remove VM cgroup %s: %w", path, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
