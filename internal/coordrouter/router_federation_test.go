package coordrouter

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
)

func TestRouterFederatesReadsAcrossBackends(t *testing.T) {
	work := beads.NewMemStore()
	// Offset the graph store's id sequence so the two MemStores occupy distinct id
	// namespaces (as real bd vs sqlite backends do); otherwise both mint "bd-1"
	// and a by-id read collides across backends.
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	gb, err := r.Create(beads.Bead{Title: "graph node", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph: %v", err)
	}
	if wb.ID == gb.ID {
		t.Fatalf("test setup: id namespaces collided (%s)", wb.ID)
	}

	// Each bead physically lands only in its owning backend.
	if _, err := work.Get(wb.ID); err != nil {
		t.Fatalf("work bead not in the work backend: %v", err)
	}
	if _, err := graph.Get(gb.ID); err != nil {
		t.Fatalf("graph bead not in the graph backend: %v", err)
	}
	if _, err := graph.Get(wb.ID); err == nil {
		t.Fatal("work bead leaked into the graph backend")
	}

	// Federated Get finds a bead in whichever backend owns it.
	if _, err := r.Get(wb.ID); err != nil {
		t.Fatalf("federated Get(work): %v", err)
	}
	if _, err := r.Get(gb.ID); err != nil {
		t.Fatalf("federated Get(graph): %v", err)
	}
	if _, err := r.Get("does-not-exist"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("federated Get(missing) = %v, want ErrNotFound", err)
	}

	// Federated List and Ready union both backends.
	assertUnions := func(name string, beadsOut []beads.Bead) {
		t.Helper()
		ids := make(map[string]bool, len(beadsOut))
		for _, b := range beadsOut {
			ids[b.ID] = true
		}
		if !ids[wb.ID] || !ids[gb.ID] {
			t.Fatalf("%s did not union both backends: have %v, want %s + %s", name, ids, wb.ID, gb.ID)
		}
	}
	listed, err := r.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	assertUnions("List", listed)

	ready, err := r.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	assertUnions("Ready", ready)
}

// TestRouterSingleBackendReadsDelegate confirms the identity-phase fast path: with
// one backend, federated reads delegate directly (byte-identical to that backend).
func TestRouterSingleBackendReadsDelegate(t *testing.T) {
	mem := beads.NewMemStore()
	r := New(mem)
	created, err := r.Create(beads.Bead{Title: "x", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	got, err := r.Get(created.ID)
	if err != nil || got.ID != created.ID {
		t.Fatalf("single-backend Get = (%q, %v), want %q", got.ID, err, created.ID)
	}
	if _, ok := r.soleBackend(); !ok {
		t.Fatal("expected a sole backend in the identity phase")
	}
}

// TestRouterReadyGraphOnlyExcludesWorkBackend proves the worker/dispatcher
// execution-readiness surface: under graph_store=sqlite (a distinct ClassGraph
// backend), ReadyGraphOnly returns ONLY the graph backend's ready set and never
// the Dolt ClassWork primary, while the full federated Ready still unions both
// for the human/diagnostic backlog view.
func TestRouterReadyGraphOnlyExcludesWorkBackend(t *testing.T) {
	work := beads.NewMemStore()
	graph := beads.NewMemStoreFrom(1000, nil, nil)
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	wb, err := r.Create(beads.Bead{Title: "work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	gb, err := r.Create(beads.Bead{Title: "graph node", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph: %v", err)
	}

	graphOnly, err := r.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	ids := make(map[string]bool, len(graphOnly))
	for _, b := range graphOnly {
		ids[b.ID] = true
	}
	if !ids[gb.ID] {
		t.Fatalf("ReadyGraphOnly missing graph bead %s: have %v", gb.ID, ids)
	}
	if ids[wb.ID] {
		t.Fatalf("ReadyGraphOnly leaked the Dolt work bead %s into the worker readiness hot loop", wb.ID)
	}

	// The full federation contract is unchanged: Ready still unions both backends.
	full, err := r.Ready()
	if err != nil {
		t.Fatalf("Ready: %v", err)
	}
	fids := make(map[string]bool, len(full))
	for _, b := range full {
		fids[b.ID] = true
	}
	if !fids[wb.ID] || !fids[gb.ID] {
		t.Fatalf("Ready must still union both backends: have %v", fids)
	}
}

// TestRouterReadyGraphOnlyIdentityPhaseFallsBack proves the default (non-sqlite)
// city stays byte-identical: with no distinct ClassGraph backend, ReadyGraphOnly
// falls back to the sole backend's ready set (the work store), so a Dolt-only
// city's readiness is unchanged.
func TestRouterReadyGraphOnlyIdentityPhaseFallsBack(t *testing.T) {
	mem := beads.NewMemStore()
	r := New(mem)
	created, err := r.Create(beads.Bead{Title: "x", Type: "task"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	ready, err := r.ReadyGraphOnly()
	if err != nil {
		t.Fatalf("ReadyGraphOnly: %v", err)
	}
	found := false
	for _, b := range ready {
		if b.ID == created.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("identity-phase ReadyGraphOnly must return the sole backend's bead %s: have %v", created.ID, ready)
	}
}

// errBoom is the sentinel leg failure used by the Group D partial-result tests.
var errBoom = errors.New("boom: leg unavailable")

// failingReadStore embeds a MemStore and fails every federated read method with
// a hard (non-partial) error. Used to simulate a locked/down Router leg.
type failingReadStore struct {
	*beads.MemStore
	err error
}

func (s *failingReadStore) List(beads.ListQuery) ([]beads.Bead, error) { return nil, s.err }
func (s *failingReadStore) ListOpen(...string) ([]beads.Bead, error)   { return nil, s.err }
func (s *failingReadStore) Children(string, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s *failingReadStore) ListByLabel(string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}

func (s *failingReadStore) ListByAssignee(string, string, int) ([]beads.Bead, error) {
	return nil, s.err
}

func (s *failingReadStore) ListByMetadata(map[string]string, int, ...beads.QueryOpt) ([]beads.Bead, error) {
	return nil, s.err
}
func (s *failingReadStore) Ready(...beads.ReadyQuery) ([]beads.Bead, error) { return nil, s.err }
func (s *failingReadStore) DepList(string, string) ([]beads.Dep, error)     { return nil, s.err }

// partialReadStore returns its MemStore rows alongside a PartialResultError, the
// contract shape a CachingStore work leg forwards from a bd parse partial.
type partialReadStore struct {
	*beads.MemStore
	err error
}

func (s *partialReadStore) List(q beads.ListQuery) ([]beads.Bead, error) {
	rows, err := s.MemStore.List(q)
	if err != nil {
		return rows, err
	}
	return rows, s.err
}

// TestRouterFederatedReadReturnsPartialWhenOneLegFails proves a failing leg no
// longer launders into a nil error: the survivor's rows come back WITH a
// PartialResultError so completeness-sensitive callers can tell degraded from
// complete.
func TestRouterFederatedReadReturnsPartialWhenOneLegFails(t *testing.T) {
	newRouter := func() (*Router, beads.Bead) {
		work := beads.NewMemStore()
		wb, err := work.Create(beads.Bead{Title: "work item", Type: "task", Status: "open", Assignee: "me", Labels: []string{"lbl"}, Metadata: map[string]string{"k": "v"}})
		if err != nil {
			t.Fatalf("create work: %v", err)
		}
		graph := &failingReadStore{MemStore: beads.NewMemStoreFrom(1000, nil, nil), err: errBoom}
		r := New(work)
		r.Register(coordclass.ClassGraph, graph)
		return r, wb
	}

	cases := []struct {
		name string
		call func(*Router) ([]beads.Bead, error)
	}{
		{"List", func(r *Router) ([]beads.Bead, error) { return r.List(beads.ListQuery{AllowScan: true}) }},
		{"ListOpen", func(r *Router) ([]beads.Bead, error) { return r.ListOpen() }},
		{"ListByLabel", func(r *Router) ([]beads.Bead, error) { return r.ListByLabel("lbl", 0) }},
		{"ListByAssignee", func(r *Router) ([]beads.Bead, error) { return r.ListByAssignee("me", "", 0) }},
		{"ListByMetadata", func(r *Router) ([]beads.Bead, error) { return r.ListByMetadata(map[string]string{"k": "v"}, 0) }},
		{"Ready", func(r *Router) ([]beads.Bead, error) { return r.Ready() }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, wb := newRouter()
			rows, err := tc.call(r)
			if err == nil {
				t.Fatalf("%s: err = nil, want a partial-result error when a leg fails", tc.name)
			}
			if !beads.IsPartialResult(err) {
				t.Fatalf("%s: err = %v, want IsPartialResult", tc.name, err)
			}
			if !errors.Is(err, errBoom) {
				t.Fatalf("%s: err = %v, want Unwrap chain to reach errBoom", tc.name, err)
			}
			found := false
			for _, b := range rows {
				if b.ID == wb.ID {
					found = true
				}
			}
			if !found {
				t.Fatalf("%s: survivor rows = %v, want work bead %s retained", tc.name, rows, wb.ID)
			}
		})
	}
}

// TestRouterDepListReturnsPartialWhenOneLegFails is the DepList analog.
func TestRouterDepListReturnsPartialWhenOneLegFails(t *testing.T) {
	work := beads.NewMemStore()
	a, err := work.Create(beads.Bead{Title: "a", Type: "task"})
	if err != nil {
		t.Fatalf("create a: %v", err)
	}
	b, err := work.Create(beads.Bead{Title: "b", Type: "task"})
	if err != nil {
		t.Fatalf("create b: %v", err)
	}
	if err := work.DepAdd(a.ID, b.ID, "blocks"); err != nil {
		t.Fatalf("dep add: %v", err)
	}
	graph := &failingReadStore{MemStore: beads.NewMemStoreFrom(1000, nil, nil), err: errBoom}
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	deps, err := r.DepList(a.ID, "down")
	if err == nil || !beads.IsPartialResult(err) || !errors.Is(err, errBoom) {
		t.Fatalf("DepList err = %v, want partial-result wrapping errBoom", err)
	}
	if len(deps) == 0 {
		t.Fatalf("DepList deps = %v, want the surviving work-leg edge retained", deps)
	}
}

// TestRouterFederatedReadAllLegsFailedHardFails pins the preserved hard-fail
// path: when every leg fails with no rows, the bare leg error propagates and is
// NOT a partial.
func TestRouterFederatedReadAllLegsFailedHardFails(t *testing.T) {
	work := &failingReadStore{MemStore: beads.NewMemStore(), err: errBoom}
	graph := &failingReadStore{MemStore: beads.NewMemStoreFrom(1000, nil, nil), err: errBoom}
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	rows, err := r.List(beads.ListQuery{AllowScan: true})
	if rows != nil {
		t.Fatalf("rows = %v, want nil on total failure", rows)
	}
	if !errors.Is(err, errBoom) {
		t.Fatalf("err = %v, want errBoom", err)
	}
	if beads.IsPartialResult(err) {
		t.Fatalf("err = %v, want a HARD failure, not a partial, when all legs fail", err)
	}
}

// TestRouterFederatedReadMergesRowsFromPartialLeg proves a leg that itself
// returned (rows, PartialResultError) contributes its rows to the union rather
// than being dropped, and the union is still reported partial.
func TestRouterFederatedReadMergesRowsFromPartialLeg(t *testing.T) {
	work := beads.NewMemStore()
	wb, err := work.Create(beads.Bead{Title: "work", Type: "task"})
	if err != nil {
		t.Fatalf("create work: %v", err)
	}
	graphMem := beads.NewMemStoreFrom(1000, nil, nil)
	gb, err := graphMem.Create(beads.Bead{Title: "graph", Type: "task"})
	if err != nil {
		t.Fatalf("create graph: %v", err)
	}
	graph := &partialReadStore{MemStore: graphMem, err: &beads.PartialResultError{Op: "fake leg", Err: errBoom}}
	r := New(work)
	r.Register(coordclass.ClassGraph, graph)

	rows, err := r.List(beads.ListQuery{AllowScan: true})
	if err == nil || !beads.IsPartialResult(err) {
		t.Fatalf("err = %v, want IsPartialResult", err)
	}
	ids := map[string]bool{}
	for _, b := range rows {
		ids[b.ID] = true
	}
	if !ids[wb.ID] || !ids[gb.ID] {
		t.Fatalf("rows = %v, want BOTH the healthy work row %s and the partial leg's row %s", rows, wb.ID, gb.ID)
	}
}

// TestRouterSingleBackendReadErrorPassesThroughUnwrapped pins the identity-phase
// byte-identical guarantee: a sole backend's error is returned verbatim, never
// re-wrapped as a PartialResultError.
func TestRouterSingleBackendReadErrorPassesThroughUnwrapped(t *testing.T) {
	r := New(&failingReadStore{MemStore: beads.NewMemStore(), err: errBoom})
	rows, err := r.List(beads.ListQuery{AllowScan: true})
	if rows != nil {
		t.Fatalf("rows = %v, want nil", rows)
	}
	if err != errBoom {
		t.Fatalf("err = %v, want the bare errBoom (no re-wrap on the sole-backend fast path)", err)
	}
	if beads.IsPartialResult(err) {
		t.Fatalf("err = %v, want NOT a partial on the identity fast path", err)
	}
}
