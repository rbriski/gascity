package beads

import (
	"errors"
	"testing"
)

// mockGraphOnlyReadyBacking is a MemStore that also implements GraphOnlyReadyStore,
// capturing the most recent query for assertion in tests.
type mockGraphOnlyReadyBacking struct {
	*MemStore
	ready     []Bead
	err       error
	lastQuery ReadyQuery
}

func (m *mockGraphOnlyReadyBacking) ReadyGraphOnly(query ...ReadyQuery) ([]Bead, error) {
	if len(query) > 0 {
		m.lastQuery = query[0]
	}
	return append([]Bead(nil), m.ready...), m.err
}

// TestCachingStoreReadyGraphOnlyHandleDelegatesAndPropagates pins the CachingStore
// capability-delegation contract: ReadyGraphOnlyHandle returns ok=true only when
// the backing implements GraphOnlyReadyStore, and the returned handle delegates
// calls and errors to the backing without caching or transforming the result.
func TestCachingStoreReadyGraphOnlyHandleDelegatesAndPropagates(t *testing.T) {
	t.Parallel()

	t.Run("ok when backing implements GraphOnlyReadyStore", func(t *testing.T) {
		t.Parallel()
		wisp := Bead{ID: "gc-cs-wisp", Status: "open"}
		backing := &mockGraphOnlyReadyBacking{MemStore: NewMemStore(), ready: []Bead{wisp}}
		cache := NewCachingStoreForTest(backing, nil)

		handle, ok := cache.ReadyGraphOnlyHandle()
		if !ok {
			t.Fatal("ReadyGraphOnlyHandle() ok = false, want true")
		}
		got, err := handle.ReadyGraphOnly()
		if err != nil {
			t.Fatalf("ReadyGraphOnly(): %v", err)
		}
		if len(got) != 1 || got[0].ID != "gc-cs-wisp" {
			t.Fatalf("ReadyGraphOnly() = %v, want [gc-cs-wisp]", got)
		}
	})

	t.Run("propagates backing errors unchanged", func(t *testing.T) {
		t.Parallel()
		sentinel := errors.New("store failure")
		backing := &mockGraphOnlyReadyBacking{MemStore: NewMemStore(), err: sentinel}
		cache := NewCachingStoreForTest(backing, nil)

		handle, ok := cache.ReadyGraphOnlyHandle()
		if !ok {
			t.Fatal("ReadyGraphOnlyHandle() ok = false, want true")
		}
		if _, err := handle.ReadyGraphOnly(); !errors.Is(err, sentinel) {
			t.Fatalf("ReadyGraphOnly() err = %v, want %v", err, sentinel)
		}
	})

	t.Run("returns nil false when backing has no graph-only capability", func(t *testing.T) {
		t.Parallel()
		cache := NewCachingStoreForTest(NewMemStore(), nil)
		handle, ok := cache.ReadyGraphOnlyHandle()
		if ok || handle != nil {
			t.Fatalf("ReadyGraphOnlyHandle() = (%v, %v), want (nil, false)", handle, ok)
		}
	})
}
