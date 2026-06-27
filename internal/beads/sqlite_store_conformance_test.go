package beads_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordclass/coordtest"
)

// newSQLiteForConformance returns a fresh, empty SQLite store with cleanup
// registered. The factory closures below hand it to the shared conformance suites
// as both a beads.GraphApplyStore (it implements ApplyGraphPlan) and a
// beads.Store.
func newSQLiteForConformance(t *testing.T) *beads.SQLiteStore {
	t.Helper()
	s, err := beads.OpenSQLiteStore(t.TempDir())
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	store := s.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = store.CloseStore() })
	return store
}

// TestSQLiteStoreSatisfiesGraphStoreConformance runs the SHARED GraphStore
// conformance suite (the same coordtest.RunGraphStoreTests every graph backend
// must pass) against the recovered SQLite store, un-skipped — proving its
// ApplyGraphPlan seam is conformant, not just its white-box behavior.
func TestSQLiteStoreSatisfiesGraphStoreConformance(t *testing.T) {
	coordtest.RunGraphStoreTestsWithOptions(t,
		func() beads.GraphApplyStore { return newSQLiteForConformance(t) },
		coordtest.Options{Skip: false})
}

// TestSQLiteStoreSatisfiesClassedStoreConformanceForGraph runs the shared
// classed-store conformance suite for ClassGraph against the SQLite store,
// un-skipped — proving it round-trips and correctly classifies graph-class beads.
func TestSQLiteStoreSatisfiesClassedStoreConformanceForGraph(t *testing.T) {
	coordtest.RunClassedStoreTestsWithOptions(t, coordclass.ClassGraph,
		func() beads.Store { return newSQLiteForConformance(t) },
		coordtest.Options{Skip: false})
}

// TestSQLiteStoreSatisfiesClassedStoreConformanceForSessions runs the shared
// classed-store conformance suite for ClassSessions against the SQLite store,
// un-skipped — proving the embedded session-class backend round-trips and
// correctly classifies session-class beads, the storage requirement for relocating
// sessions onto the Router's gcs backend.
func TestSQLiteStoreSatisfiesClassedStoreConformanceForSessions(t *testing.T) {
	coordtest.RunClassedStoreTestsWithOptions(t, coordclass.ClassSessions,
		func() beads.Store { return newSQLiteForConformance(t) },
		coordtest.Options{Skip: false})
}

// TestSQLiteStoreRoundTripsSessionWaitBeads proves the session backend round-trips
// BOTH bead shapes that classify to ClassSessions: the session lifecycle bead
// (type=session / gc:session) and the durable session-wait bead (type=gate /
// gc:wait). The generic conformance suite exercises the representative session
// bead; this pins the wait bead explicitly because waits relocate WITH the session
// class and the controller's wait dispatch reads them from the same store.
func TestSQLiteStoreRoundTripsSessionWaitBeads(t *testing.T) {
	store := newSQLiteForConformance(t)
	for _, b := range []beads.Bead{
		{Title: "session", Type: "session", Labels: []string{"gc:session"}},
		{Title: "wait", Type: "gate", Labels: []string{"gc:wait"}},
	} {
		created, err := store.Create(b)
		if err != nil {
			t.Fatalf("Create(%s): %v", b.Title, err)
		}
		got, err := store.Get(created.ID)
		if err != nil {
			t.Fatalf("Get(%s): %v", created.ID, err)
		}
		if got.Type != b.Type {
			t.Fatalf("round-trip %s: Type = %q, want %q", b.Title, got.Type, b.Type)
		}
		if coordclass.Classify(got) != coordclass.ClassSessions {
			t.Fatalf("Classify(%s) = %v, want ClassSessions", b.Title, coordclass.Classify(got))
		}
	}
}
