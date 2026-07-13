package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// droppingListStore wraps a Store and silently omits selected bead IDs from
// List results, simulating a cleanly parsed but incomplete List under backend
// stress. It distinguishes the reconcile fresh scan (unscoped) from the recovery
// batch lookup (scoped by ListQuery.IDs): dropFromList omits ids only from the
// unscoped fresh scan (making them recovery candidates), dropFromScoped also
// omits them from the scoped batch lookup (modeling a genuinely-gone bead), and
// scopedListErr fails the batch lookup outright (modeling a transient backing
// error that must defer rather than evict).
type droppingListStore struct {
	Store
	dropFromList   map[string]struct{}
	dropFromScoped map[string]struct{}
	scopedListErr  error
}

func (s *droppingListStore) List(query ListQuery) ([]Bead, error) {
	scoped := len(query.IDs) > 0
	if scoped && s.scopedListErr != nil {
		return nil, s.scopedListErr
	}
	all, err := s.Store.List(query)
	if err != nil {
		return all, err
	}
	drop := s.dropFromList
	if scoped {
		drop = s.dropFromScoped
	}
	if len(drop) == 0 {
		return all, nil
	}
	filtered := make([]Bead, 0, len(all))
	for _, b := range all {
		if _, d := drop[b.ID]; d {
			continue
		}
		filtered = append(filtered, b)
	}
	return filtered, nil
}

func assertNotCached(t *testing.T, cache *CachingStore, id string) {
	t.Helper()
	cache.mu.RLock()
	_, ok := cache.beads[id]
	cache.mu.RUnlock()
	if ok {
		t.Fatalf("cache still has bead %q after confirmed close", id)
	}
}

// TestReconcileSkipsCloseWhenListDropsAliveBead reproduces the cache-thrash
// scenario where a cleanly incomplete List omits an alive bead. Before the
// fix, the reconciler would synthesize bead.closed every cycle and
// re-introduction via other paths would re-trigger it.
func TestReconcileSkipsCloseWhenListDropsAliveBead(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	survivor, err := mem.Create(Bead{Title: "Survivor"})
	if err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	dropped, err := mem.Create(Bead{Title: "Dropped by tolerant parser"})
	if err != nil {
		t.Fatalf("Create dropped: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{dropped.ID: {}}
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+dropped.ID {
			t.Fatalf("emitted bead.closed for an alive bead dropped by List; events = %v", events)
		}
	}

	got, err := cache.Get(dropped.ID)
	if err != nil {
		t.Fatalf("Get(dropped) after reconcile: %v", err)
	}
	if got.Status == "closed" {
		t.Fatalf("Get(dropped) returned status=closed; cache should still see it as alive")
	}
	if _, err := cache.Get(survivor.ID); err != nil {
		t.Fatalf("Get(survivor) after reconcile: %v", err)
	}
	stats := cache.Stats()
	if stats.ReconcileRecoveries != 1 {
		t.Fatalf("ReconcileRecoveries = %d, want 1", stats.ReconcileRecoveries)
	}
	if stats.ReconcileCloseDeferrals != 0 {
		t.Fatalf("ReconcileCloseDeferrals = %d, want 0", stats.ReconcileCloseDeferrals)
	}
}

// TestReconcileEmitsCloseWhenBackingConfirmsNotFound verifies that a genuine
// closure (the bead is absent from BOTH the fresh scan and the scoped recovery
// batch lookup) still produces a bead.closed event.
func TestReconcileEmitsCloseWhenBackingConfirmsNotFound(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	gone, err := mem.Create(Bead{Title: "Truly gone"})
	if err != nil {
		t.Fatalf("Create gone: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Genuinely gone from the backing after prime: absent from the fresh scan
	// (candidate) and from the scoped batch lookup (confirmed gone).
	if err := mem.Delete(gone.ID); err != nil {
		t.Fatalf("Delete gone: %v", err)
	}
	events = events[:0]

	cache.runReconciliation()

	want := "bead.closed:" + gone.ID
	found := false
	for _, e := range events {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("events = %v, want %s when backing confirmed not-found", events, want)
	}
	if _, err := cache.Get(gone.ID); err == nil {
		t.Fatalf("Get(gone) succeeded after confirmed close; cache should evict it")
	}
	assertNotCached(t, cache, gone.ID)
}

// TestReconcileEmitsCloseWhenGetReturnsClosed verifies that a real open-to-
// closed transition still emits bead.closed when the closed bead is absent
// from normal List results.
func TestReconcileEmitsCloseWhenGetReturnsClosed(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	closing, err := mem.Create(Bead{Title: "Closing"})
	if err != nil {
		t.Fatalf("Create closing: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if err := mem.Close(closing.ID); err != nil {
		t.Fatalf("Close backing bead: %v", err)
	}
	events = events[:0]

	cache.runReconciliation()

	want := "bead.closed:" + closing.ID
	found := false
	for _, e := range events {
		if e == want {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("events = %v, want %s when backing returned closed bead", events, want)
	}
	assertNotCached(t, cache, closing.ID)
}

// TestReconcileEmitsFreshClosePayloadWhenGetReturnsClosed pins the close
// recovery path that verifies a missing active-list row with backing.Get.
// The close notification must carry the fresh closed row, not a synthetic
// status flip built from stale cache contents.
func TestReconcileEmitsFreshClosePayloadWhenGetReturnsClosed(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	closing, err := mem.Create(Bead{Title: "Closing"})
	if err != nil {
		t.Fatalf("Create closing: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var closedPayload Bead
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, payload json.RawMessage) {
		if eventType != "bead.closed" || beadID != closing.ID {
			return
		}
		if err := json.Unmarshal(payload, &closedPayload); err != nil {
			t.Fatalf("unmarshal close payload: %v", err)
		}
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	status := "closed"
	if err := mem.Update(closing.ID, UpdateOpts{
		Status: &status,
		Metadata: map[string]string{
			"ci.verdict": "done",
			"gc.outcome": "pass",
		},
	}); err != nil {
		t.Fatalf("Close backing bead with metadata: %v", err)
	}

	cache.runReconciliation()

	if closedPayload.ID != closing.ID {
		t.Fatalf("closed payload ID = %q, want %q", closedPayload.ID, closing.ID)
	}
	if closedPayload.Metadata["ci.verdict"] != "done" || closedPayload.Metadata["gc.outcome"] != "pass" {
		t.Fatalf("closed payload metadata = %#v, want fresh backing close metadata", closedPayload.Metadata)
	}
	assertNotCached(t, cache, closing.ID)
}

// TestReconcileDefersCloseOnBackingError verifies that a transient backing
// failure (the fresh scan omits the bead, and the scoped recovery batch lookup
// errors) does NOT produce a bead.closed event — the close is deferred until a
// later, unambiguous scan.
func TestReconcileDefersCloseOnBackingError(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	uncertain, err := mem.Create(Bead{Title: "Uncertain"})
	if err != nil {
		t.Fatalf("Create uncertain: %v", err)
	}

	backing := &droppingListStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	backing.dropFromList = map[string]struct{}{uncertain.ID: {}}
	backing.scopedListErr = errors.New("dolt: connection reset")
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+uncertain.ID {
			t.Fatalf("emitted bead.closed despite backing.Get error; events = %v", events)
		}
	}
	if _, err := cache.Get(uncertain.ID); err != nil {
		t.Fatalf("Get(uncertain) after reconcile: %v", err)
	}
	stats := cache.Stats()
	if stats.ReconcileRecoveries != 0 {
		t.Fatalf("ReconcileRecoveries = %d, want 0", stats.ReconcileRecoveries)
	}
	if stats.ReconcileCloseDeferrals != 1 {
		t.Fatalf("ReconcileCloseDeferrals = %d, want 1", stats.ReconcileCloseDeferrals)
	}
}

// (The former TestReconcileDefersCloseWhenGetReturnsWrongID is obsolete: the
// recovery batch lookup uses List(ListQuery.IDs), whose exact id predicate can
// never return a row under a different id, so the wrong-id-from-fuzzy-Get case
// it guarded no longer exists.)
