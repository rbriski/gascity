package storeref

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
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

// TestResolve_MatchesRouterGet pins storeref.Resolve to the behavior of the
// coordrouter.Router.Get it replaces, over the exact same backend set. When the
// Router is deleted, this differential test is deleted with it; storeref.go is
// untouched.
func TestResolve_MatchesRouterGet(t *testing.T) {
	// assertParity checks Resolve(id, stores) against the Router's Get for the
	// exact same backend set, on bead identity AND error identity (both the
	// nil/non-nil split and whether the error wraps ErrNotFound).
	assertParity := func(t *testing.T, stores []beads.Store, get func(string) (beads.Bead, error), ids []string) {
		t.Helper()
		for _, id := range ids {
			wantBead, wantErr := get(id)
			gotBead, gotErr := Resolve(id, stores)
			if gotBead.ID != wantBead.ID {
				t.Errorf("Resolve(%q).ID = %q, Router.Get = %q", id, gotBead.ID, wantBead.ID)
			}
			if (gotErr == nil) != (wantErr == nil) {
				t.Errorf("Resolve(%q) err = %v, Router.Get err = %v", id, gotErr, wantErr)
			}
			if errors.Is(gotErr, beads.ErrNotFound) != errors.Is(wantErr, beads.ErrNotFound) {
				t.Errorf("Resolve(%q) ErrNotFound=%v, Router.Get ErrNotFound=%v",
					id, errors.Is(gotErr, beads.ErrNotFound), errors.Is(wantErr, beads.ErrNotFound))
			}
		}
	}

	t.Run("multi-backend", func(t *testing.T) {
		work := beads.NewMemStore()
		graph := openPrefixed(t, "gcg")
		orders := openPrefixed(t, "gco")
		r := coordrouter.New(work)
		r.Register(coordclass.ClassGraph, graph)
		r.Register(coordclass.ClassOrders, orders)

		gb, _ := graph.Create(beads.Bead{Title: "g"})
		ob, _ := orders.Create(beads.Bead{Title: "o"})
		wb, _ := work.Create(beads.Bead{Title: "w"})

		assertParity(t, r.Backends(), r.Get, []string{gb.ID, ob.ID, wb.ID, "gcg-absent", "zz-9", ""})
	})

	// soleBackend: Router.Get short-circuits to b.Get(id) for a single backend,
	// while Resolve always runs PrefixOwner+probe. Pin that they still agree
	// (the path differs, the result/error must not).
	t.Run("sole-backend", func(t *testing.T) {
		work := beads.NewMemStore()
		r := coordrouter.New(work)
		wb, _ := work.Create(beads.Bead{Title: "only"})
		assertParity(t, r.Backends(), r.Get, []string{wb.ID, "absent-1", ""})
	})

	// A prefixed sole backend: the prefix owner IS the only store, exercising the
	// owner-hit-then-no-fallback path against the Router's sole short-circuit.
	t.Run("sole-prefixed-backend", func(t *testing.T) {
		graph := openPrefixed(t, "gcg")
		r := coordrouter.New(graph)
		gb, _ := graph.Create(beads.Bead{Title: "g"})
		assertParity(t, r.Backends(), r.Get, []string{gb.ID, "gcg-absent", "zz-1"})
	})
}
