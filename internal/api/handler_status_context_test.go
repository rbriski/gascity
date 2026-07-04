package api

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// deadlineRecordingListStore is a beads.Store + beads.ContextLister fake
// that records whether each ListContext call carried a deadline. It also
// implements Counter, returning ErrCountUnsupported, to force the status
// work-count path down to its List/ListContext fallback.
type deadlineRecordingListStore struct {
	beads.Store
	listContextCalls int
	sawDeadline      bool
	listCalls        int // plain (non-ctx) List calls; must stay 0 once ListContext is preferred
}

func (s *deadlineRecordingListStore) Count(context.Context, beads.ListQuery, ...string) (int, error) {
	return 0, beads.ErrCountUnsupported
}

func (s *deadlineRecordingListStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	return s.Store.List(query)
}

func (s *deadlineRecordingListStore) ListContext(ctx context.Context, query beads.ListQuery) ([]beads.Bead, error) {
	s.listContextCalls++
	if _, ok := ctx.Deadline(); ok {
		s.sawDeadline = true
	}
	return s.Store.List(query)
}

// TestHandleStatusWorkCountsPreferListContextOverGoroutineList proves the
// status work-count fallback (Counter unsupported) prefers ListContext, with
// a real, request-scoped deadline bound to it — not the abandon-goroutine
// plain List path — when the store implements beads.ContextLister.
func TestHandleStatusWorkCountsPreferListContextOverGoroutineList(t *testing.T) {
	state := newFakeState(t)
	store := &deadlineRecordingListStore{Store: beads.NewMemStore()}
	state.stores["myrig"] = store

	getStatus(t, state)

	if store.listContextCalls == 0 {
		t.Fatal("ListContext was never called; want the ContextLister path preferred over plain List")
	}
	if !store.sawDeadline {
		t.Fatal("ListContext ctx had no deadline; want it bound to statusStoreReadTimeout")
	}
	if store.listCalls != 0 {
		t.Fatalf("plain List called %d times, want 0 (ListContext should be used exclusively)", store.listCalls)
	}
}

// TestStatusSessionSnapshotThreadsDeadlineIntoContextLister proves
// statusSessionSnapshot's internal ctx (bound to statusStoreReadTimeout)
// reaches the city store's ListContext, not context.Background(), so a
// timeout actually cancels the backing query instead of merely abandoning
// the goroutine.
func TestStatusSessionSnapshotThreadsDeadlineIntoContextLister(t *testing.T) {
	state := newFakeState(t)
	store := &deadlineRecordingListStore{Store: beads.NewMemStore()}
	state.cityBeadStore = store

	getStatus(t, state)

	if store.listContextCalls == 0 {
		t.Fatal("ListContext was never called from the session snapshot path; want the ContextLister path preferred")
	}
	if !store.sawDeadline {
		t.Fatal("session snapshot ListContext ctx had no deadline; want it bound to statusStoreReadTimeout")
	}
}
