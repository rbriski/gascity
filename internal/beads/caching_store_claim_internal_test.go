package beads

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
)

// nonClaimerStore embeds the Store interface so it satisfies Store but exposes
// none of the embedded value's optional capabilities (Claim is not a Store
// method, so it is not promoted). Used to drive the CachingStore's
// ErrClaimUnsupported fallback now that every concrete classed store is a Claimer.
type nonClaimerStore struct {
	Store
}

// claimerBackingStore is a Store that also implements the 2-arg Claimer,
// delegating the claim to the embedded store's Update so the CachingStore's
// post-claim refresh observes the assignee + in_progress status. It models the
// production backing (the bead-policy store wrapping the Router, which routes a
// graph bead to the SQLite Claimer).
type claimerBackingStore struct {
	Store
	claimCalls   int
	lastAssignee string
}

func (c *claimerBackingStore) Claim(id, assignee string) (Bead, bool, error) {
	c.claimCalls++
	c.lastAssignee = assignee
	b, err := c.Get(id)
	if err != nil {
		return Bead{}, false, err
	}
	if b.Assignee != "" && b.Assignee != assignee {
		return Bead{}, false, nil // already claimed by another actor
	}
	status := "in_progress"
	if err := c.Update(id, UpdateOpts{Assignee: &assignee, Status: &status}); err != nil {
		return Bead{}, false, err
	}
	claimed, err := c.Get(id)
	if err != nil {
		return Bead{}, false, err
	}
	return claimed, true, nil
}

// TestCachingStoreClaimDelegatesToClaimerBackingAndRefreshesCache asserts the
// outer CachingStore forwards Claim to a Claimer backing and refreshes its cache
// to the claimed state — the fix that lets `gc hook --claim` (→ POST
// /bead/{id}/claim → store.(Claimer)) reach the SQLite graph store through the
// city's CachingStore.
func TestCachingStoreClaimDelegatesToClaimerBackingAndRefreshesCache(t *testing.T) {
	t.Parallel()

	backing := &claimerBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "graph step"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	var events []string
	cache := NewCachingStoreForTest(backing, func(eventType, beadID string, _ json.RawMessage) {
		events = append(events, eventType+":"+beadID)
	})
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	events = nil

	claimed, ok, err := cache.Claim(bead.ID, "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if !ok {
		t.Fatal("Claim ok = false, want true")
	}
	if backing.claimCalls != 1 {
		t.Fatalf("backing Claim calls = %d, want 1", backing.claimCalls)
	}
	if backing.lastAssignee != "worker-1" {
		t.Fatalf("backing claim assignee = %q, want worker-1", backing.lastAssignee)
	}
	if claimed.Assignee != "worker-1" || claimed.Status != "in_progress" {
		t.Fatalf("claimed bead = %+v, want assignee worker-1 and in_progress", claimed)
	}
	got, err := cache.Get(bead.ID)
	if err != nil {
		t.Fatalf("cache Get: %v", err)
	}
	if got.Assignee != "worker-1" || got.Status != "in_progress" {
		t.Fatalf("cached bead = %+v, want assignee worker-1 and in_progress", got)
	}
	if !stringSliceContains(events, "bead.updated:"+bead.ID) {
		t.Fatalf("events = %v, want bead.updated for claimed bead", events)
	}
}

// TestCachingStoreClaimUnsupportedWhenBackingNotClaimer asserts the CachingStore
// surfaces ErrClaimUnsupported when its backing can neither Claim(id, assignee)
// nor Claim(id), rather than silently dropping the claim.
func TestCachingStoreClaimUnsupportedWhenBackingNotClaimer(t *testing.T) {
	t.Parallel()

	// nonClaimerStore embeds the Store interface, so it satisfies Store but
	// promotes none of MemStore's concrete optional capabilities — in particular
	// NOT Claimer (Claim is not part of the Store interface). MemStore itself is a
	// Claimer now (every classed backend must be), so we need an explicit
	// non-claimer double to exercise the CachingStore's ErrClaimUnsupported path.
	backing := nonClaimerStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "step"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	if _, ok, err := cache.Claim(bead.ID, "worker-1"); !errors.Is(err, ErrClaimUnsupported) || ok {
		t.Fatalf("Claim = (ok=%v, err=%v), want (false, ErrClaimUnsupported)", ok, err)
	}
}

// TestCachingStoreClaimRejectedLeavesBeadUnclaimed asserts a lost-race claim
// (backing returns ok=false) reports the miss without error and without
// recording a stolen assignment in the cache.
func TestCachingStoreClaimRejectedLeavesBeadUnclaimed(t *testing.T) {
	t.Parallel()

	backing := &claimerBackingStore{Store: NewMemStore()}
	bead, err := backing.Create(Bead{Title: "step", Assignee: "other-worker"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	cache := NewCachingStoreForTest(backing, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	claimed, ok, err := cache.Claim(bead.ID, "worker-1")
	if err != nil {
		t.Fatalf("Claim: %v", err)
	}
	if ok {
		t.Fatalf("Claim ok = true, want false (already claimed by other-worker); claimed=%+v", claimed)
	}
}
