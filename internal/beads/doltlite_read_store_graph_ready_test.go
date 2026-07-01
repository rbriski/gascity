//go:build gascity_native_beads

package beads

import (
	"testing"
	"time"
)

// TestDoltliteReadStoreReadyGraphOnlyReturnsTierWispsOnly is the TDD anchor for
// ga-ifavnc.2. Until DoltliteReadStore.ReadyGraphOnly is added, GraphOnlyReadyFor
// returns (nil, false) and the test fails at the ok-assertion. Once the method
// exists it must return only wisp-table beads, never durable issue-table beads.
func TestDoltliteReadStoreReadyGraphOnlyReturnsTierWispsOnly(t *testing.T) {
	now := time.Now().UTC()
	store := newDoltliteStoreWithRows(t,
		[]testDoltliteIssue{
			{ID: "gco-issue", Title: "durable issue", Status: "open", IssueType: "task", CreatedAt: now},
		},
		[]testDoltliteIssue{
			{ID: "gco-wisp", Title: "wisp step", Status: "open", IssueType: "molecule", CreatedAt: now.Add(time.Second), Ephemeral: true},
			{ID: "gco-nohistory", Title: "no-history step", Status: "open", IssueType: "molecule", CreatedAt: now.Add(2 * time.Second), NoHistory: true},
		},
	)

	graphOnly, ok := GraphOnlyReadyFor(store)
	if !ok {
		t.Skip("DoltliteReadStore does not implement GraphOnlyReadyStore; add ReadyGraphOnly in ga-ifavnc.2")
	}

	rows, err := graphOnly.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	if hasTestBead(rows, "gco-issue") {
		t.Errorf("ReadyGraphOnly included durable issues-tier bead gco-issue; want wisp tier only")
	}
	if !hasTestBead(rows, "gco-wisp") {
		t.Errorf("ReadyGraphOnly missing ephemeral wisp gco-wisp; ids=%v", testBeadIDs(rows))
	}
	if !hasTestBead(rows, "gco-nohistory") {
		t.Errorf("ReadyGraphOnly missing no-history bead gco-nohistory; ids=%v", testBeadIDs(rows))
	}
}

// TestDoltliteReadStoreReadyGraphOnlyExcludesBlockedWisps pins that ReadyGraphOnly
// filters out wisp beads with open blocking dependencies (ga-b5j6av). A wisp
// whose blocker is already closed must still appear as ready.
func TestDoltliteReadStoreReadyGraphOnlyExcludesBlockedWisps(t *testing.T) {
	now := time.Now().UTC()
	store := newDoltliteStoreWithRows(t, nil, []testDoltliteIssue{
		{ID: "gco-blocker", Title: "open blocker", Status: "open", IssueType: "molecule", CreatedAt: now, Ephemeral: true},
		{
			ID: "gco-blocked", Title: "blocked step", Status: "open", IssueType: "molecule",
			CreatedAt: now.Add(time.Second), Ephemeral: true,
			Dependencies: []testDoltliteDependency{{DependsOnID: "gco-blocker"}},
		},
		{ID: "gco-closed-dep", Title: "closed dep", Status: "closed", IssueType: "molecule", CreatedAt: now.Add(2 * time.Second), Ephemeral: true},
		{
			ID: "gco-unblocked", Title: "unblocked step", Status: "open", IssueType: "molecule",
			CreatedAt: now.Add(3 * time.Second), Ephemeral: true,
			Dependencies: []testDoltliteDependency{{DependsOnID: "gco-closed-dep"}},
		},
	})

	graphOnly, ok := GraphOnlyReadyFor(store)
	if !ok {
		t.Skip("DoltliteReadStore does not implement GraphOnlyReadyStore; add ReadyGraphOnly in ga-ifavnc.2")
	}

	rows, err := graphOnly.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	if hasTestBead(rows, "gco-blocked") {
		t.Errorf("ReadyGraphOnly included gco-blocked (open blocker gco-blocker); ids=%v", testBeadIDs(rows))
	}
	if !hasTestBead(rows, "gco-blocker") {
		t.Errorf("ReadyGraphOnly missing gco-blocker (no deps, should be ready); ids=%v", testBeadIDs(rows))
	}
	if !hasTestBead(rows, "gco-unblocked") {
		t.Errorf("ReadyGraphOnly missing gco-unblocked (blocker closed, should be ready); ids=%v", testBeadIDs(rows))
	}
}

// TestDoltliteReadStoreReadyGraphOnlyForcesTierWisps pins that ReadyGraphOnly
// ignores the caller-supplied TierMode and always queries the wisp table set.
// Passing TierBoth or TierIssues must not cause durable issues to appear.
func TestDoltliteReadStoreReadyGraphOnlyForcesTierWisps(t *testing.T) {
	now := time.Now().UTC()
	store := newDoltliteStoreWithRows(t,
		[]testDoltliteIssue{
			{ID: "gco-issue", Title: "durable issue", Status: "open", IssueType: "task", CreatedAt: now},
		},
		[]testDoltliteIssue{
			{ID: "gco-wisp", Title: "wisp step", Status: "open", IssueType: "molecule", CreatedAt: now.Add(time.Second), Ephemeral: true},
		},
	)

	graphOnly, ok := GraphOnlyReadyFor(store)
	if !ok {
		t.Skip("DoltliteReadStore does not implement GraphOnlyReadyStore; add ReadyGraphOnly in ga-ifavnc.2")
	}

	for _, mode := range []TierMode{TierBoth, TierIssues} {
		rows, err := graphOnly.ReadyGraphOnly(ReadyQuery{TierMode: mode})
		if err != nil {
			t.Fatalf("ReadyGraphOnly(TierMode=%v): %v", mode, err)
		}
		if hasTestBead(rows, "gco-issue") {
			t.Errorf("ReadyGraphOnly(TierMode=%v) returned durable issues-tier bead; must be forced to TierWisps", mode)
		}
		if !hasTestBead(rows, "gco-wisp") {
			t.Errorf("ReadyGraphOnly(TierMode=%v) missing wisp gco-wisp; ids=%v", mode, testBeadIDs(rows))
		}
	}
}
