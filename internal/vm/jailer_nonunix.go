//go:build !unix

package vm

import (
	"os"
	"syscall"
)

func statUID(os.FileInfo) int                 { return -1 }
func jailerSysProcAttr() *syscall.SysProcAttr { return nil }
func pathDevice(string) (uint64, error)       { return 0, syscall.ENOTSUP }
