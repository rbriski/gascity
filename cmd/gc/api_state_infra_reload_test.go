package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// closableMemStore is a MemStore that records whether CloseStore was called, so a
// reload test can assert a retained store is NOT scheduled for close (the
// use-after-close guard).
type closableMemStore struct {
	*beads.MemStore
	closed atomic.Int32
}

func newClosableMemStore() *closableMemStore {
	return &closableMemStore{MemStore: beads.NewMemStore()}
}

// CloseStore records the close and returns nil. The error return is required to
// satisfy the interface{ CloseStore() error } that closeBeadStoreHandle
// type-asserts, so it cannot be dropped.
func (c *closableMemStore) CloseStore() error { //nolint:unparam // interface contract requires the error return
	c.closed.Add(1)
	return nil
}

// newInfraReloadTestState builds a controllerState wired for the update() reload
// corner tests: a non-cancellable cacheCtx so wrapWithCachingStore skips the
// async prime + reconciler (no leaked goroutines), an empty city config (no rigs),
// and a seeded old infra store. controllerStateStoreCloseDelay is forced to 0 so
// any scheduled close runs synchronously and the test can observe it.
func newInfraReloadTestState(t *testing.T, oldInfra beads.Store) *controllerState {
	t.Helper()
	cs := &controllerState{
		cfg:            &config.City{},
		cacheCtx:       context.Background(), // Done()==nil ⇒ no reconciler goroutine
		cityName:       "test-city",
		cityPath:       t.TempDir(),
		cityBeadStore:  beads.NewMemStore(),
		cityInfraStore: oldInfra,
	}
	cs.cityMailProv = newCityMailProvider(cs.cityBeadStore, cs.cityInfraStore, cs.cfg, cs.cityPath, cs.eventProv)
	return cs
}

func infraStoreOpenResult(store beads.Store) beads.StoreOpenResult {
	return beads.StoreOpenResult{Store: store}
}

func cityStoreOpenResult(store beads.Store) beads.StoreOpenResult {
	return beads.StoreOpenResult{Store: store}
}

// TestUpdateInfraSucceedsCityFails covers defect 1: when the infra reopen
// succeeds but the city-store reopen FAILS, the retained cityMailProv still
// references the OLD infra store, so the old infra store must NOT be swapped out
// or scheduled for close (use-after-close). The freshly opened but unadopted infra
// handle is closed instead.
func TestUpdateInfraSucceedsCityFails(t *testing.T) {
	oldInfra := newClosableMemStore()
	cs := newInfraReloadTestState(t, oldInfra)

	newInfra := newClosableMemStore()

	restoreClose := swapStoreCloseDelay(0)
	defer restoreClose()
	restoreInfra := swapOpenCityInfraStore(func(string) (beads.StoreOpenResult, bool, error) {
		return infraStoreOpenResult(newInfra), true, nil // infra reopen SUCCEEDS
	})
	defer restoreInfra()
	restoreCity := swapOpenCityStore(func(string) (beads.StoreOpenResult, error) {
		return beads.StoreOpenResult{}, errors.New("city store reopen failed") // city FAILS
	})
	defer restoreCity()

	cs.update(cs.cfg, nil)

	// The old infra store is still referenced by the retained mail provider, so it
	// must remain live and un-swapped.
	if got := cs.CityInfraBeadStore(); !sameStorePtr(got, oldInfra) {
		t.Fatalf("cityInfraStore = %p, want retained old infra %p (city reopen failed)", got, oldInfra)
	}
	if n := oldInfra.closed.Load(); n != 0 {
		t.Fatalf("old infra store was closed %d times, want 0 (still referenced by retained provider)", n)
	}
	// The freshly opened but unadopted infra handle must be closed so it does not
	// leak.
	if n := newInfra.closed.Load(); n != 1 {
		t.Fatalf("unadopted new infra store closed %d times, want 1", n)
	}
}

// TestUpdateInfraFailsCitySucceeds covers defect 2: when the infra reopen FAILS
// while the city reopen succeeds, the provider is rebuilt with the EFFECTIVE infra
// store (the retained old handle) and cs.cityInfraStore keeps that same old handle
// — no provider/accessor split-brain, and the old infra store is not closed.
func TestUpdateInfraFailsCitySucceeds(t *testing.T) {
	oldInfra := newClosableMemStore()
	cs := newInfraReloadTestState(t, oldInfra)

	restoreClose := swapStoreCloseDelay(0)
	defer restoreClose()
	restoreInfra := swapOpenCityInfraStore(func(string) (beads.StoreOpenResult, bool, error) {
		return beads.StoreOpenResult{}, true, errors.New("transient infra open error") // infra FAILS
	})
	defer restoreInfra()
	restoreCity := swapOpenCityStore(func(string) (beads.StoreOpenResult, error) {
		return cityStoreOpenResult(beads.NewMemStore()), nil // city SUCCEEDS
	})
	defer restoreCity()

	cs.update(cs.cfg, nil)

	// The accessor must retain the old infra handle (a transient error must not
	// deactivate the split), matching the store the rebuilt provider was given.
	if got := cs.CityInfraBeadStore(); !sameStorePtr(got, oldInfra) {
		t.Fatalf("cityInfraStore = %p, want retained old infra %p (infra open error)", got, oldInfra)
	}
	if n := oldInfra.closed.Load(); n != 0 {
		t.Fatalf("old infra store closed %d times, want 0 (retained on transient error)", n)
	}
}

// TestUpdateInfraDeactivatedCitySucceeds covers the deactivation corner (scope
// removed, present=false): distinct from an open error, this is a valid
// deactivation, so cs.cityInfraStore swaps to nil and the old handle is closed.
func TestUpdateInfraDeactivatedCitySucceeds(t *testing.T) {
	oldInfra := newClosableMemStore()
	cs := newInfraReloadTestState(t, oldInfra)

	restoreClose := swapStoreCloseDelay(0)
	defer restoreClose()
	restoreInfra := swapOpenCityInfraStore(func(string) (beads.StoreOpenResult, bool, error) {
		return beads.StoreOpenResult{}, false, nil // present=false ⇒ deactivation
	})
	defer restoreInfra()
	restoreCity := swapOpenCityStore(func(string) (beads.StoreOpenResult, error) {
		return cityStoreOpenResult(beads.NewMemStore()), nil // city SUCCEEDS
	})
	defer restoreCity()

	cs.update(cs.cfg, nil)

	if got := cs.CityInfraBeadStore(); got != nil {
		t.Fatalf("cityInfraStore = %p, want nil after deactivation", got)
	}
	if n := oldInfra.closed.Load(); n != 1 {
		t.Fatalf("old infra store closed %d times, want 1 (deactivated, no longer referenced)", n)
	}
}

func swapStoreCloseDelay(d time.Duration) func() {
	prev := controllerStateStoreCloseDelay
	controllerStateStoreCloseDelay = d
	return func() { controllerStateStoreCloseDelay = prev }
}

func swapOpenCityInfraStore(fn func(string) (beads.StoreOpenResult, bool, error)) func() {
	prev := newControllerStateOpenCityInfraStore
	newControllerStateOpenCityInfraStore = fn
	return func() { newControllerStateOpenCityInfraStore = prev }
}

func swapOpenCityStore(fn func(string) (beads.StoreOpenResult, error)) func() {
	prev := newControllerStateOpenCityStore
	newControllerStateOpenCityStore = fn
	return func() { newControllerStateOpenCityStore = prev }
}
