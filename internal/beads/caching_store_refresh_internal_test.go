package beads

import (
	"context"
	"strconv"
	"testing"
)

func TestCachingStoreBoundedParentMetadataRefreshStatsWorkIsResultBounded(t *testing.T) {
	mem := NewMemStore()
	parent, err := mem.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create(parent): %v", err)
	}
	const siblingCount = 128
	for i := 0; i < siblingCount; i++ {
		if _, err := mem.Create(Bead{
			Title:    "sibling-" + strconv.Itoa(i+1),
			ParentID: parent.ID,
			Metadata: map[string]string{"idempotency_key": "other:" + strconv.Itoa(i+1)},
		}); err != nil {
			t.Fatalf("Create(sibling %d): %v", i+1, err)
		}
	}

	cache := NewCachingStoreForTest(mem, nil)
	if err := cache.Prime(context.Background()); err != nil {
		t.Fatalf("Prime: %v", err)
	}
	statsBefore := cache.Stats()

	const targetKey = "converge:root-1:iter:1"
	target, err := mem.Create(Bead{
		Title:    "target",
		ParentID: parent.ID,
		Metadata: map[string]string{"idempotency_key": targetKey},
		Needs:    []string{"dep-1"},
	})
	if err != nil {
		t.Fatalf("Create(target): %v", err)
	}

	var statsWork []int
	cache.mu.Lock()
	cache.statsWorkForTest = func(rows int) {
		statsWork = append(statsWork, rows)
	}
	cache.mu.Unlock()

	got, err := cache.List(ListQuery{
		ParentID:      parent.ID,
		Metadata:      map[string]string{"idempotency_key": targetKey},
		IncludeClosed: true,
		TierMode:      TierBoth,
		Limit:         2,
	})
	if err != nil {
		t.Fatalf("List(bounded parent metadata): %v", err)
	}
	if len(got) != 1 || got[0].ID != target.ID {
		t.Fatalf("List(bounded parent metadata) = %#v, want target %q", got, target.ID)
	}
	if len(statsWork) != 1 || statsWork[0] != 2 {
		t.Fatalf("stats lock work = %v, want two result-ID visits independent of %d cached siblings", statsWork, siblingCount)
	}
	statsAfter := cache.Stats()
	if statsAfter.TotalBeads != statsBefore.TotalBeads+1 {
		t.Fatalf("TotalBeads after bounded refresh = %d, want %d", statsAfter.TotalBeads, statsBefore.TotalBeads+1)
	}
	if statsAfter.TotalDeps != statsBefore.TotalDeps+1 {
		t.Fatalf("TotalDeps after bounded refresh = %d, want %d", statsAfter.TotalDeps, statsBefore.TotalDeps+1)
	}
}
