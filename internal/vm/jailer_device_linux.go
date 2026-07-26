//go:build linux

package vm

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func backingDevice(path string) (string, error) {
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	dev := uint64(st.Sys().(*syscall.Stat_t).Dev)
	return fmt.Sprintf("%d:%d", unix.Major(dev), unix.Minor(dev)), nil
}
