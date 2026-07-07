package beads_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/beadstest"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// newJournalConformanceFactory returns a Store factory that opens a fresh,
// empty JournalStore over a temp graphstore for every call. The graphstore is
// closed when the enclosing test (and all its subtests) complete.
func newJournalConformanceFactory(t *testing.T) func() beads.Store {
	t.Helper()
	return func() beads.Store {
		path := filepath.Join(t.TempDir(), "journal.db")
		gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "conformance-city"})
		if err != nil {
			t.Fatalf("open graphstore: %v", err)
		}
		t.Cleanup(func() { _ = gs.Close() })
		return beads.NewJournalStore(gs)
	}
}

// TestJournalStoreConformance runs the shared beads.Store conformance suite
// against a bare JournalStore. Every RunStoreTests subtest is a real
// cross-backend parity contract and passes with zero journal-specific skips.
//
// RunMetadataTests is included: the journal store preserves empty-string
// metadata values (it stores the empty value in node_metadata rather than
// deleting the key), so absent-vs-empty semantics hold exactly as they do for
// MemStore/FileStore.
//
// CONFORMANCE EXCEPTION — RunDepTests is deliberately NOT enrolled. That
// auxiliary suite (enrolled only by MemStore and FileStore) calls
// DepAdd("a","b",...) and DepRemove("x","y") against beads that were never
// created, i.e. it pins those stores' laxity of recording edges for a
// non-existent issue. JournalStore's DepAdd/DepRemove
// intentionally route through journalGuardMutable, which rejects a missing
// issue with ErrNotFound and a fold-owned Tier-A row with
// ErrFoldOwnedWriteClosed (the fold-ownership half is load-bearing and pinned
// by TestJournalStoreFoldOwnedGuardsDepAndReopen). Matching the lax suite would
// require dropping that existence guard, trading a loud, correct failure for a
// silent dangling-edge write on the graph substrate. The journal store's own
// dependency behavior is fully covered by TestJournalStoreDepAddListRemove and
// the fold-guard test. RunDepTests is not part of the canonical RunStoreTests
// gate (neither NativeDoltStore nor BdStore enroll it), so declining it is not a
// skip of any shared-gate subtest.
func TestJournalStoreConformance(t *testing.T) {
	factory := newJournalConformanceFactory(t)
	beadstest.RunStoreTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
}

// TestJournalStoreConformanceCachingWrapped runs the same conformance suite
// against a JournalStore wrapped in a CachingStore, proving the cache layer
// preserves the journal backend's parity behavior. See TestJournalStoreConformance
// for why RunDepTests is not enrolled.
func TestJournalStoreConformanceCachingWrapped(t *testing.T) {
	backing := newJournalConformanceFactory(t)
	factory := func() beads.Store {
		return beads.NewCachingStoreForTest(backing(), nil)
	}
	beadstest.RunStoreTests(t, factory)
	beadstest.RunMetadataTests(t, factory)
}
