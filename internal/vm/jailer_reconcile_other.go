//go:build !linux

package vm

import "fmt"

type JailerReconcileResult struct {
	ProcessesTerminated int
	JailsRemoved        int
	IdentitiesReleased  int
	CgroupsRemoved      int
}

func ReconcileJailer(JailerConfig) (JailerReconcileResult, error) {
	return JailerReconcileResult{}, fmt.Errorf("jailer reconciliation requires Linux")
}
