//go:build !linux

package vm

import "fmt"

type JailerReconcileResult struct {
	ProcessesTerminated int
	JailsRemoved        int
	IdentitiesReleased  int
	CgroupsRemoved      int
	// SharedArtifactsRemoved mirrors the Linux field; keep the two in sync.
	SharedArtifactsRemoved int
}

func ReconcileJailer(JailerConfig) (JailerReconcileResult, error) {
	return JailerReconcileResult{}, fmt.Errorf("jailer reconciliation requires Linux")
}
