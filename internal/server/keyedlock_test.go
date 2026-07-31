package server

import (
	"fmt"
	"testing"
)

// The per-id lifecycle/snapshot lock maps must not retain an entry per sandbox
// forever: a long-lived worker mints a fresh uuid on every create.
func TestKeyedLockMapsDoNotGrowUnbounded(t *testing.T) {
	s := &Server{}
	const n = 5000
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("sandbox-%d", i)
		mu := s.wakeLock(id)
		mu.Lock()
		mu.Unlock()
	}
	if got := s.wakeLockLen(); got != 0 {
		t.Fatalf("wake lock map retained %d of %d entries after every holder released", got, n)
	}
}
