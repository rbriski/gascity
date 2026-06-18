package beads

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

// liveMaxSuffix returns the largest numeric suffix among all persisted beads
// whose ID carries the store's prefix. It reads from disk, not from the
// in-memory seq, so tests can assert a freshly minted ID outran the real max.
func liveMaxSuffix(t *testing.T, s *SQLiteStore) int {
	t.Helper()
	rows, err := s.readDB.QueryContext(context.Background(), `SELECT id FROM beads WHERE id LIKE ?`, s.prefix+"-%")
	if err != nil {
		t.Fatalf("liveMaxSuffix query: %v", err)
	}
	defer rows.Close() //nolint:errcheck
	max := 0
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("liveMaxSuffix scan: %v", err)
		}
		if n := numericIDSuffix(id); n > max {
			max = n
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("liveMaxSuffix rows: %v", err)
	}
	return max
}

// TestSQLiteCreateSelfHealsStaleSeq reproduces the production race: another
// process mints higher-numbered beads on the shared DB while this process's
// in-memory seq falls behind the on-disk max. nextID() would re-issue an
// already-occupied suffix and Create would fail with "duplicate id". With the
// allocator self-heal, an auto-id Create must succeed and return an ID whose
// suffix exceeds the live max — i.e. a genuinely free ID.
func TestSQLiteCreateSelfHealsStaleSeq(t *testing.T) {
	s := openTestSQLiteStore(t)

	// Simulate beads minted by other processes: pin high IDs directly so the
	// rows exist on disk at suffixes 100..105.
	for n := 100; n <= 105; n++ {
		if _, err := s.Create(Bead{ID: pf(s, n), Title: "external", Type: "task"}); err != nil {
			t.Fatalf("seed external bead %d: %v", n, err)
		}
	}

	// Drive this process's in-memory seq BELOW the on-disk max, exactly as a
	// concurrent process that never observed 100..105 would be, AND make the
	// very next minted suffix collide so the reseed path fires. With seq=99 the
	// next nextID() yields gc-100 — already taken — forcing a one-shot reseed
	// that lifts the floor past the on-disk max.
	s.seq.Store(99)

	maxBefore := liveMaxSuffix(t, s)
	created, err := s.Create(Bead{Title: "fresh work", Type: "task"})
	if err != nil {
		t.Fatalf("auto-id Create with stale seq must self-heal, got error: %v", err)
	}

	suffix := numericIDSuffix(created.ID)
	if suffix <= maxBefore {
		t.Fatalf("minted ID %q (suffix %d) did not outrun pre-create live max %d — reused an occupied suffix",
			created.ID, suffix, maxBefore)
	}

	// The bead actually persisted and is retrievable & unique.
	got, err := s.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%q) after self-heal Create: %v", created.ID, err)
	}
	if got.Title != "fresh work" {
		t.Fatalf("persisted bead = %+v, want title 'fresh work'", got)
	}
}

// TestSQLiteCreatePinnedDuplicateStillFails locks in the NDI contract: a
// caller-PINNED id that already exists must STILL hard-fail with "duplicate
// id". Self-heal applies only to store-generated ids; pinned ids drive
// resume/crash-adoption and must never be silently remapped.
func TestSQLiteCreatePinnedDuplicateStillFails(t *testing.T) {
	s := openTestSQLiteStore(t)

	pinned := pf(s, 100)
	if _, err := s.Create(Bead{ID: pinned, Title: "first", Type: "task"}); err != nil {
		t.Fatalf("first pinned Create: %v", err)
	}

	_, err := s.Create(Bead{ID: pinned, Title: "second", Type: "task"})
	if err == nil {
		t.Fatalf("re-creating pinned id %q must fail, got nil", pinned)
	}
	if !strings.Contains(err.Error(), "duplicate id") {
		t.Fatalf("pinned duplicate error = %v, want 'duplicate id'", err)
	}
}

// TestSQLiteGraphApplySelfHealsStaleSeq forces Pass-1 to mint an ID that is
// already occupied on disk (stale seq), then asserts ApplyGraphPlanWithStorage
// still SUCCEEDS: every node lands on a free, unique ID and edges/parents
// resolve to the (possibly remapped) ids.
func TestSQLiteGraphApplySelfHealsStaleSeq(t *testing.T) {
	s := openTestSQLiteStore(t)

	// Occupy the exact suffixes Pass-1 would mint next from a stale floor.
	for n := 100; n <= 102; n++ {
		if _, err := s.Create(Bead{ID: pf(s, n), Title: "external", Type: "task"}); err != nil {
			t.Fatalf("seed external bead %d: %v", n, err)
		}
	}
	// Stale floor: next auto IDs would be prefix-100, prefix-101 — both taken.
	s.seq.Store(99)

	plan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "root", Title: "root", Type: "task"},
			{Key: "step", Title: "step", Type: "task", ParentKey: "root"},
		},
		Edges: []GraphApplyEdge{
			{FromKey: "step", ToKey: "root", Type: "blocks"},
		},
	}
	res, err := s.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err != nil {
		t.Fatalf("graph apply with stale seq must self-heal, got error: %v", err)
	}
	if err := ValidateGraphApplyResult(plan, res); err != nil {
		t.Fatalf("result must resolve every node key: %v", err)
	}

	rootID, stepID := res.IDs["root"], res.IDs["step"]
	if rootID == stepID {
		t.Fatalf("root and step collapsed onto the same id %q", rootID)
	}
	// Neither node may reuse an occupied external suffix.
	for _, id := range []string{rootID, stepID} {
		if n := numericIDSuffix(id); n >= 100 && n <= 102 {
			t.Fatalf("node id %q reused an occupied external suffix", id)
		}
	}

	// Both nodes persist; parent rides parent_id and resolves to the remapped root.
	if _, err := s.Get(rootID); err != nil {
		t.Fatalf("Get(root %q): %v", rootID, err)
	}
	step, err := s.Get(stepID)
	if err != nil {
		t.Fatalf("Get(step %q): %v", stepID, err)
	}
	if step.ParentID != rootID {
		t.Fatalf("step.ParentID = %q, want remapped root %q", step.ParentID, rootID)
	}
	// Edge resolves to the remapped root via DepList.
	deps, err := s.DepList(stepID, "down")
	if err != nil {
		t.Fatalf("DepList(step): %v", err)
	}
	if len(deps) != 1 || deps[0].DependsOnID != rootID || deps[0].Type != "blocks" {
		t.Fatalf("DepList(step) = %+v, want one blocks->%s", deps, rootID)
	}
}

// TestSQLiteGraphApplyMetadataRefSurvivesRemap guards the MetadataRefs fixup:
// when a referenced node is remapped to a fresh id, the referring node's
// metadata value must follow the remap, not point at the now-stolen old id.
func TestSQLiteGraphApplyMetadataRefSurvivesRemap(t *testing.T) {
	s := openTestSQLiteStore(t)
	for n := 100; n <= 102; n++ {
		if _, err := s.Create(Bead{ID: pf(s, n), Title: "external", Type: "task"}); err != nil {
			t.Fatalf("seed external bead %d: %v", n, err)
		}
	}
	s.seq.Store(99)

	plan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "target", Title: "target", Type: "task"},
			{Key: "ref", Title: "ref", Type: "task", MetadataRefs: map[string]string{"points_at": "target"}},
		},
	}
	res, err := s.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err != nil {
		t.Fatalf("graph apply: %v", err)
	}
	targetID := res.IDs["target"]
	refBead, err := s.Get(res.IDs["ref"])
	if err != nil {
		t.Fatalf("Get(ref): %v", err)
	}
	if refBead.Metadata["points_at"] != targetID {
		t.Fatalf("metadata ref = %q, want remapped target id %q", refBead.Metadata["points_at"], targetID)
	}
}

// TestSQLiteGraphApplyIntraBatchDistinct exercises the shared seen-map: two
// nodes that would each mint the same fresh id within a single, not-yet-
// committed plan must end up with two distinct ids.
func TestSQLiteGraphApplyIntraBatchDistinct(t *testing.T) {
	s := openTestSQLiteStore(t)

	// Occupy the next two suffixes so Pass-1 mints collide, forcing a re-mint
	// path that the seen-map must keep distinct between the two nodes.
	for n := 100; n <= 101; n++ {
		if _, err := s.Create(Bead{ID: pf(s, n), Title: "external", Type: "task"}); err != nil {
			t.Fatalf("seed external bead %d: %v", n, err)
		}
	}
	s.seq.Store(99)

	plan := &GraphApplyPlan{
		Nodes: []GraphApplyNode{
			{Key: "a", Title: "a", Type: "task"},
			{Key: "b", Title: "b", Type: "task"},
		},
	}
	res, err := s.ApplyGraphPlanWithStorage(context.Background(), plan, StorageDefault)
	if err != nil {
		t.Fatalf("graph apply: %v", err)
	}
	if res.IDs["a"] == res.IDs["b"] {
		t.Fatalf("intra-batch nodes minted the same id %q", res.IDs["a"])
	}
	if _, err := s.Get(res.IDs["a"]); err != nil {
		t.Fatalf("Get(a): %v", err)
	}
	if _, err := s.Get(res.IDs["b"]); err != nil {
		t.Fatalf("Get(b): %v", err)
	}
}

// pf formats a prefixed bead id like "gc-100" for the store under test.
func pf(s *SQLiteStore, n int) string {
	return s.prefix + "-" + strconv.Itoa(n)
}
