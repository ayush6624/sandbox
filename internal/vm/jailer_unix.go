//go:build unix

package vm

import (
	"fmt"
	"os"
	"syscall"
)

func statUID(info os.FileInfo) int {
	return int(info.Sys().(*syscall.Stat_t).Uid)
}

func jailerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

func pathDevice(path string) (uint64, error) {
	st, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	stat, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("stat device unavailable for %s", path)
	}
	return uint64(stat.Dev), nil
}
