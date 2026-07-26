//go:build !linux

package vm

import "fmt"

func backingDevice(string) (string, error) {
	return "", fmt.Errorf("backing-device resolution requires Linux")
}
