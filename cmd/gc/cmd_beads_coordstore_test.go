package main

import (
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestCopyBeadsIntoCoordstoreDryRunCountsDepsWithoutTarget(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	source := beads.NewMemStoreFrom(2, []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}, []beads.Dep{
		{IssueID: "ga-2", DependsOnID: "ga-1", Type: "blocks"},
		{IssueID: "ga-2", DependsOnID: "ga-missing", Type: "blocks"},
	})

	summary, err := copyBeadsIntoCoordstore(source, nil, true)
	if err != nil {
		t.Fatalf("copyBeadsIntoCoordstore dry run: %v", err)
	}
	if summary.SourceCount != 2 || summary.Deps != 1 || summary.Imported != 0 || summary.Skipped != 0 || !summary.DryRun {
		t.Fatalf("dry-run summary = %+v, want source=2 importable deps=1 no writes", summary)
	}
}

func TestDiffCoordstoreShadowDetectsDependencyMismatch(t *testing.T) {
	created := time.Unix(100, 0).UTC()
	sourceBeads := []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}
	targetBeads := []beads.Bead{
		{ID: "ga-1", Title: "blocker", Status: "open", Type: "task", CreatedAt: created, UpdatedAt: created},
		{ID: "ga-2", Title: "work", Status: "open", Type: "task", CreatedAt: created.Add(time.Second), UpdatedAt: created.Add(time.Second)},
	}
	source := beads.NewMemStoreFrom(2, sourceBeads, []beads.Dep{
		{IssueID: "ga-2", DependsOnID: "ga-1", Type: "blocks"},
	})
	target := beads.NewMemStoreFrom(2, targetBeads, nil)

	summary, err := diffCoordstoreShadow(source, target)
	if err != nil {
		t.Fatalf("diffCoordstoreShadow: %v", err)
	}
	if summary.OK {
		t.Fatal("shadow summary OK = true, want dependency mismatch")
	}
	if !reflect.DeepEqual(summary.Corrupted, []string{"ga-2"}) {
		t.Fatalf("corrupted = %+v, want [ga-2]", summary.Corrupted)
	}
}

func TestCoordstoreImportShadowRoundTripsBdUpdatedAt(t *testing.T) {
	created := time.Date(2026, 5, 30, 6, 52, 8, 0, time.UTC)
	updated := time.Date(2026, 5, 30, 23, 52, 11, 0, time.UTC)
	source := beads.NewBdStore("/city", fakeCoordstoreBdRunner(map[string][]byte{
		`bd list --json --all --include-infra --include-gates --limit 0`: []byte(`[
			{
				"id":"ga-updated",
				"title":"updated bead",
				"status":"closed",
				"issue_type":"task",
				"created_at":"` + created.Format(time.RFC3339) + `",
				"updated_at":"` + updated.Format(time.RFC3339) + `"
			}
		]`),
		`bd query --json ephemeral=true --all --limit 0`: []byte(`[]`),
		`bd dep list ga-updated --json`:                  []byte(`[]`),
	}))
	target := newPreservingCoordstoreTestStore()

	importSummary, err := copyBeadsIntoCoordstore(source, target, false)
	if err != nil {
		t.Fatalf("copyBeadsIntoCoordstore: %v", err)
	}
	if importSummary.SourceCount != 1 || importSummary.Imported != 1 || importSummary.Skipped != 0 {
		t.Fatalf("import summary = %+v, want one imported bead", importSummary)
	}
	imported, err := target.Get("ga-updated")
	if err != nil {
		t.Fatalf("target Get: %v", err)
	}
	if !imported.UpdatedAt.Equal(updated) {
		t.Fatalf("imported UpdatedAt = %s, want %s", imported.UpdatedAt, updated)
	}
	shadow, err := diffCoordstoreShadow(source, target)
	if err != nil {
		t.Fatalf("diffCoordstoreShadow: %v", err)
	}
	if !shadow.OK {
		t.Fatalf("shadow summary = %+v, want ok", shadow)
	}
}

func fakeCoordstoreBdRunner(responses map[string][]byte) beads.CommandRunner {
	return func(_, name string, args ...string) ([]byte, error) {
		key := name + " " + strings.Join(args, " ")
		if out, ok := responses[key]; ok {
			return out, nil
		}
		return nil, fmt.Errorf("unexpected command: %s", key)
	}
}

type preservingCoordstoreTestStore struct {
	*beads.MemStore
	seq   int
	rows  map[string]beads.Bead
	order []string
	deps  []beads.Dep
}

func newPreservingCoordstoreTestStore() *preservingCoordstoreTestStore {
	return &preservingCoordstoreTestStore{
		MemStore: beads.NewMemStore(),
		rows:     make(map[string]beads.Bead),
	}
}

func (s *preservingCoordstoreTestStore) Create(b beads.Bead) (beads.Bead, error) {
	if b.ID == "" {
		s.seq++
		b.ID = fmt.Sprintf("gc-%d", s.seq)
	}
	if _, ok := s.rows[b.ID]; ok {
		return beads.Bead{}, fmt.Errorf("duplicate bead %q", b.ID)
	}
	if b.Status == "" {
		b.Status = "open"
	}
	if b.Type == "" {
		b.Type = "task"
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now().UTC()
	}
	if b.UpdatedAt.IsZero() {
		b.UpdatedAt = b.CreatedAt
	}
	s.rows[b.ID] = b
	s.order = append(s.order, b.ID)
	return b, nil
}

func (s *preservingCoordstoreTestStore) Get(id string) (beads.Bead, error) {
	b, ok := s.rows[id]
	if !ok {
		return beads.Bead{}, beads.ErrNotFound
	}
	return b, nil
}

func (s *preservingCoordstoreTestStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	if !query.HasFilter() && !query.AllowScan {
		return nil, beads.ErrQueryRequiresScan
	}
	out := make([]beads.Bead, 0, len(s.rows))
	for _, id := range s.order {
		out = append(out, s.rows[id])
	}
	return beads.ApplyListQuery(out, query), nil
}

func (s *preservingCoordstoreTestStore) DepAdd(issueID, dependsOnID, depType string) error {
	if _, ok := s.rows[issueID]; !ok {
		return beads.ErrNotFound
	}
	if _, ok := s.rows[dependsOnID]; !ok {
		return beads.ErrNotFound
	}
	if depType == "" {
		depType = "blocks"
	}
	s.deps = append(s.deps, beads.Dep{IssueID: issueID, DependsOnID: dependsOnID, Type: depType})
	return nil
}

func (s *preservingCoordstoreTestStore) DepList(id, direction string) ([]beads.Dep, error) {
	if _, ok := s.rows[id]; !ok {
		return nil, beads.ErrNotFound
	}
	var out []beads.Dep
	for _, dep := range s.deps {
		switch {
		case direction == "up" && dep.DependsOnID == id:
			out = append(out, dep)
		case direction != "up" && dep.IssueID == id:
			out = append(out, dep)
		}
	}
	return out, nil
}
