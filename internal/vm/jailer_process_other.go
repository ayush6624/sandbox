//go:build !linux

package vm

import "fmt"

func validateJailedProcess(int, int, string) error {
	return fmt.Errorf("jailed process validation requires Linux")
}
