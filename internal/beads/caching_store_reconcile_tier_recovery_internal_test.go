package beads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
)

// tierBlindReverifyStore models the split-store failure mode behind the wisp
// membership flip-flop: an ephemeral wisp is transiently omitted from one
// composite List scan (a managed-Dolt rig-store flake) AND the per-id Get path
// is tier-blind — it returns ErrNotFound for a live gcg-wisp-*. dropNextList
// omits the given ids from the NEXT List result only (a transient omission),
// then clears itself so a re-verify List sees the full set again. getNotFound
// ids always return ErrNotFound from Get; getErr ids return the given error.
type tierBlindReverifyStore struct {
	Store
	dropNextList map[string]struct{}
	getNotFound  map[string]struct{}
	getErr       map[string]error
	// listErrOnce, when set, fails the NEXT List call once then clears itself.
	listErrOnce error
}

func (s *tierBlindReverifyStore) List(query ListQuery) ([]Bead, error) {
	all, err := s.Store.List(query)
	if err != nil {
		return all, err
	}
	// The fresh full scan (dropNextList armed) transiently omits the ids and
	// disarms, so the later re-verify scan sees the full set again.
	if len(s.dropNextList) > 0 {
		drop := s.dropNextList
		s.dropNextList = nil
		filtered := make([]Bead, 0, len(all))
		for _, b := range all {
			if _, ok := drop[b.ID]; ok {
				continue
			}
			filtered = append(filtered, b)
		}
		return filtered, nil
	}
	// A later scan (the re-verify) may fail once with a transient error.
	if s.listErrOnce != nil {
		e := s.listErrOnce
		s.listErrOnce = nil
		return nil, e
	}
	return all, nil
}

func (s *tierBlindReverifyStore) Get(id string) (Bead, error) {
	if err, ok := s.getErr[id]; ok {
		return Bead{}, err
	}
	if _, ok := s.getNotFound[id]; ok {
		return Bead{}, fmt.Errorf("getting bead %q: %w", id, ErrNotFound)
	}
	return s.Store.Get(id)
}

// TestReconcileDoesNotEvictWispWhenGetTierBlindButListRecovers pins facet 3:
// a live ephemeral wisp transiently dropped from the fresh full scan must NOT
// be evicted just because the tier-blind per-id Get reports ErrNotFound. A
// tier-consistent (TierBoth) re-verify scan sees the wisp, so it is recovered
// alive instead of flipped to bead.closed.
func TestReconcileDoesNotEvictWispWhenGetTierBlindButListRecovers(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	survivor, err := mem.Create(Bead{Title: "Survivor"})
	if err != nil {
		t.Fatalf("Create survivor: %v", err)
	}
	wisp, err := mem.Create(Bead{Title: "Live wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}

	backing := &tierBlindReverifyStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// The fresh full scan transiently omits the wisp (one flake); the tier-blind
	// Get reports it gone. Before the fix, recoverMissingFromList trusted that
	// Get and synthesized a false bead.closed.
	backing.dropNextList = map[string]struct{}{wisp.ID: {}}
	backing.getNotFound = map[string]struct{}{wisp.ID: {}}
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+wisp.ID {
			t.Fatalf("emitted bead.closed for a live ephemeral wisp the tier-consistent re-verify still sees; events = %v", events)
		}
	}

	cache.mu.RLock()
	_, stillCached := cache.beads[wisp.ID]
	cache.mu.RUnlock()
	if !stillCached {
		t.Fatalf("cache evicted the live wisp %q", wisp.ID)
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

// TestReconcileDefersWispCloseWhenReverifyListErrors verifies that a transient
// managed-Dolt read error on the re-verify scan defers the eviction instead of
// evicting: a bead Get reports gone but the re-verify List cannot confirm it is
// gone must stay alive for a later, unambiguous scan.
func TestReconcileDefersWispCloseWhenReverifyListErrors(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	wisp, err := mem.Create(Bead{Title: "Uncertain wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create wisp: %v", err)
	}

	backing := &tierBlindReverifyStore{Store: mem}
	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}

	// Fresh scan drops the wisp; tier-blind Get says gone; the re-verify scan
	// then fails with a transient error, so the close must be deferred.
	backing.dropNextList = map[string]struct{}{wisp.ID: {}}
	backing.getNotFound = map[string]struct{}{wisp.ID: {}}
	backing.listErrOnce = errors.New("dolt: invalid connection")
	events = events[:0]

	cache.runReconciliation()

	for _, e := range events {
		if e == "bead.closed:"+wisp.ID {
			t.Fatalf("emitted bead.closed despite a transient re-verify List error; events = %v", events)
		}
	}
	cache.mu.RLock()
	_, stillCached := cache.beads[wisp.ID]
	cache.mu.RUnlock()
	if !stillCached {
		t.Fatalf("cache evicted the wisp %q on a transient re-verify error", wisp.ID)
	}
	stats := cache.Stats()
	if stats.ReconcileCloseDeferrals != 1 {
		t.Fatalf("ReconcileCloseDeferrals = %d, want 1", stats.ReconcileCloseDeferrals)
	}
	if stats.ReconcileRecoveries != 0 {
		t.Fatalf("ReconcileRecoveries = %d, want 0", stats.ReconcileRecoveries)
	}
}
