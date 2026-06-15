package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// TestBeadCloseHandlerReachesSQLiteGraphBackend is the viability guarantee for
// routing the bd shim through the HTTP API under graph_store=sqlite: with the
// controller's city store a Router{work: MemStore, graph: SQLite}, a bead close
// routed through the HTTP handler lands on the SQLite graph backend (never the
// work backend). It proves the API server operates on the per-class Router and
// reaches the embedded graph store — so an HTTP `bd close <graph-id>` mutates the
// SQLite bead, the precondition for the pure-HTTP shim.
func TestBeadCloseHandlerReachesSQLiteGraphBackend(t *testing.T) {
	work := beads.NewMemStore() // mints gc-N work ids
	sqlite, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	router := coordrouter.New(work)
	router.Register(coordclass.ClassGraph, graph)

	// A graph-classified bead routes to SQLite (gcg-N), disjoint from work gc-N.
	gb, err := router.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	if _, err := graph.Get(gb.ID); err != nil {
		t.Fatalf("graph bead %s not in SQLite: %v", gb.ID, err)
	}

	state := newFakeState(t)
	state.cityBeadStore = router
	state.stores = nil // no rigs: beadStoresForID falls back to the city Router
	s := New(state)

	if _, err := s.humaHandleBeadClose(context.Background(), &BeadCloseInput{ID: gb.ID}); err != nil {
		t.Fatalf("humaHandleBeadClose(%s): %v", gb.ID, err)
	}

	got, err := graph.Get(gb.ID)
	if err != nil {
		t.Fatalf("re-get graph bead from SQLite: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("graph bead %s status = %q, want closed (the HTTP close did not reach SQLite)", gb.ID, got.Status)
	}
	if _, err := work.Get(gb.ID); err == nil {
		t.Fatalf("graph bead %s leaked into the work backend", gb.ID)
	}
}

// TestBeadReleaseIfCurrentHandlerReachesSQLiteGraphBackend proves the atomic
// compare-and-swap release endpoint operates on the SQLite graph backend via the
// Router: a mismatched expected-assignee is skipped (assignment intact), a match
// releases it — both reflected in the on-disk SQLite bead.
func TestBeadReleaseIfCurrentHandlerReachesSQLiteGraphBackend(t *testing.T) {
	work := beads.NewMemStore()
	sqlite, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	router := coordrouter.New(work)
	router.Register(coordclass.ClassGraph, graph)

	gb, err := router.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}
	// ReleaseIfCurrent only releases an in_progress assignment, so claim it first.
	assignee := "worker"
	inProgress := "in_progress"
	if err := router.Update(gb.ID, beads.UpdateOpts{Assignee: &assignee, Status: &inProgress}); err != nil {
		t.Fatalf("claim graph bead: %v", err)
	}

	state := newFakeState(t)
	state.cityBeadStore = router
	state.stores = nil
	s := New(state)

	// Mismatched expected assignee -> skipped; the SQLite assignment stays.
	skip := &BeadReleaseIfCurrentInput{ID: gb.ID}
	skip.Body.ExpectedAssignee = "someone-else"
	out, err := s.humaHandleBeadReleaseIfCurrent(context.Background(), skip)
	if err != nil {
		t.Fatalf("release-if-current (mismatch): %v", err)
	}
	if out.Body["status"] != "skipped" {
		t.Fatalf("mismatch status = %q, want skipped", out.Body["status"])
	}
	if got, _ := graph.Get(gb.ID); got.Assignee != "worker" {
		t.Fatalf("after skip, SQLite assignee = %q, want worker", got.Assignee)
	}

	// Matching expected assignee -> released; the SQLite assignment is cleared.
	rel := &BeadReleaseIfCurrentInput{ID: gb.ID}
	rel.Body.ExpectedAssignee = "worker"
	out, err = s.humaHandleBeadReleaseIfCurrent(context.Background(), rel)
	if err != nil {
		t.Fatalf("release-if-current (match): %v", err)
	}
	if out.Body["status"] != "released" {
		t.Fatalf("match status = %q, want released", out.Body["status"])
	}
	if got, _ := graph.Get(gb.ID); got.Assignee != "" {
		t.Fatalf("after release, SQLite assignee = %q, want cleared", got.Assignee)
	}
}

// TestBeadClaimHandlerReachesSQLiteGraphBackend proves the atomic claim endpoint
// operates on the SQLite graph backend via the Router: a graph-class bead is
// claimed for the explicit assignee in the on-disk SQLite store (the C6 fix so a
// worker's graph-step claim reaches SQLite rather than a work-only store).
func TestBeadClaimHandlerReachesSQLiteGraphBackend(t *testing.T) {
	work := beads.NewMemStore()
	sqlite, err := beads.OpenSQLiteStore(t.TempDir(), beads.WithSQLiteStoreIDPrefix("gcg"))
	if err != nil {
		t.Fatalf("OpenSQLiteStore: %v", err)
	}
	graph := sqlite.(*beads.SQLiteStore)
	t.Cleanup(func() { _ = graph.CloseStore() })

	router := coordrouter.New(work)
	router.Register(coordclass.ClassGraph, graph)
	gb, err := router.Create(beads.Bead{Title: "graph step", Type: "task", Labels: []string{"gc:wisp"}})
	if err != nil {
		t.Fatalf("create graph bead: %v", err)
	}

	state := newFakeState(t)
	state.cityBeadStore = router
	state.stores = nil
	s := New(state)

	in := &BeadClaimInput{ID: gb.ID}
	in.Body.Assignee = "worker"
	out, err := s.humaHandleBeadClaim(context.Background(), in)
	if err != nil {
		t.Fatalf("humaHandleBeadClaim: %v", err)
	}
	if !out.Body.Claimed || out.Body.Bead == nil || out.Body.Bead.Assignee != "worker" {
		t.Fatalf("claim result = %+v, want claimed for worker", out.Body)
	}
	got, err := graph.Get(gb.ID)
	if err != nil {
		t.Fatalf("re-get graph bead from SQLite: %v", err)
	}
	if got.Assignee != "worker" {
		t.Fatalf("SQLite assignee = %q, want worker (claim did not reach SQLite)", got.Assignee)
	}
}

// TestBeadReadyFederatesCityStore proves GET /v0/beads/ready surfaces city-scope
// ready work. The city store is not among the per-rig BeadStores(), so before the
// fix a single-HQ city's ready work (e.g. a graph.v2 molecule's actionable step)
// was invisible over HTTP — which would have broken a pure-HTTP worker's discovery.
func TestBeadReadyFederatesCityStore(t *testing.T) {
	cityStore := beads.NewMemStore()
	b, err := cityStore.Create(beads.Bead{Title: "city work", Type: "task"})
	if err != nil {
		t.Fatalf("create city bead: %v", err)
	}

	state := newFakeState(t)
	state.cityBeadStore = cityStore
	state.stores = nil // no rigs: ready must still surface the city store's work
	s := New(state)

	out, err := s.humaHandleBeadReady(context.Background(), &BeadReadyInput{})
	if err != nil {
		t.Fatalf("humaHandleBeadReady: %v", err)
	}
	found := false
	for _, item := range out.Body.Items {
		if item.ID == b.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("ready did not surface city-store bead %s (items=%d)", b.ID, len(out.Body.Items))
	}
}

// TestClientBeadWriteMethodsIssueExpectedRequests proves the new write-path client
// methods (the bd shim will call these) issue the correct HTTP verb, path, and
// body against the city-scoped endpoints.
func TestClientBeadWriteMethodsIssueExpectedRequests(t *testing.T) {
	var gotMethod, gotPath string
	var gotBody map[string]any
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod, gotPath = r.Method, r.URL.Path
		gotBody = nil
		_ = json.NewDecoder(r.Body).Decode(&gotBody) //nolint:errcheck
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"status": "ok"}) //nolint:errcheck
	}))
	defer ts.Close()
	c := NewCityScopedClient(ts.URL, "alpha")

	if err := c.CloseBead("gcg-1"); err != nil {
		t.Fatalf("CloseBead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-1/close" {
		t.Fatalf("CloseBead -> %s %s, want POST /v0/city/alpha/bead/gcg-1/close", gotMethod, gotPath)
	}

	if err := c.ReopenBead("gcg-1"); err != nil {
		t.Fatalf("ReopenBead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-1/reopen" {
		t.Fatalf("ReopenBead -> %s %s", gotMethod, gotPath)
	}

	if err := c.DeleteBead("gcg-1"); err != nil {
		t.Fatalf("DeleteBead: %v", err)
	}
	if gotMethod != http.MethodDelete || gotPath != "/v0/city/alpha/bead/gcg-1" {
		t.Fatalf("DeleteBead -> %s %s, want DELETE /v0/city/alpha/bead/gcg-1", gotMethod, gotPath)
	}

	pass := "closed"
	if err := c.UpdateBead("gcg-1", beads.UpdateOpts{Status: &pass, Metadata: map[string]string{"gc.outcome": "pass"}}); err != nil {
		t.Fatalf("UpdateBead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-1/update" {
		t.Fatalf("UpdateBead -> %s %s, want POST /v0/city/alpha/bead/gcg-1/update", gotMethod, gotPath)
	}
	if gotBody["status"] != "closed" {
		t.Fatalf("UpdateBead body status = %v, want closed", gotBody["status"])
	}
	if md, ok := gotBody["metadata"].(map[string]any); !ok || md["gc.outcome"] != "pass" {
		t.Fatalf("UpdateBead body metadata = %v, want gc.outcome=pass", gotBody["metadata"])
	}

	if _, err := c.ReadyBeads(); err != nil {
		t.Fatalf("ReadyBeads: %v", err)
	}
	if gotMethod != http.MethodGet || gotPath != "/v0/city/alpha/beads/ready" {
		t.Fatalf("ReadyBeads -> %s %s, want GET /v0/city/alpha/beads/ready", gotMethod, gotPath)
	}

	if _, err := c.ReleaseBeadIfCurrent("gcg-2", "worker"); err != nil {
		t.Fatalf("ReleaseBeadIfCurrent: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/release-if-current" {
		t.Fatalf("ReleaseBeadIfCurrent -> %s %s, want POST /v0/city/alpha/bead/gcg-2/release-if-current", gotMethod, gotPath)
	}
	if gotBody["expected_assignee"] != "worker" {
		t.Fatalf("ReleaseBeadIfCurrent body expected_assignee = %v, want worker", gotBody["expected_assignee"])
	}

	if _, _, err := c.ClaimBead("gcg-2", "worker"); err != nil {
		t.Fatalf("ClaimBead: %v", err)
	}
	if gotMethod != http.MethodPost || gotPath != "/v0/city/alpha/bead/gcg-2/claim" {
		t.Fatalf("ClaimBead -> %s %s, want POST /v0/city/alpha/bead/gcg-2/claim", gotMethod, gotPath)
	}
	if gotBody["assignee"] != "worker" {
		t.Fatalf("ClaimBead body assignee = %v, want worker", gotBody["assignee"])
	}
}
