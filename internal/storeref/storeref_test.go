package storeref

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func openPrefixed(t *testing.T, prefix string) beads.Store {
	t.Helper()
	s, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix(prefix))
	if err != nil {
		t.Fatalf("OpenSQLiteStore(prefix=%q): %v", prefix, err)
	}
	t.Cleanup(func() {
		if c, ok := s.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore()
		}
	})
	return s
}

func TestPrefixOwner(t *testing.T) {
	graph := openPrefixed(t, "gcg")
	orders := openPrefixed(t, "gco")
	work := beads.NewMemStore() // no IDPrefix() — stands in for the bd work store
	stores := []beads.Store{work, graph, orders}

	cases := []struct {
		id   string
		want beads.Store
	}{
		{"gcg-1", graph},
		{"gco-42", orders},
		{"gc-7", nil}, // no store claims the work prefix here
		{"zz-9", nil}, // unknown prefix
		{"gcg", nil},  // missing the namespace separator
		{"orphan", nil},
	}
	for _, tc := range cases {
		if got := PrefixOwner(tc.id, stores); got != tc.want {
			t.Errorf("PrefixOwner(%q) = %p, want %p", tc.id, got, tc.want)
		}
	}

	// nil stores in the slice are skipped, not panicked on.
	if got := PrefixOwner("gcg-1", []beads.Store{nil, graph}); got != graph {
		t.Errorf("PrefixOwner with a leading nil store = %p, want graph", got)
	}
}

func TestResolve_FederationFallback(t *testing.T) {
	graph := openPrefixed(t, "gcg")
	work := beads.NewMemStore()

	gb, err := graph.Create(beads.Bead{Title: "graph node"})
	if err != nil {
		t.Fatalf("seed graph: %v", err)
	}
	wb, err := work.Create(beads.Bead{Title: "work item"})
	if err != nil {
		t.Fatalf("seed work: %v", err)
	}
	stores := []beads.Store{work, graph}

	// Prefix-routed read.
	if got, err := Resolve(gb.ID, stores); err != nil || got.ID != gb.ID {
		t.Fatalf("Resolve(%q) = (%+v, %v), want the graph bead", gb.ID, got, err)
	}
	// Probe fallback: the work store has no IDPrefix, so its bead is found by probe.
	if got, err := Resolve(wb.ID, stores); err != nil || got.ID != wb.ID {
		t.Fatalf("Resolve(%q) = (%+v, %v), want the work bead via probe fallback", wb.ID, got, err)
	}
	// Absent everywhere.
	if _, err := Resolve("gcg-does-not-exist", stores); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("Resolve(absent) err = %v, want ErrNotFound", err)
	}
}
