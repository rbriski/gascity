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
}
