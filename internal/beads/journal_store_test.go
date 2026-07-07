package beads

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
)

func newJournalTestStore(t *testing.T) *JournalStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "city-under-test"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	return NewJournalStore(gs)
}

func TestJournalStoreCapabilities(t *testing.T) {
	s := newJournalTestStore(t)
	if got := s.IDPrefix(); got != "gcg" {
		t.Fatalf("IDPrefix() = %q, want gcg", got)
	}
	if !s.AtomicTx() {
		t.Fatal("AtomicTx() = false, want true")
	}
	if !s.SupportsEphemeralGraphApply() {
		t.Fatal("SupportsEphemeralGraphApply() = false, want true")
	}
	if err := s.Ping(); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

func TestJournalStoreCreateGetRoundTrip(t *testing.T) {
	s := newJournalTestStore(t)
	p := 1
	in := Bead{
		Title:       "hello",
		Type:        "task",
		Priority:    &p,
		Description: "do the thing",
		Assignee:    "worker-1",
		From:        "planner",
		Labels:      []string{"alpha", "beta"},
		Metadata:    StringMap{"k1": "v1", "k2": "v2"},
	}
	created, err := s.Create(in)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(created.ID, "gcg-j") {
		t.Fatalf("minted ID %q lacks the gcg-j journal marker shape", created.ID)
	}
	if s.IDPrefix() != "gcg" {
		t.Fatalf("IDPrefix() = %q, want gcg (marker must not leak into the prefix)", s.IDPrefix())
	}
	if created.Status != "open" {
		t.Fatalf("Status = %q, want open", created.Status)
	}
	if created.CreatedAt.IsZero() {
		t.Fatal("CreatedAt is zero")
	}

	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "hello" || got.Description != "do the thing" || got.Assignee != "worker-1" || got.From != "planner" {
		t.Fatalf("round-trip mismatch: %+v", got)
	}
	if got.Priority == nil || *got.Priority != 1 {
		t.Fatalf("Priority = %v, want 1", got.Priority)
	}
	if len(got.Labels) != 2 || got.Labels[0] != "alpha" || got.Labels[1] != "beta" {
		t.Fatalf("Labels = %v", got.Labels)
	}
	if got.Metadata["k1"] != "v1" || got.Metadata["k2"] != "v2" {
		t.Fatalf("Metadata = %v", got.Metadata)
	}
	if got.IsBlocked == nil || *got.IsBlocked {
		t.Fatalf("IsBlocked = %v, want non-nil false", got.IsBlocked)
	}
}

func TestJournalStoreGetNotFound(t *testing.T) {
	s := newJournalTestStore(t)
	_, err := s.Get("gcg-nope")
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing = %v, want ErrNotFound", err)
	}
}

func TestJournalStorePriorityTwoReadsBackNil(t *testing.T) {
	s := newJournalTestStore(t)
	p := 2
	created, err := s.Create(Bead{Title: "p2", Priority: &p})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Priority != nil {
		t.Fatalf("Priority = %v, want nil (P2 sparse convention)", got.Priority)
	}
}

func TestJournalStoreReadyBlockingDep(t *testing.T) {
	s := newJournalTestStore(t)
	a, err := s.Create(Bead{Title: "A"})
	if err != nil {
		t.Fatalf("Create A: %v", err)
	}
	b, err := s.Create(Bead{Title: "B"})
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	if err := s.DepAdd(b.ID, a.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !journalContainsID(ready, a.ID) {
		t.Fatalf("A should be ready, got %v", journalIDsOf(ready))
	}
	if journalContainsID(ready, b.ID) {
		t.Fatalf("B should be blocked, got %v", journalIDsOf(ready))
	}

	// B's stored projection must also read as blocked.
	gotB, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get B: %v", err)
	}
	if gotB.IsBlocked == nil || !*gotB.IsBlocked {
		t.Fatalf("B.IsBlocked = %v, want true", gotB.IsBlocked)
	}

	// Closing A unblocks B.
	if err := s.Close(a.ID); err != nil {
		t.Fatalf("Close A: %v", err)
	}
	ready, err = s.Ready()
	if err != nil {
		t.Fatalf("Ready after close: %v", err)
	}
	if journalContainsID(ready, a.ID) {
		t.Fatalf("A closed should not be ready, got %v", journalIDsOf(ready))
	}
	if !journalContainsID(ready, b.ID) {
		t.Fatalf("B should be ready after A closes, got %v", journalIDsOf(ready))
	}
}

func TestJournalStoreReadyDanglingDepBlocks(t *testing.T) {
	s := newJournalTestStore(t)
	c, err := s.Create(Bead{Title: "C"})
	if err != nil {
		t.Fatalf("Create C: %v", err)
	}
	if err := s.DepAdd(c.ID, "gcg-missing", "blocks"); err != nil {
		t.Fatalf("DepAdd dangling: %v", err)
	}
	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if journalContainsID(ready, c.ID) {
		t.Fatalf("C with dangling dep must block (D-4), got %v", journalIDsOf(ready))
	}
}

func TestJournalStoreReadyLimitAndExcludedTypes(t *testing.T) {
	s := newJournalTestStore(t)
	if _, err := s.Create(Bead{Title: "task-1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(Bead{Title: "task-2"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	// A molecule is an excluded infrastructure type: never actionable Ready work.
	if _, err := s.Create(Bead{Title: "mol", Type: "molecule"}); err != nil {
		t.Fatalf("Create molecule: %v", err)
	}
	ready, err := s.Ready(ReadyQuery{Limit: 1})
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if len(ready) != 1 {
		t.Fatalf("Ready limit=1 returned %d beads", len(ready))
	}
	all, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	for _, b := range all {
		if b.Type == "molecule" {
			t.Fatalf("molecule leaked into Ready: %v", journalIDsOf(all))
		}
	}
	if len(all) != 2 {
		t.Fatalf("Ready returned %d actionable beads, want 2", len(all))
	}
}

func TestJournalStoreSetMetadataAndClose(t *testing.T) {
	s := newJournalTestStore(t)
	created, err := s.Create(Bead{Title: "x"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := s.SetMetadata(created.ID, "gc.routed_to", "worker-9"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	if err := s.SetMetadataBatch(created.ID, map[string]string{"a": "1", "b": "2"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["gc.routed_to"] != "worker-9" || got.Metadata["a"] != "1" || got.Metadata["b"] != "2" {
		t.Fatalf("metadata = %v", got.Metadata)
	}

	if err := s.Close(created.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// Idempotent.
	if err := s.Close(created.ID); err != nil {
		t.Fatalf("Close again: %v", err)
	}
	got, err = s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("Status = %q, want closed", got.Status)
	}
	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if journalContainsID(ready, created.ID) {
		t.Fatal("closed bead must not be Ready")
	}

	if err := s.Reopen(created.ID); err != nil {
		t.Fatalf("Reopen: %v", err)
	}
	got, err = s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Status != "open" {
		t.Fatalf("Status = %q, want open after Reopen", got.Status)
	}
}

func TestJournalStoreUpdate(t *testing.T) {
	s := newJournalTestStore(t)
	created, err := s.Create(Bead{Title: "orig", Labels: []string{"keep", "drop"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	title := "renamed"
	assignee := "w2"
	if err := s.Update(created.ID, UpdateOpts{
		Title:        &title,
		Assignee:     &assignee,
		Labels:       []string{"added"},
		RemoveLabels: []string{"drop"},
		Metadata:     map[string]string{"m": "1"},
	}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Title != "renamed" || got.Assignee != "w2" {
		t.Fatalf("update mismatch: %+v", got)
	}
	if !journalHasLabelIn(got.Labels, "keep") || !journalHasLabelIn(got.Labels, "added") || journalHasLabelIn(got.Labels, "drop") {
		t.Fatalf("labels = %v", got.Labels)
	}
	if got.Metadata["m"] != "1" {
		t.Fatalf("metadata = %v", got.Metadata)
	}
}

func TestJournalStoreUpdateNotFound(t *testing.T) {
	s := newJournalTestStore(t)
	title := "x"
	if err := s.Update("gcg-missing", UpdateOpts{Title: &title}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Update missing = %v, want ErrNotFound", err)
	}
}

func TestJournalStoreDepAddListRemove(t *testing.T) {
	s := newJournalTestStore(t)
	a, _ := s.Create(Bead{Title: "A"})
	b, _ := s.Create(Bead{Title: "B"})
	if err := s.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	down, err := s.DepList(a.ID, "down")
	if err != nil {
		t.Fatalf("DepList down: %v", err)
	}
	if len(down) != 1 || down[0].IssueID != a.ID || down[0].DependsOnID != b.ID || down[0].Type != "blocks" {
		t.Fatalf("down deps = %+v", down)
	}
	up, err := s.DepList(b.ID, "up")
	if err != nil {
		t.Fatalf("DepList up: %v", err)
	}
	if len(up) != 1 || up[0].IssueID != a.ID || up[0].DependsOnID != b.ID {
		t.Fatalf("up deps = %+v", up)
	}

	if err := s.DepRemove(a.ID, b.ID); err != nil {
		t.Fatalf("DepRemove: %v", err)
	}
	down, err = s.DepList(a.ID, "down")
	if err != nil {
		t.Fatalf("DepList down after remove: %v", err)
	}
	if len(down) != 0 {
		t.Fatalf("deps after remove = %+v", down)
	}
}

func TestJournalStoreCreateWithDependencies(t *testing.T) {
	s := newJournalTestStore(t)
	a, _ := s.Create(Bead{Title: "A"})
	b, err := s.Create(Bead{Title: "B", Dependencies: []Dep{{DependsOnID: a.ID, Type: "blocks"}}})
	if err != nil {
		t.Fatalf("Create B: %v", err)
	}
	got, err := s.Get(b.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if len(got.Dependencies) != 1 || got.Dependencies[0].DependsOnID != a.ID {
		t.Fatalf("deps = %+v", got.Dependencies)
	}
}

func TestJournalStoreListFilters(t *testing.T) {
	s := newJournalTestStore(t)
	if _, err := s.Create(Bead{Title: "open-1", Assignee: "w1", Labels: []string{"team-a"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(Bead{Title: "open-2", Assignee: "w2", Labels: []string{"team-b"}}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	closed, _ := s.Create(Bead{Title: "closed-1", Assignee: "w1"})
	if err := s.Close(closed.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}

	// Filterless without AllowScan is rejected.
	if _, err := s.List(ListQuery{}); !errors.Is(err, ErrQueryRequiresScan) {
		t.Fatalf("filterless List = %v, want ErrQueryRequiresScan", err)
	}

	byAssignee, err := s.List(ListQuery{Assignee: "w1"})
	if err != nil {
		t.Fatalf("List assignee: %v", err)
	}
	// Default excludes closed.
	if len(byAssignee) != 1 || byAssignee[0].Title != "open-1" {
		t.Fatalf("assignee filter = %v", journalTitlesOf(byAssignee))
	}

	withClosed, err := s.List(ListQuery{Assignee: "w1", IncludeClosed: true})
	if err != nil {
		t.Fatalf("List include closed: %v", err)
	}
	if len(withClosed) != 2 {
		t.Fatalf("include-closed filter = %v", journalTitlesOf(withClosed))
	}

	byLabel, err := s.ListByLabel("team-b", 0)
	if err != nil {
		t.Fatalf("ListByLabel: %v", err)
	}
	if len(byLabel) != 1 || byLabel[0].Title != "open-2" {
		t.Fatalf("label filter = %v", journalTitlesOf(byLabel))
	}
}

func TestJournalStoreChildren(t *testing.T) {
	s := newJournalTestStore(t)
	parent, _ := s.Create(Bead{Title: "parent"})
	c1, _ := s.Create(Bead{Title: "c1", ParentID: parent.ID})
	c2, _ := s.Create(Bead{Title: "c2", ParentID: parent.ID})
	_, _ = s.Create(Bead{Title: "unrelated"})

	kids, err := s.Children(parent.ID)
	if err != nil {
		t.Fatalf("Children: %v", err)
	}
	if len(kids) != 2 || !journalContainsID(kids, c1.ID) || !journalContainsID(kids, c2.ID) {
		t.Fatalf("children = %v", journalIDsOf(kids))
	}
}

func TestJournalStoreCount(t *testing.T) {
	s := newJournalTestStore(t)
	if _, err := s.Create(Bead{Title: "t1", Assignee: "w1"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := s.Create(Bead{Title: "m1", Assignee: "w1", Type: "molecule"}); err != nil {
		t.Fatalf("Create: %v", err)
	}
	n, err := s.Count(context.Background(), ListQuery{Assignee: "w1"})
	if err != nil {
		t.Fatalf("Count: %v", err)
	}
	if n != 2 {
		t.Fatalf("Count = %d, want 2", n)
	}
	n, err = s.Count(context.Background(), ListQuery{Assignee: "w1"}, "molecule")
	if err != nil {
		t.Fatalf("Count exclude: %v", err)
	}
	if n != 1 {
		t.Fatalf("Count excluding molecule = %d, want 1", n)
	}
}

func TestJournalStoreDelete(t *testing.T) {
	s := newJournalTestStore(t)
	ctx := context.Background()
	target, _ := s.Create(Bead{Title: "target"})
	created, _ := s.Create(Bead{
		Title:        "doomed",
		Labels:       []string{"l"},
		Metadata:     StringMap{"k": "v"},
		Dependencies: []Dep{{DependsOnID: target.ID, Type: "blocks"}},
	})

	assertCount := func(what, query string, want int) {
		t.Helper()
		var n int
		if err := s.gs.DB().QueryRowContext(ctx, query, created.ID).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", what, err)
		}
		if n != want {
			t.Fatalf("%s rows for %q = %d, want %d", what, created.ID, n, want)
		}
	}
	// The child rows exist before the delete, so the post-delete assertions below
	// are non-vacuous (not empty-vs-empty).
	assertCount("node_labels", `SELECT COUNT(*) FROM node_labels WHERE node_id = ?`, 1)
	assertCount("node_metadata", `SELECT COUNT(*) FROM node_metadata WHERE node_id = ?`, 1)
	assertCount("edges", `SELECT COUNT(*) FROM edges WHERE from_id = ?`, 1)

	if err := s.Delete(created.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get(created.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want ErrNotFound", err)
	}
	// node_labels, node_metadata, and outbound edges cascade away with the node.
	assertCount("node_labels", `SELECT COUNT(*) FROM node_labels WHERE node_id = ?`, 0)
	assertCount("node_metadata", `SELECT COUNT(*) FROM node_metadata WHERE node_id = ?`, 0)
	assertCount("edges", `SELECT COUNT(*) FROM edges WHERE from_id = ?`, 0)

	if err := s.Delete("gcg-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Delete missing = %v, want ErrNotFound", err)
	}
}

func TestJournalStoreCloseAll(t *testing.T) {
	s := newJournalTestStore(t)
	a, _ := s.Create(Bead{Title: "a"})
	b, _ := s.Create(Bead{Title: "b"})
	if err := s.Close(b.ID); err != nil {
		t.Fatalf("Close: %v", err)
	}
	n, err := s.CloseAll([]string{a.ID, b.ID}, map[string]string{"reason": "done"})
	if err != nil {
		t.Fatalf("CloseAll: %v", err)
	}
	if n != 1 {
		t.Fatalf("CloseAll closed %d, want 1 (b already closed)", n)
	}
	got, _ := s.Get(a.ID)
	if got.Status != "closed" || got.Metadata["reason"] != "done" {
		t.Fatalf("a after CloseAll = %+v", got)
	}
}

func TestJournalStoreReleaseIfCurrent(t *testing.T) {
	s := newJournalTestStore(t)
	created, _ := s.Create(Bead{Title: "claimed"})
	inProgress := "in_progress"
	assignee := "w1"
	if err := s.Update(created.ID, UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("Update: %v", err)
	}
	// Wrong assignee: no release.
	released, err := s.ReleaseIfCurrent(created.ID, "someone-else")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if released {
		t.Fatal("released with wrong assignee")
	}
	// Correct assignee: releases.
	released, err = s.ReleaseIfCurrent(created.ID, "w1")
	if err != nil {
		t.Fatalf("ReleaseIfCurrent: %v", err)
	}
	if !released {
		t.Fatal("expected release with current assignee")
	}
	got, _ := s.Get(created.ID)
	if got.Status != "open" || got.Assignee != "" {
		t.Fatalf("after release = %+v", got)
	}
}

func TestJournalStoreTxAtomicRollback(t *testing.T) {
	s := newJournalTestStore(t)
	sentinel := errors.New("boom")
	var createdID string
	err := s.Tx("commit-msg", func(tx Tx) error {
		b, err := tx.Create(Bead{Title: "in-tx"})
		if err != nil {
			return err
		}
		createdID = b.ID
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Tx err = %v, want sentinel", err)
	}
	if createdID == "" {
		t.Fatal("expected an ID minted inside the tx")
	}
	if _, err := s.Get(createdID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("bead survived rolled-back tx: %v", err)
	}
}

func TestJournalStoreTxCommits(t *testing.T) {
	s := newJournalTestStore(t)
	var id string
	err := s.Tx("commit-msg", func(tx Tx) error {
		b, err := tx.Create(Bead{Title: "committed"})
		if err != nil {
			return err
		}
		id = b.ID
		return tx.SetMetadataBatch(id, map[string]string{"done": "yes"})
	})
	if err != nil {
		t.Fatalf("Tx: %v", err)
	}
	got, err := s.Get(id)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.Metadata["done"] != "yes" {
		t.Fatalf("metadata = %v", got.Metadata)
	}
}

func TestJournalStoreApplyGraphPlan(t *testing.T) {
	s := newJournalTestStore(t)
	plan := &GraphApplyPlan{
		CommitMessage: "materialize molecule",
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "root", Type: "molecule"},
			{Key: "step1", Title: "step 1", ParentKey: "root"},
			{Key: "step2", Title: "step 2", ParentKey: "root", Metadata: map[string]string{"phase": "2"}, MetadataRefs: map[string]string{"blocks_on": "step1"}},
		},
		Edges: []GraphApplyEdge{
			{FromKey: "step2", ToKey: "step1", Type: "blocks"},
		},
	}
	result, err := s.ApplyGraphPlan(context.Background(), plan)
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	if err := ValidateGraphApplyResult(plan, result); err != nil {
		t.Fatalf("ValidateGraphApplyResult: %v", err)
	}
	for _, key := range []string{"root", "step1", "step2"} {
		id := result.IDs[key]
		if !strings.HasPrefix(id, "gcg-j") {
			t.Fatalf("key %q -> %q lacks the gcg-j journal marker shape", key, id)
		}
		if _, err := s.Get(id); err != nil {
			t.Fatalf("Get materialized %q: %v", key, err)
		}
	}

	// step2 depends on step1, so only step1 is ready among the steps; the root
	// molecule is an excluded type.
	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !journalContainsID(ready, result.IDs["step1"]) {
		t.Fatalf("step1 should be ready, got %v", journalIDsOf(ready))
	}
	if journalContainsID(ready, result.IDs["step2"]) {
		t.Fatalf("step2 should be blocked by step1, got %v", journalIDsOf(ready))
	}
	if journalContainsID(ready, result.IDs["root"]) {
		t.Fatalf("root molecule must be excluded from Ready, got %v", journalIDsOf(ready))
	}

	// MetadataRefs resolved to the minted step1 ID.
	step2, err := s.Get(result.IDs["step2"])
	if err != nil {
		t.Fatalf("Get step2: %v", err)
	}
	if step2.Metadata["blocks_on"] != result.IDs["step1"] {
		t.Fatalf("MetadataRefs = %v, want blocks_on=%s", step2.Metadata, result.IDs["step1"])
	}
	if step2.ParentID != result.IDs["root"] {
		t.Fatalf("step2 ParentID = %q, want %q", step2.ParentID, result.IDs["root"])
	}

	// Closing step1 unblocks step2.
	if err := s.Close(result.IDs["step1"]); err != nil {
		t.Fatalf("Close step1: %v", err)
	}
	ready, err = s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if !journalContainsID(ready, result.IDs["step2"]) {
		t.Fatalf("step2 should be ready after step1 closes, got %v", journalIDsOf(ready))
	}
}

func TestJournalStoreApplyGraphPlanEphemeralStorage(t *testing.T) {
	s := newJournalTestStore(t)
	plan := &GraphApplyPlan{Nodes: []GraphApplyNode{{Key: "n", Title: "ephemeral node"}}}
	result, err := s.ApplyGraphPlanWithStorage(context.Background(), plan, StorageEphemeral)
	if err != nil {
		t.Fatalf("ApplyGraphPlanWithStorage: %v", err)
	}
	got, err := s.Get(result.IDs["n"])
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.Ephemeral {
		t.Fatalf("node not ephemeral: %+v", got)
	}
	// TierIssues hides ephemeral rows; TierWisps surfaces them.
	if issues, err := s.List(ListQuery{AllowScan: true, TierMode: TierIssues}); err != nil {
		t.Fatalf("List issues: %v", err)
	} else if journalContainsID(issues, got.ID) {
		t.Fatalf("ephemeral row leaked into TierIssues: %v", journalIDsOf(issues))
	}
	if wisps, err := s.List(ListQuery{AllowScan: true, TierMode: TierWisps}); err != nil {
		t.Fatalf("List wisps: %v", err)
	} else if !journalContainsID(wisps, got.ID) {
		t.Fatalf("ephemeral row missing from TierWisps: %v", journalIDsOf(wisps))
	}
}

func TestJournalStoreCreateWithStorage(t *testing.T) {
	s := newJournalTestStore(t)
	// StorageDefault preserves the caller's ephemeral hint.
	eph, err := s.CreateWithStorage(Bead{Title: "eph", Ephemeral: true}, StorageDefault)
	if err != nil {
		t.Fatalf("CreateWithStorage default: %v", err)
	}
	if got, _ := s.Get(eph.ID); !got.Ephemeral {
		t.Fatalf("default did not preserve ephemeral hint: %+v", got)
	}
	// StorageHistory clears the caller's hints.
	forced, err := s.CreateWithStorage(Bead{Title: "forced", Ephemeral: true}, StorageHistory)
	if err != nil {
		t.Fatalf("CreateWithStorage history: %v", err)
	}
	if got, _ := s.Get(forced.ID); got.Ephemeral || got.NoHistory {
		t.Fatalf("history storage did not clear hints: %+v", got)
	}
}

func TestJournalStoreFoldOwnedWriteClosed(t *testing.T) {
	s := newJournalTestStore(t)
	ctx := context.Background()
	db := s.gs.DB()
	// Insert a fold-owned Tier-A row by opening the write gate directly, mimicking
	// the fold applier. The façade must refuse to mutate it.
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO nodes (id, title, status, created_at, fold_owned, stream_id)
		VALUES ('gcg-fold-1', 'fold owned', 'open', ?, 1, 'gcg-root')`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert fold-owned node: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}

	title := "hijack"
	if err := s.Update("gcg-fold-1", UpdateOpts{Title: &title}); !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("Update fold-owned = %v, want ErrFoldOwnedWriteClosed", err)
	}
	if err := s.Close("gcg-fold-1"); !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("Close fold-owned = %v, want ErrFoldOwnedWriteClosed", err)
	}
	if err := s.Delete("gcg-fold-1"); !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("Delete fold-owned = %v, want ErrFoldOwnedWriteClosed", err)
	}
	if err := s.SetMetadata("gcg-fold-1", "k", "v"); !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("SetMetadata fold-owned = %v, want ErrFoldOwnedWriteClosed", err)
	}
	// Reads scope to fold_owned=0: the façade does not surface the fold row.
	if _, err := s.Get("gcg-fold-1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get fold-owned = %v, want ErrNotFound (façade sees only fold_owned=0)", err)
	}
}

// TestJournalStoreReadInsideTxDoesNotDeadlock pins H1: a façade read issued from
// inside a write Tx callback must return, not self-deadlock waiting for the
// single write connection the Tx already holds. Reads are routed to the pooled
// read handle, which also means an in-tx read observes only committed state.
func TestJournalStoreReadInsideTxDoesNotDeadlock(t *testing.T) {
	s := newJournalTestStore(t)
	seed, err := s.Create(Bead{Title: "seed"})
	if err != nil {
		t.Fatalf("Create seed: %v", err)
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Tx("in-tx-reads", func(tx Tx) error {
			if _, err := s.Get(seed.ID); err != nil {
				return fmt.Errorf("Get(seed) inside tx: %w", err)
			}
			if _, err := s.Ready(); err != nil {
				return fmt.Errorf("Ready inside tx: %w", err)
			}
			inTx, err := tx.Create(Bead{Title: "in-tx"})
			if err != nil {
				return err
			}
			// The read handle sees only committed state: the uncommitted in-tx row
			// is invisible — and, decisively, the read returns rather than hangs.
			if _, err := s.Get(inTx.ID); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("Get(in-tx) inside tx: want pre-commit ErrNotFound, got %w", err)
			}
			return nil
		})
	}()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Tx with in-callback reads: %v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("Tx callback deadlocked: a façade read blocked on the single write connection")
	}

	// Read-your-writes still holds for committed writes: the seed is visible
	// outside the tx.
	if _, err := s.Get(seed.ID); err != nil {
		t.Fatalf("Get(seed) after tx: %v", err)
	}
}

// TestJournalStoreRejectsCallerSuppliedID pins M1: the store mints its own
// gcg-j<seq> ids, so any caller-supplied id is rejected (a gcg-j* id would wedge
// the mint sequence; a foreign-shaped id would break the residence marker). A
// blank id mints normally.
func TestJournalStoreRejectsCallerSuppliedID(t *testing.T) {
	s := newJournalTestStore(t)
	for _, id := range []string{"gcg-j5", "gcy-1", "gcg-7"} {
		if _, err := s.Create(Bead{Title: "x", ID: id}); err == nil {
			t.Fatalf("Create with caller-supplied id %q = nil error, want rejection", id)
		}
	}
	minted, err := s.Create(Bead{Title: "ok"})
	if err != nil {
		t.Fatalf("Create blank id: %v", err)
	}
	if !strings.HasPrefix(minted.ID, "gcg-j") {
		t.Fatalf("minted id %q lacks the gcg-j shape", minted.ID)
	}
}

// TestJournalStoreCreatePreservesCreatedAt pins L1: a caller-supplied CreatedAt
// (P1.5 rehoming/backfill) round-trips; a blank one is stamped.
func TestJournalStoreCreatePreservesCreatedAt(t *testing.T) {
	s := newJournalTestStore(t)
	past := time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)
	created, err := s.Create(Bead{Title: "backfilled", CreatedAt: past})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.CreatedAt.Equal(past) {
		t.Fatalf("returned CreatedAt = %v, want %v", created.CreatedAt, past)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.CreatedAt.Equal(past) {
		t.Fatalf("round-tripped CreatedAt = %v, want %v", got.CreatedAt, past)
	}
	stamped, err := s.Create(Bead{Title: "fresh"})
	if err != nil {
		t.Fatalf("Create fresh: %v", err)
	}
	if stamped.CreatedAt.IsZero() {
		t.Fatal("blank CreatedAt was not stamped")
	}
}

// TestJournalStoreParentChildDepProjectsParentID pins M4: a parent-child
// relationship expressed as a dependency (via Create) or via DepAdd projects onto
// the parent_id column, so Children() and Get().ParentID agree — a copied
// parent-child dep no longer orphans the bead.
func TestJournalStoreParentChildDepProjectsParentID(t *testing.T) {
	s := newJournalTestStore(t)
	parent, err := s.Create(Bead{Title: "parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}

	child, err := s.Create(Bead{Title: "child", Dependencies: []Dep{{DependsOnID: parent.ID, Type: "parent-child"}}})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	gotChild, err := s.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if gotChild.ParentID != parent.ID {
		t.Fatalf("child.ParentID = %q, want %q (parent-child dep must project onto parent_id)", gotChild.ParentID, parent.ID)
	}
	if kids, err := s.Children(parent.ID); err != nil {
		t.Fatalf("Children: %v", err)
	} else if !journalContainsID(kids, child.ID) {
		t.Fatalf("Children(parent) = %v, want to include child %q", journalIDsOf(kids), child.ID)
	}

	orphan, err := s.Create(Bead{Title: "orphan"})
	if err != nil {
		t.Fatalf("Create orphan: %v", err)
	}
	if err := s.DepAdd(orphan.ID, parent.ID, "parent-child"); err != nil {
		t.Fatalf("DepAdd parent-child: %v", err)
	}
	gotOrphan, err := s.Get(orphan.ID)
	if err != nil {
		t.Fatalf("Get orphan: %v", err)
	}
	if gotOrphan.ParentID != parent.ID {
		t.Fatalf("orphan.ParentID = %q, want %q after DepAdd parent-child", gotOrphan.ParentID, parent.ID)
	}
	if kids, err := s.Children(parent.ID); err != nil {
		t.Fatalf("Children after DepAdd: %v", err)
	} else if !journalContainsID(kids, orphan.ID) {
		t.Fatalf("Children(parent) after DepAdd = %v, want to include orphan %q", journalIDsOf(kids), orphan.ID)
	}
}

// TestJournalStoreDepAddPreservesEdgeMetadata pins M3: DepAdd re-adds edges with
// empty metadata and must not wipe metadata a plan wrote earlier.
func TestJournalStoreDepAddPreservesEdgeMetadata(t *testing.T) {
	s := newJournalTestStore(t)
	ctx := context.Background()
	plan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{{Key: "a", Title: "A"}, {Key: "b", Title: "B"}},
		Edges: []GraphApplyEdge{{FromKey: "b", ToKey: "a", Type: "blocks", Metadata: "edge-meta"}},
	}
	result, err := s.ApplyGraphPlan(ctx, plan)
	if err != nil {
		t.Fatalf("ApplyGraphPlan: %v", err)
	}
	aID, bID := result.IDs["a"], result.IDs["b"]

	edgeMeta := func() string {
		t.Helper()
		var meta string
		if err := s.gs.DB().QueryRowContext(ctx,
			`SELECT metadata FROM edges WHERE from_id = ? AND to_id = ? AND dep_type = 'blocks'`, bID, aID).Scan(&meta); err != nil {
			t.Fatalf("read edge metadata: %v", err)
		}
		return meta
	}
	if got := edgeMeta(); got != "edge-meta" {
		t.Fatalf("plan-written edge metadata = %q, want %q", got, "edge-meta")
	}
	if err := s.DepAdd(bID, aID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}
	if got := edgeMeta(); got != "edge-meta" {
		t.Fatalf("edge metadata after DepAdd = %q, want %q preserved", got, "edge-meta")
	}
}

// TestJournalStoreReadyOrdering pins the canonical (priority, created_at, id)
// ready ordering. It leans on L1's preserved CreatedAt to control the order.
func TestJournalStoreReadyOrdering(t *testing.T) {
	s := newJournalTestStore(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	p1 := 1
	late, err := s.Create(Bead{Title: "p1-late", Priority: &p1, CreatedAt: base.Add(2 * time.Hour)})
	if err != nil {
		t.Fatalf("Create late: %v", err)
	}
	early, err := s.Create(Bead{Title: "p1-early", Priority: &p1, CreatedAt: base.Add(1 * time.Hour)})
	if err != nil {
		t.Fatalf("Create early: %v", err)
	}
	lowPri, err := s.Create(Bead{Title: "p2-earliest", CreatedAt: base}) // nil priority sorts as 2
	if err != nil {
		t.Fatalf("Create lowPri: %v", err)
	}

	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	got := journalIDsOf(ready)
	want := []string{early.ID, late.ID, lowPri.ID}
	if len(got) != len(want) {
		t.Fatalf("Ready = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("Ready order = %v, want %v (priority, created_at, id)", got, want)
		}
	}
}

// TestJournalStoreGraphApplyEdgeGuards pins the graph-apply edge guards: a
// parent-child edge duplicating the ParentKey relationship is silently skipped
// (native parity) and the parent projection still holds; a blocking edge that
// reverses a parent-child relationship is rejected.
func TestJournalStoreGraphApplyEdgeGuards(t *testing.T) {
	s := newJournalTestStore(t)
	ctx := context.Background()

	dupPlan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "parent", Title: "parent"},
			{Key: "child", Title: "child", ParentKey: "parent"},
		},
		Edges: []GraphApplyEdge{{FromKey: "child", ToKey: "parent", Type: "parent-child"}},
	}
	result, err := s.ApplyGraphPlan(ctx, dupPlan)
	if err != nil {
		t.Fatalf("ApplyGraphPlan with duplicate parent-child edge = %v, want nil (skipped)", err)
	}
	if kids, err := s.Children(result.IDs["parent"]); err != nil {
		t.Fatalf("Children: %v", err)
	} else if !journalContainsID(kids, result.IDs["child"]) {
		t.Fatalf("Children(parent) = %v, want to include child", journalIDsOf(kids))
	}

	revPlan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "parent", Title: "parent"},
			{Key: "child", Title: "child", ParentKey: "parent"},
		},
		Edges: []GraphApplyEdge{{FromKey: "parent", ToKey: "child", Type: "blocks"}},
	}
	if _, err := s.ApplyGraphPlan(ctx, revPlan); err == nil {
		t.Fatal("ApplyGraphPlan with a blocking reverse of a parent-child relationship = nil, want error")
	}
}

// TestJournalStoreDeferUntilRoundTripAndReadyExclusion pins DeferUntil: it
// round-trips through Create/Get and a future defer excludes a bead from Ready.
func TestJournalStoreDeferUntilRoundTripAndReadyExclusion(t *testing.T) {
	s := newJournalTestStore(t)
	future := time.Now().UTC().Add(24 * time.Hour).Truncate(time.Second)
	created, err := s.Create(Bead{Title: "deferred", DeferUntil: &future})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.DeferUntil == nil || !got.DeferUntil.Equal(future) {
		t.Fatalf("DeferUntil = %v, want %v", got.DeferUntil, future)
	}
	ready, err := s.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	if journalContainsID(ready, created.ID) {
		t.Fatalf("a future-deferred bead must be excluded from Ready, got %v", journalIDsOf(ready))
	}
}

// TestJournalStoreSeqPersistsAcrossReopen pins that the mint counter is persisted:
// a store reopened at the same path mints strictly after the pre-close ids, never
// reusing one.
func TestJournalStoreSeqPersistsAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "journal.db")
	ctx := context.Background()

	gs1, err := graphstore.Open(ctx, path, graphstore.Options{CityID: "seq-city"})
	if err != nil {
		t.Fatalf("open 1: %v", err)
	}
	s1 := NewJournalStore(gs1)
	first, err := s1.Create(Bead{Title: "first"})
	if err != nil {
		t.Fatalf("Create first: %v", err)
	}
	second, err := s1.Create(Bead{Title: "second"})
	if err != nil {
		t.Fatalf("Create second: %v", err)
	}
	if err := gs1.Close(); err != nil {
		t.Fatalf("close 1: %v", err)
	}

	gs2, err := graphstore.Open(ctx, path, graphstore.Options{CityID: "seq-city"})
	if err != nil {
		t.Fatalf("open 2: %v", err)
	}
	t.Cleanup(func() { _ = gs2.Close() })
	s2 := NewJournalStore(gs2)
	third, err := s2.Create(Bead{Title: "third"})
	if err != nil {
		t.Fatalf("Create third: %v", err)
	}

	seen := map[string]bool{}
	for _, id := range []string{first.ID, second.ID, third.ID} {
		if seen[id] {
			t.Fatalf("mint reused id %q across reopen", id)
		}
		seen[id] = true
	}
}

// TestJournalStoreFoldOwnedGuardsDepAndReopen pins that DepAdd, DepRemove, and
// Reopen also refuse to mutate a fold-owned (Tier-A) row, completing the
// write-closure coverage alongside Update/Close/Delete/SetMetadata.
func TestJournalStoreFoldOwnedGuardsDepAndReopen(t *testing.T) {
	s := newJournalTestStore(t)
	ctx := context.Background()
	db := s.gs.DB()
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO nodes (id, title, status, created_at, fold_owned, stream_id)
		VALUES ('gcg-fold-dep', 'fold owned', 'closed', ?, 1, 'gcg-root')`,
		time.Now().UTC().Format(time.RFC3339Nano),
	); err != nil {
		t.Fatalf("insert fold-owned node: %v", err)
	}
	if _, err := db.ExecContext(ctx, `UPDATE tier_a_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}

	if err := s.DepAdd("gcg-fold-dep", "gcg-target", "blocks"); !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("DepAdd on fold-owned = %v, want ErrFoldOwnedWriteClosed", err)
	}
	if err := s.DepRemove("gcg-fold-dep", "gcg-target"); !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("DepRemove on fold-owned = %v, want ErrFoldOwnedWriteClosed", err)
	}
	if err := s.Reopen("gcg-fold-dep"); !errors.Is(err, ErrFoldOwnedWriteClosed) {
		t.Fatalf("Reopen on fold-owned = %v, want ErrFoldOwnedWriteClosed", err)
	}
}

// TestJournalStoreHydrationSnapshotConsistency pins M2: hydration runs the node
// SELECT and its N+1 child queries inside one read snapshot, so a concurrent
// commit can never tear a hydrated bead. A writer flips a bead's label and
// metadata "generation" together in one write tx; a snapshot-consistent read must
// always observe the two in agreement. Race-tolerant by design (run under -race).
func TestJournalStoreHydrationSnapshotConsistency(t *testing.T) {
	s := newJournalTestStore(t)
	x, err := s.Create(Bead{Title: "x", Labels: []string{"gen-0"}, Metadata: StringMap{"gen": "0"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const iterations = 200
	writerDone := make(chan error, 1)
	go func() {
		for i := 1; i <= iterations; i++ {
			cur := strconv.Itoa(i)
			prev := "gen-" + strconv.Itoa(i-1)
			if err := s.Update(x.ID, UpdateOpts{
				Labels:       []string{"gen-" + cur},
				RemoveLabels: []string{prev},
				Metadata:     map[string]string{"gen": cur},
			}); err != nil {
				writerDone <- err
				return
			}
		}
		writerDone <- nil
	}()

	for {
		select {
		case err := <-writerDone:
			if err != nil {
				t.Fatalf("writer: %v", err)
			}
			return
		default:
		}
		got, err := s.Get(x.ID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if len(got.Labels) != 1 {
			t.Fatalf("labels = %v, want exactly one gen label (torn snapshot?)", got.Labels)
		}
		labelGen := strings.TrimPrefix(got.Labels[0], "gen-")
		if metaGen := got.Metadata["gen"]; metaGen != labelGen {
			t.Fatalf("snapshot tear: label gen %q != metadata gen %q", labelGen, metaGen)
		}
	}
}

// --- helpers ---------------------------------------------------------------

func journalContainsID(beads []Bead, id string) bool {
	for _, b := range beads {
		if b.ID == id {
			return true
		}
	}
	return false
}

func journalIDsOf(beads []Bead) []string {
	ids := make([]string, len(beads))
	for i, b := range beads {
		ids[i] = b.ID
	}
	return ids
}

func journalTitlesOf(beads []Bead) []string {
	titles := make([]string, len(beads))
	for i, b := range beads {
		titles[i] = b.Title
	}
	return titles
}

func journalHasLabelIn(labels []string, want string) bool {
	for _, l := range labels {
		if l == want {
			return true
		}
	}
	return false
}
