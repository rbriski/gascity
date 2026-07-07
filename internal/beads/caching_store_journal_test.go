package beads

import (
	"context"
	"testing"
)

// TestCachingStoreForwardsControlFrontierHandle pins the P2.2 cap-forwarding
// requirement: ControlFrontierStoreFor must reach a JournalStore's frontier
// capability through a CachingStore wrapper, unmediated. The forward is total
// (caching_store_journal.go) because the frontier reads the journal projection
// tables, a domain disjoint from the bead-row cache.
func TestCachingStoreForwardsControlFrontierHandle(t *testing.T) {
	journal := newJournalTestStore(t)
	caching := NewCachingStoreForTest(journal, nil)

	frontier, ok := ControlFrontierStoreFor(caching)
	if !ok {
		t.Fatal("ControlFrontierStoreFor(cachingWrapped) = false, want the forwarded journal capability")
	}
	if frontier != ControlFrontierStore(journal) {
		t.Fatalf("forwarded frontier = %T, want the backing *JournalStore itself (unmediated)", frontier)
	}
	// The handle must actually execute a SELECT, not just type-assert.
	if _, err := frontier.ControlFrontier(context.Background(), ControlFrontierParams{}); err != nil {
		t.Fatalf("ControlFrontier via caching handle: %v", err)
	}
}

// TestCachingStoreControlFrontierAbsentWhenBackingLacksIt confirms the honest
// (nil, false) signal when the backing store has no frontier capability.
func TestCachingStoreControlFrontierAbsentWhenBackingLacksIt(t *testing.T) {
	caching := NewCachingStoreForTest(NewMemStore(), nil)
	if _, ok := ControlFrontierStoreFor(caching); ok {
		t.Fatal("ControlFrontierStoreFor over a non-journal backing = true, want false")
	}
}
