package api

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
)

// withRigNameLock serializes rig-create admission for one (city, rig name) so
// the live-index read-modify-write for a single name is a critical section
// (G13 §7 / G16). It mirrors sourceworkflow.WithLock's in-process tier — a
// refcounted cap-1 channel token whose map entry is deleted when the last
// waiter releases — so idle names leak no memory, unlike a map[string]*Mutex.
//
// It is deliberately the in-process tier ONLY: admission is process-local by
// construction (the live index is process-local, single-replica accepted,
// G13 §12). A concurrent CLI `gc rig add` in another process is out of this
// lock's scope and is caught by CreateRig's under-lock duplicate guard.
//
// The lock is held for admission only (validate → index → durable fallback →
// collision → record + entry + cursor). The clone/provision runs outside it;
// the byName live entry — not a held lock — excludes same-name work for the
// provision's lifetime.
//
// An empty rig name is an error, not a bypass: unlike sourceworkflow.WithLock,
// which early-returns fn() unlocked on an empty id, this refuses. Rig name is
// already minLength:"1" on the wire plus the G13 validator, so the refusal is
// a programming-error backstop, not a normal path.
var (
	rigNameLocksMu sync.Mutex
	rigNameLocks   = map[string]*rigNameLock{}
)

// rigNameLock is a single refcounted admission token. token has capacity 1: a
// value in the channel means "free"; taking it acquires, returning it releases.
type rigNameLock struct {
	token chan struct{}
	refs  int
}

func withRigNameLock(ctx context.Context, cityPath, rigName string, fn func() error) error {
	if strings.TrimSpace(rigName) == "" {
		return errors.New("rig name lock: empty rig name")
	}
	key := filepath.Clean(strings.TrimSpace(cityPath)) + "\x00" + rigName
	lk := acquireRigNameLock(key)
	defer releaseRigNameLock(key, lk)

	select {
	case <-lk.token:
	case <-ctx.Done():
		return ctx.Err()
	}
	defer func() { lk.token <- struct{}{} }()

	return fn()
}

// acquireRigNameLock returns the shared lock for key, creating it (with its
// token pre-loaded as "free") on first use and bumping the refcount.
func acquireRigNameLock(key string) *rigNameLock {
	rigNameLocksMu.Lock()
	defer rigNameLocksMu.Unlock()
	lk := rigNameLocks[key]
	if lk == nil {
		lk = &rigNameLock{token: make(chan struct{}, 1)}
		lk.token <- struct{}{}
		rigNameLocks[key] = lk
	}
	lk.refs++
	return lk
}

// releaseRigNameLock drops one reference and deletes the map entry when the
// last waiter departs, so idle names hold no memory.
func releaseRigNameLock(key string, lk *rigNameLock) {
	rigNameLocksMu.Lock()
	defer rigNameLocksMu.Unlock()
	cur := rigNameLocks[key]
	if cur == nil || cur != lk {
		return
	}
	if cur.refs > 0 {
		cur.refs--
	}
	if cur.refs == 0 {
		delete(rigNameLocks, key)
	}
}
