package beads

import (
	"slices"
	"testing"
)

func TestDiagSliceDiffKind(t *testing.T) {
	cases := []struct {
		old, fresh []string
		want       string
	}{
		{nil, nil, ""},
		{[]string{"a", "b"}, []string{"a", "b"}, ""},
		{[]string{"a", "b"}, []string{"b", "a"}, "order-only"},
		{[]string{"a", "b"}, []string{"a", "c"}, "set"},
		{[]string{"a"}, []string{"a", "b"}, "set"},
	}
	for _, c := range cases {
		if got := diagSliceDiffKind(c.old, c.fresh); got != c.want {
			t.Errorf("diagSliceDiffKind(%v,%v)=%q want %q", c.old, c.fresh, got, c.want)
		}
	}
}

func TestDiagMetadataDiffKind(t *testing.T) {
	if k := diagMetadataDiffKind(map[string]string{"a": "1"}, map[string]string{"a": "1"}); k != "" {
		t.Errorf("equal metadata: got %q want empty", k)
	}
	if k := diagMetadataDiffKind(map[string]string{"a": "1"}, map[string]string{"a": "2"}); k != "metadata-value:a" {
		t.Errorf("value change: got %q want metadata-value:a", k)
	}
	if k := diagMetadataDiffKind(map[string]string{"a": "1"}, map[string]string{"b": "1"}); k != "metadata-keys" {
		t.Errorf("key change: got %q want metadata-keys", k)
	}
}

func TestReconcileUpdateDiffFieldsClassifiesOrderVsReal(t *testing.T) {
	base := Bead{ID: "x", Status: "open", Labels: []string{"a", "b"}, Needs: []string{"n1"}}
	// pure label reorder => order-only false-positive
	reordered := base
	reordered.Labels = []string{"b", "a"}
	if got := reconcileUpdateDiffFields(base, reordered); !slices.Contains(got, "labels-order-only") {
		t.Errorf("label reorder: got %v, want labels-order-only", got)
	}
	// real status change
	changed := base
	changed.Status = "closed"
	if got := reconcileUpdateDiffFields(base, changed); !slices.Contains(got, "status") {
		t.Errorf("status change: got %v, want status", got)
	}
}

func TestReconcileEvictDiagEnvGate(t *testing.T) {
	if reconcileEvictDiag() {
		t.Fatal("reconcileEvictDiag must be off by default")
	}
	t.Setenv("GC_RECONCILE_EVICT_DIAG", "1")
	if !reconcileEvictDiag() {
		t.Fatal("reconcileEvictDiag must be on when GC_RECONCILE_EVICT_DIAG=1")
	}
}

// The diagnostic must not change reconcile behavior: recoverMissingFromList
// recovers an alive cached bead and leaves a genuinely-gone one to be evicted,
// identically whether the diag env is set or not.
func TestReconcileEvictDiagIsDiagnosticOnly(t *testing.T) {
	backing := NewMemStore()
	alive, err := backing.Create(Bead{Type: "task", Status: "open"})
	if err != nil {
		t.Fatalf("create alive: %v", err)
	}

	run := func() map[string]Bead {
		cs := NewCachingStoreForTest(backing, nil)
		cs.mu.Lock()
		cs.beads = map[string]Bead{
			alive.ID:  {ID: alive.ID, Status: "open"},
			"gone-id": {ID: "gone-id", Status: "open"}, // in cache, absent from backing
		}
		cs.mu.Unlock()
		fresh := map[string]Bead{}
		cs.recoverMissingFromList(fresh)
		return fresh
	}

	assertOutcome := func(label string, fresh map[string]Bead) {
		if _, ok := fresh[alive.ID]; !ok {
			t.Errorf("%s: alive bead should be recovered into freshByID", label)
		}
		if _, ok := fresh["gone-id"]; ok {
			t.Errorf("%s: gone bead (ErrNotFound) must NOT be recovered", label)
		}
	}

	assertOutcome("diag-off", run())
	t.Setenv("GC_RECONCILE_EVICT_DIAG", "1")
	assertOutcome("diag-on", run())
}
