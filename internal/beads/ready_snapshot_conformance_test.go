package beads

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/fsys"
)

func TestFilterReadySnapshotMatchesRealStoreReadyQueries(t *testing.T) {
	tests := []struct {
		name string
		open func(*testing.T) Store
		seed bool
	}{
		{
			name: "mem",
			open: func(*testing.T) Store {
				return NewMemStore()
			},
			seed: true,
		},
		{
			name: "file",
			open: func(t *testing.T) Store {
				store, err := OpenFileStore(fsys.OSFS{}, filepath.Join(t.TempDir(), "beads.json"))
				if err != nil {
					t.Fatalf("OpenFileStore: %v", err)
				}
				return store
			},
			seed: true,
		},
		{
			name: "native_dolt_fixture",
			open: func(*testing.T) Store {
				return newNativeDoltStoreForTest(newNativeDoltMemStorage())
			},
			seed: true,
		},
		{
			name: "caching_store",
			open: func(*testing.T) Store {
				return NewCachingStoreForTest(NewMemStore(), nil)
			},
			seed: true,
		},
		{
			name: "bd",
			open: func(t *testing.T) Store {
				return NewBdStore(t.TempDir(), readySnapshotBDRunner(t))
			},
		},
		{
			name: "beadslib_wrapper",
			open: func(t *testing.T) Store {
				return &BeadsLibStore{
					BdStore: NewBdStore(t.TempDir(), readySnapshotBDRunner(t)),
				}
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := test.open(t)
			if test.seed {
				seedReadySnapshotConformanceStore(t, store)
			}
			assertReadySnapshotConformance(t, store, "worker-a", "worker-b")
		})
	}
}

func TestFilterReadySnapshotBoundsSparseLimitedAllocation(t *testing.T) {
	const rowCount = 10_000
	rows := make([]Bead, rowCount)
	for i := range rows {
		rows[i] = Bead{ID: fmt.Sprintf("work-%05d", i), Assignee: "other"}
	}
	rows[rowCount-1].Assignee = "needle"

	got := FilterReadySnapshot(rows, ReadyQuery{Assignee: "needle", Limit: 1})
	if len(got) != 1 || got[0].ID != "work-09999" {
		t.Fatalf("filtered rows = %#v, want only work-09999", got)
	}
	if capacity := cap(got); capacity > 1 {
		t.Fatalf("filtered capacity = %d, want <= query limit 1", capacity)
	}
}

func TestFilterReadySnapshotDeepCopiesEveryReferenceField(t *testing.T) {
	priority := 1
	blocked := false
	deferUntil := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	wantDeferUntil := deferUntil
	rows := []Bead{{
		ID:         "work-rich",
		Priority:   &priority,
		Assignee:   "worker-a",
		Needs:      []string{"blocks:dependency"},
		Labels:     []string{"original-label"},
		Metadata:   StringMap{"route": "original-route"},
		DeferUntil: &deferUntil,
		IsBlocked:  &blocked,
		Dependencies: []Dep{{
			IssueID:     "work-rich",
			DependsOnID: "dependency",
			Type:        "blocks",
		}},
	}}

	got := FilterReadySnapshot(rows, ReadyQuery{Assignee: "worker-a", Limit: 1})
	if len(got) != 1 {
		t.Fatalf("filtered rows = %d, want 1", len(got))
	}

	*rows[0].Priority = 9
	rows[0].Needs[0] = "mutated"
	rows[0].Labels[0] = "mutated"
	rows[0].Metadata["route"] = "mutated"
	*rows[0].DeferUntil = time.Time{}
	*rows[0].IsBlocked = true
	rows[0].Dependencies[0].DependsOnID = "mutated"

	row := got[0]
	if row.Priority == nil || *row.Priority != 1 {
		t.Fatalf("filtered Priority = %v, want independent value 1", row.Priority)
	}
	if !reflect.DeepEqual(row.Needs, []string{"blocks:dependency"}) ||
		!reflect.DeepEqual(row.Labels, []string{"original-label"}) ||
		row.Metadata["route"] != "original-route" ||
		row.DeferUntil == nil || !row.DeferUntil.Equal(wantDeferUntil) ||
		row.IsBlocked == nil || *row.IsBlocked ||
		len(row.Dependencies) != 1 || row.Dependencies[0].DependsOnID != "dependency" {
		t.Fatalf("filtered row retained an input alias: %#v", row)
	}
}

func assertReadySnapshotConformance(t *testing.T, store Store, assigneeA, assigneeB string) {
	t.Helper()
	snapshotQuery := ReadyQuery{TierMode: TierBoth}
	snapshot, err := store.Ready(snapshotQuery)
	if err != nil {
		t.Fatalf("Ready(unfiltered TierBoth): %v", err)
	}

	var countA, countB int
	var richRow bool
	for _, row := range snapshot {
		switch row.Assignee {
		case assigneeA:
			countA++
		case assigneeB:
			countB++
		}
		if row.Title == "a-rich" {
			// DoltLite's Ready fast path intentionally skips label hydration;
			// priority, description, and metadata still ensure the comparison is
			// over complete backend-returned rows rather than IDs alone.
			richRow = row.Priority != nil && row.Description != "" && len(row.Metadata) > 0
		}
	}
	if countA < 3 || countB < 1 {
		t.Fatalf("fixture Ready rows do not exercise sparse assignee filtering: count(%q)=%d count(%q)=%d rows=%#v", assigneeA, countA, assigneeB, countB, snapshot)
	}
	if !richRow {
		t.Fatalf("fixture Ready rows do not contain the full-row sentinel a-rich: %#v", snapshot)
	}

	queries := []struct {
		name  string
		query ReadyQuery
	}{
		{name: "unfiltered_unlimited", query: ReadyQuery{TierMode: TierBoth}},
		{name: "unfiltered_limit_one", query: ReadyQuery{Limit: 1, TierMode: TierBoth}},
		{name: "assignee_a_unlimited", query: ReadyQuery{Assignee: assigneeA, TierMode: TierBoth}},
		{name: "assignee_a_limit_one", query: ReadyQuery{Assignee: assigneeA, Limit: 1, TierMode: TierBoth}},
		{name: "assignee_a_limit_two", query: ReadyQuery{Assignee: assigneeA, Limit: 2, TierMode: TierBoth}},
		{name: "assignee_b_limit_one", query: ReadyQuery{Assignee: assigneeB, Limit: 1, TierMode: TierBoth}},
		{name: "missing_assignee", query: ReadyQuery{Assignee: "missing-worker", Limit: 2, TierMode: TierBoth}},
	}
	for _, test := range queries {
		t.Run(test.name, func(t *testing.T) {
			direct, directErr := store.Ready(test.query)
			if directErr != nil {
				t.Fatalf("direct Ready(%+v): %v", test.query, directErr)
			}
			filtered := FilterReadySnapshot(snapshot, test.query)
			if !readySnapshotRowsEqual(direct, filtered) {
				t.Fatalf("direct Ready and filtered unlimited snapshot differ\nquery: %+v\ndirect: %#v\nfiltered: %#v", test.query, direct, filtered)
			}
		})
	}
}

// readySnapshotRowsEqual compares the complete ordered rows while treating nil
// and an allocated empty slice as the same zero-row result. Store backends do
// not share a nil-slice convention, and the Ready contract does not distinguish
// the two; every populated Bead must still match exactly.
func readySnapshotRowsEqual(left, right []Bead) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if !reflect.DeepEqual(left[i], right[i]) {
			return false
		}
	}
	return true
}

func seedReadySnapshotConformanceStore(t *testing.T, store Store) {
	t.Helper()
	priority := 1
	past := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	rows := []Bead{
		{
			Title:       "a-rich",
			Type:        "task",
			Priority:    &priority,
			Assignee:    "worker-a",
			Description: "full-row sentinel",
			Labels:      []string{"snapshot", "rich"},
			Metadata:    StringMap{"route": "worker-a", "ordinal": "1"},
			DeferUntil:  &past,
		},
		{Title: "worker-b-middle", Type: "task", Assignee: "worker-b", Metadata: StringMap{"ordinal": "2"}},
		{Title: "a-second", Type: "task", Assignee: "worker-a", Metadata: StringMap{"ordinal": "3"}},
		{Title: "unassigned", Type: "task", Metadata: StringMap{"ordinal": "4"}},
		{Title: "a-third", Type: "task", Assignee: "worker-a", Metadata: StringMap{"ordinal": "5"}},
		{Title: "a-wisp", Type: "task", Assignee: "worker-a", Metadata: StringMap{"ordinal": "6"}, Ephemeral: true},
	}
	for _, row := range rows {
		if _, err := store.Create(row); err != nil {
			t.Fatalf("Create(%q): %v", row.Title, err)
		}
	}
}

func readySnapshotBDRunner(t *testing.T) CommandRunner {
	t.Helper()
	const rows = `[
		{"id":"bd-a-rich","title":"a-rich","status":"open","issue_type":"task","priority":1,"assignee":"worker-a","description":"full-row sentinel","labels":["snapshot","rich"],"metadata":{"route":"worker-a","ordinal":"1"},"created_at":"2025-01-15T10:30:00Z","defer_until":"2020-01-02T03:04:05Z"},
		{"id":"bd-worker-b","title":"worker-b-middle","status":"open","issue_type":"task","assignee":"worker-b","metadata":{"ordinal":"2"},"created_at":"2025-01-15T10:31:00Z"},
		{"id":"bd-a-second","title":"a-second","status":"open","issue_type":"task","assignee":"worker-a","metadata":{"ordinal":"3"},"created_at":"2025-01-15T10:32:00Z"},
		{"id":"bd-unassigned","title":"unassigned","status":"open","issue_type":"task","metadata":{"ordinal":"4"},"created_at":"2025-01-15T10:33:00Z"},
		{"id":"bd-a-third","title":"a-third","status":"open","issue_type":"task","assignee":"worker-a","metadata":{"ordinal":"5"},"created_at":"2025-01-15T10:34:00Z"},
		{"id":"bd-a-wisp","title":"a-wisp","status":"open","issue_type":"task","assignee":"worker-a","metadata":{"ordinal":"6"},"created_at":"2025-01-15T10:35:00Z","ephemeral":true}
	]`
	return func(_ string, name string, args ...string) ([]byte, error) {
		if name != "bd" || len(args) == 0 || args[0] != "ready" {
			return nil, fmt.Errorf("unexpected command: %s %s", name, strings.Join(args, " "))
		}
		return []byte(rows), nil
	}
}
