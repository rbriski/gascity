package main

import (
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestCachedCityInfraStoreMemoization pins the three-way caching policy of
// cachedCityInfraStore: a real absence caches nil (single re-stat avoided), a
// successful open caches the handle, and an open ERROR is NOT cached so the next
// call retries rather than permanently poisoning the route to the work store.
// The last property is load-bearing for the running controller (circuit-reset
// socket → cliSessionStore → resolveSessionStore): a transient first-open failure
// must not strand session writes on the domain store for the process lifetime.
func TestCachedCityInfraStoreMemoization(t *testing.T) {
	t.Run("absence caches nil", func(t *testing.T) {
		cityPath := t.TempDir()
		var opens int32
		restore := swapCachedInfraStoreOpen(func(string) (beads.Store, bool, error) {
			atomic.AddInt32(&opens, 1)
			return nil, false, nil // authoritative absence: no infra scope
		})
		defer restore()
		defer clearInfraStoreCacheKey(cityPath)

		if got := cachedCityInfraStore(cityPath, nil); got != nil {
			t.Fatalf("first call: got %v, want nil (absence)", got)
		}
		if got := cachedCityInfraStore(cityPath, nil); got != nil {
			t.Fatalf("second call: got %v, want nil (absence)", got)
		}
		if n := atomic.LoadInt32(&opens); n != 1 {
			t.Fatalf("absence should be cached: opens = %d, want 1", n)
		}
	})

	t.Run("open error is not cached and retries", func(t *testing.T) {
		cityPath := t.TempDir()
		var opens int32
		var store beads.Store
		restore := swapCachedInfraStoreOpen(func(string) (beads.Store, bool, error) {
			n := atomic.AddInt32(&opens, 1)
			if n == 1 {
				return nil, true, errors.New("transient dolt hiccup")
			}
			// Second call: the transient failure cleared, real handle available.
			return store, true, nil
		})
		defer restore()
		defer clearInfraStoreCacheKey(cityPath)

		store = beads.NewMemStore()

		if got := cachedCityInfraStore(cityPath, nil); got != nil {
			t.Fatalf("first call on error: got %v, want nil (route to work store)", got)
		}
		// The error must NOT have been memoized: the retry reaches the open seam
		// again and now returns the recovered handle.
		got := cachedCityInfraStore(cityPath, nil)
		if !sameStorePtr(got, store) {
			t.Fatalf("retry after transient error: got %v, want recovered store %p", got, store)
		}
		if n := atomic.LoadInt32(&opens); n != 2 {
			t.Fatalf("error must retry: opens = %d, want 2", n)
		}
		// The recovered handle is now cached: a third call does not re-open.
		if got := cachedCityInfraStore(cityPath, nil); !sameStorePtr(got, store) {
			t.Fatalf("third call: got %v, want cached store %p", got, store)
		}
		if n := atomic.LoadInt32(&opens); n != 2 {
			t.Fatalf("success must be cached: opens = %d, want 2", n)
		}
	})

	t.Run("successful open is cached", func(t *testing.T) {
		cityPath := t.TempDir()
		var opens int32
		store := beads.NewMemStore()
		restore := swapCachedInfraStoreOpen(func(string) (beads.Store, bool, error) {
			atomic.AddInt32(&opens, 1)
			return store, true, nil
		})
		defer restore()
		defer clearInfraStoreCacheKey(cityPath)

		if got := cachedCityInfraStore(cityPath, nil); !sameStorePtr(got, store) {
			t.Fatalf("first call: got %v, want store %p", got, store)
		}
		if got := cachedCityInfraStore(cityPath, nil); !sameStorePtr(got, store) {
			t.Fatalf("second call: got %v, want cached store %p", got, store)
		}
		if n := atomic.LoadInt32(&opens); n != 1 {
			t.Fatalf("success should be cached: opens = %d, want 1", n)
		}
	})
}

func swapCachedInfraStoreOpen(fn func(string) (beads.Store, bool, error)) func() {
	prev := cachedInfraStoreOpen
	cachedInfraStoreOpen = fn
	return func() { cachedInfraStoreOpen = prev }
}

func clearInfraStoreCacheKey(cityPath string) {
	cityInfraStoreCache.Delete(filepath.Clean(cityPath))
}
