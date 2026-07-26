//go:build unix

package vm

import (
	"os"
	"syscall"
)

func statUID(info os.FileInfo) int {
	return int(info.Sys().(*syscall.Stat_t).Uid)
}

func jailerSysProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}
