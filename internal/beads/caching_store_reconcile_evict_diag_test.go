package beads

import (
	"testing"
)

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
