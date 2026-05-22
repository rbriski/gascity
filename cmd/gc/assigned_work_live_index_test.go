package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestBuildAssignedWorkLiveIndexUsesMinimalLiveQueries(t *testing.T) {
	store := &recordingAssignedWorkStore{
		rows: []beads.Bead{
			{ID: "work-1", Status: "open", Assignee: "worker", Type: "task"},
			{ID: "session-1", Status: "open", Assignee: "worker", Type: "session"},
		},
	}

	idx := buildAssignedWorkLiveIndex(store, nil)

	if len(store.queries) != 2 {
		t.Fatalf("List queries = %d, want 2", len(store.queries))
	}
	for _, query := range store.queries {
		if !query.Live {
			t.Fatalf("List query Live = false, want true")
		}
		if query.TierMode != beads.TierBoth {
			t.Fatalf("List query TierMode = %v, want TierBoth", query.TierMode)
		}
		if !query.SkipLabels {
			t.Fatalf("List query SkipLabels = false, want true to use bd --json-minimal")
		}
	}

	has, err := idx.hasOpenAssignedWork(beads.Bead{
		ID: "session-target",
		Metadata: map[string]string{
			"session_name": "worker",
		},
	})
	if err != nil {
		t.Fatalf("hasOpenAssignedWork: %v", err)
	}
	if !has {
		t.Fatal("hasOpenAssignedWork = false, want true")
	}
}

func TestSessionHasOpenAssignedWorkInStoreUsesBoundedMinimalLiveProbe(t *testing.T) {
	store := &recordingAssignedWorkStore{
		rows: []beads.Bead{
			{ID: "work-1", Status: "open", Assignee: "session-1", Type: "task"},
		},
	}

	has, err := sessionHasOpenAssignedWorkInStore(store, beads.Bead{ID: "session-1"})
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkInStore: %v", err)
	}
	if !has {
		t.Fatal("sessionHasOpenAssignedWorkInStore = false, want true")
	}
	if len(store.queries) != 1 {
		t.Fatalf("List queries = %d, want 1", len(store.queries))
	}
	query := store.queries[0]
	if !query.Live {
		t.Fatalf("List query Live = false, want true")
	}
	if query.TierMode != beads.TierBoth {
		t.Fatalf("List query TierMode = %v, want TierBoth", query.TierMode)
	}
	if !query.SkipLabels {
		t.Fatalf("List query SkipLabels = false, want true to use bd --json-minimal")
	}
	if query.Limit != 1 {
		t.Fatalf("List query Limit = %d, want 1", query.Limit)
	}
	if query.Assignee != "session-1" || query.Status != "open" {
		t.Fatalf("List query = %+v, want assignee=session-1 status=open", query)
	}
}

func TestSessionHasOpenAssignedWorkInStoreFallsBackWhenBoundedProbeOnlyFindsSessionRows(t *testing.T) {
	store := &recordingAssignedWorkStore{
		rows: []beads.Bead{
			{ID: "session-row", Status: "open", Assignee: "session-1", Type: "session"},
			{ID: "work-1", Status: "open", Assignee: "session-1", Type: "task"},
		},
	}

	has, err := sessionHasOpenAssignedWorkInStore(store, beads.Bead{ID: "session-1"})
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkInStore: %v", err)
	}
	if !has {
		t.Fatal("sessionHasOpenAssignedWorkInStore = false, want true from fallback query")
	}
	if len(store.queries) != 2 {
		t.Fatalf("List queries = %d, want bounded probe then fallback", len(store.queries))
	}
	if got := store.queries[0].Limit; got != 1 {
		t.Fatalf("bounded probe Limit = %d, want 1", got)
	}
	if got := store.queries[1].Limit; got != 0 {
		t.Fatalf("fallback query Limit = %d, want 0", got)
	}
	if !store.queries[1].SkipLabels {
		t.Fatalf("fallback query SkipLabels = false, want true")
	}
}

type recordingAssignedWorkStore struct {
	beads.Store
	rows    []beads.Bead
	queries []beads.ListQuery
}

func (s *recordingAssignedWorkStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.queries = append(s.queries, query)
	return beads.ApplyListQuery(s.rows, query), nil
}
