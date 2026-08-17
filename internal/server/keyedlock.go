package server

import "sync"

// keyedMutexes hands out one mutex per key — a sandbox id, a snapshot id, a
// staged rootfs path — and reclaims the entry once nothing references it.
//
// The previous implementation was a plain map that only ever grew: every create
// mints a fresh uuid, so a long-lived worker retained one mutex per sandbox it
// had EVER seen (confirmed: 10 000 entries after 10 000 completed sandboxes),
// and the same for every snapshot id and GCS pull key. Reclaiming them is safe
// because an entry is only removed when its reference count is zero, i.e. when
// no goroutine holds the mutex or is waiting to: while any holder or waiter
// exists, everyone acquiring that key still gets the identical *keyedMutex, so
// mutual exclusion is unchanged.
//
// Contract: acquire() returns an UNLOCKED handle whose Lock/Unlock must be
// called exactly once each. Unlock releases the mutex AND drops the reference.
type keyedMutexes struct {
	mu      sync.Mutex
	entries map[string]*keyedMutex
}

// keyedMutex is one key's mutex plus the reference count that keeps it in the
// owning map. It intentionally mirrors the *sync.RWMutex API so call sites read
// the same as before; keys used only exclusively simply never call RLock.
type keyedMutex struct {
	owner *keyedMutexes
	key   string
	mu    sync.RWMutex

	// refs is guarded by owner.mu, never by mu: it is incremented before the
	// caller blocks on Lock and decremented after it releases, so it counts
	// holders AND waiters.
	refs int
}

// acquire reserves the mutex for key and returns it unlocked.
func (k *keyedMutexes) acquire(key string) *keyedMutex {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.entries == nil {
		k.entries = map[string]*keyedMutex{}
	}
	e := k.entries[key]
	if e == nil {
		e = &keyedMutex{owner: k, key: key}
		k.entries[key] = e
	}
	e.refs++
	return e
}

// len reports how many keys are currently referenced. Test/diagnostic only.
func (k *keyedMutexes) len() int {
	k.mu.Lock()
	defer k.mu.Unlock()
	return len(k.entries)
}

func (e *keyedMutex) Lock() { e.mu.Lock() }

// RLock takes the key in shared mode. Concurrent readers proceed together and
// still exclude every Lock holder, which is what the snapshot consumers need:
// any number of restores/fanouts may read one snapshot at once, but none of
// them may overlap a delete or a metadata write.
func (e *keyedMutex) RLock() { e.mu.RLock() }

// Unlock releases the mutex and drops this acquisition's reference, removing
// the entry when it was the last one.
func (e *keyedMutex) Unlock() {
	e.mu.Unlock()
	e.release()
}

// RUnlock releases a shared acquisition and drops its reference.
func (e *keyedMutex) RUnlock() {
	e.mu.RUnlock()
	e.release()
}

func (e *keyedMutex) release() {
	k := e.owner
	k.mu.Lock()
	e.refs--
	if e.refs <= 0 && k.entries[e.key] == e {
		delete(k.entries, e.key)
	}
	k.mu.Unlock()
}
