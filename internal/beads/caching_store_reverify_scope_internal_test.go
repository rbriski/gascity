package beads

import "testing"

// TestListQueryIDsFilter pins the ListQuery.IDs contract: an ids-only query is a
// valid filter (no AllowScan needed) and Matches keeps only the listed ids.
func TestListQueryIDsFilter(t *testing.T) {
	t.Parallel()

	q := ListQuery{IDs: []string{"gc-1", "gc-3"}}
	if !q.HasFilter() {
		t.Fatalf("an IDs-only query must report HasFilter() == true")
	}
	if !q.Matches(Bead{ID: "gc-1"}) {
		t.Fatalf("gc-1 should match IDs filter")
	}
	if !q.Matches(Bead{ID: "gc-3"}) {
		t.Fatalf("gc-3 should match IDs filter")
	}
	if q.Matches(Bead{ID: "gc-2"}) {
		t.Fatalf("gc-2 must not match an IDs filter that excludes it")
	}
}

// TestNativeIssueFilterFromListQueryPushesIDs verifies the native store maps
// ListQuery.IDs onto IssueFilter.IDs so the scan is pushed to `id IN (...)`.
func TestNativeIssueFilterFromListQueryPushesIDs(t *testing.T) {
	t.Parallel()

	f := nativeIssueFilterFromListQuery(ListQuery{
		IDs:       []string{"gc-1", "gc-2"},
		TierMode:  TierBoth,
		AllowScan: true,
	})
	if len(f.IDs) != 2 || f.IDs[0] != "gc-1" || f.IDs[1] != "gc-2" {
		t.Fatalf("IssueFilter.IDs = %v, want [gc-1 gc-2]", f.IDs)
	}
}

// reverifyListCaptureStore records every List query so a test can assert the
// re-verify scopes to the missing ids rather than scanning the active universe.
type reverifyListCaptureStore struct {
	Store
	listQueries []ListQuery
}

func (s *reverifyListCaptureStore) List(q ListQuery) ([]Bead, error) {
	s.listQueries = append(s.listQueries, q)
	return s.Store.List(q)
}

// TestFetchBeadsByIDsScopesToMissingIDs pins the recovery batch lookup: it
// issues ONE List scoped to exactly the missing ids (ListQuery.IDs),
// tier-consistent (TierBoth) and IncludeClosed, instead of re-scanning the whole
// active universe, and surfaces the requested bead among the missing set.
func TestFetchBeadsByIDsScopesToMissingIDs(t *testing.T) {
	t.Parallel()

	mem := NewMemStore()
	alive, err := mem.Create(Bead{Title: "alive wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("Create alive: %v", err)
	}
	// An unrelated active bead the batch lookup must NOT fetch.
	if _, err := mem.Create(Bead{Title: "unrelated"}); err != nil {
		t.Fatalf("Create unrelated: %v", err)
	}

	backing := &reverifyListCaptureStore{Store: mem}
	cache := NewCachingStoreForTest(backing, nil)

	want := map[string]Bead{alive.ID: {ID: alive.ID}}
	found, err := cache.fetchBeadsByIDs(want)
	if err != nil {
		t.Fatalf("fetchBeadsByIDs: %v", err)
	}

	if len(backing.listQueries) != 1 {
		t.Fatalf("batch lookup issued %d List calls, want 1", len(backing.listQueries))
	}
	q := backing.listQueries[0]
	if len(q.IDs) != 1 || q.IDs[0] != alive.ID {
		t.Fatalf("batch lookup List query IDs = %v, want scoped to [%s]", q.IDs, alive.ID)
	}
	if q.TierMode != TierBoth {
		t.Fatalf("batch lookup List query TierMode = %v, want TierBoth (tier-consistent)", q.TierMode)
	}
	if !q.IncludeClosed {
		t.Fatalf("batch lookup List query IncludeClosed = false, want true (distinguish closed from gone)")
	}
	if _, ok := found[alive.ID]; !ok {
		t.Fatalf("batch lookup did not surface the alive wisp %s: %v", alive.ID, found)
	}
}
