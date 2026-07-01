package dashboardbff

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// runDetailRootEvent builds a graph.v2 run-root molecule with the scope metadata
// the detail snapshot projection requires (gc.scope_kind / gc.scope_ref /
// gc.root_store_ref).
func runDetailRootEvent(seq uint64, id, formula string) events.Event {
	return beadCreatedEvent(seq, beads.Bead{
		ID:        id,
		Title:     formula,
		Status:    "open",
		Type:      "molecule",
		Ref:       formula,
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 12, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          formula,
			"gc.run_target":       "rig:demo",
			"gc.root_store_ref":   "rig:demo",
			"gc.scope_kind":       "rig",
			"gc.scope_ref":        "demo",
		},
	})
}

// runDetailStepEvent builds a step bead parented to a run root.
func runDetailStepEvent(seq uint64, id, parent, stepID, status string) events.Event {
	return beadCreatedEvent(seq, beads.Bead{
		ID:        id,
		Title:     stepID,
		Status:    status,
		Type:      "task",
		ParentID:  parent,
		Ref:       "mol-adopt-pr-v2." + stepID,
		CreatedAt: time.Date(2026, 6, 1, 10, 1, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 6, 1, 10, 5, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.kind":         "step",
			"gc.root_bead_id": parent,
			"gc.step_id":      stepID,
			"gc.scope_ref":    "demo",
		},
	})
}

func beadCreatedEvent(seq uint64, b beads.Bead) events.Event {
	payload, _ := json.Marshal(struct {
		Bead beads.Bead `json:"bead"`
	}{b})
	return events.Event{Seq: seq, Type: events.BeadCreated, Payload: payload}
}

// fakeSupervisor serves the loopback endpoints the run-detail path reads: an
// empty /v0/city/{name}/sessions and a /v0/city/{name}/beads/graph/{rootID} that
// returns the authoritative run graph for graphByRoot[rootID] (mirroring
// internal/api.BeadGraphResponse: {root, beads, deps}). A rootID absent from the
// map 404s, which the BFF treats as "graph unreachable" → partial fallback.
func fakeSupervisor(t *testing.T, graphByRoot map[string][]beads.Bead) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if strings.HasSuffix(r.URL.Path, "/sessions") {
			_, _ = w.Write([]byte(`{"items":[],"total":0}`))
			return
		}
		const graphPrefix = "/beads/graph/"
		if idx := strings.Index(r.URL.Path, graphPrefix); idx >= 0 {
			rootID := r.URL.Path[idx+len(graphPrefix):]
			runBeads, ok := graphByRoot[rootID]
			if !ok || len(runBeads) == 0 {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"not found"}`))
				return
			}
			body, _ := json.Marshal(struct {
				Root  beads.Bead   `json:"root"`
				Beads []beads.Bead `json:"beads"`
				Deps  []any        `json:"deps"`
			}{Root: runBeads[0], Beads: runBeads, Deps: nil})
			_, _ = w.Write(body)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// graphStepBead builds a graph.v2 step bead as the beads/graph endpoint returns
// it: carrying gc.root_bead_id so snapshotForRun selects it as a run member.
func graphStepBead(id, root, stepID, status string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     stepID,
		Status:    status,
		Type:      "task",
		CreatedAt: time.Date(2026, 7, 1, 13, 1, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 1, 13, 5, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.kind":         "step",
			"gc.root_bead_id": root,
			"gc.step_id":      stepID,
		},
	}
}

// graphRootBead builds the graph.v2 run-root bead as beads/graph returns it: it
// carries gc.formula_contract=graph.v2 and gc.root_store_ref, but (like the live
// gcg-* root) NOT gc.scope_kind / gc.scope_ref — scope is derived from the store
// ref.
func graphRootBead(id, formula, storeRef string) beads.Bead {
	return beads.Bead{
		ID:        id,
		Title:     formula,
		Status:    "in_progress",
		CreatedAt: time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC),
		Metadata: map[string]string{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "workflow",
			"gc.formula":          formula,
			"gc.root_store_ref":   storeRef,
		},
	}
}

// runDetailWire is the decoded detail body — a structural contract check that the
// wire carries the FormulaRunDetail shape the SPA renderer reads.
type runDetailWire struct {
	RunID    string `json:"runId"`
	ScopeRef string `json:"scopeRef"`
	Title    string `json:"title"`
	Formula  struct {
		Kind string `json:"kind"`
		Name string `json:"name"`
	} `json:"formula"`
	Phase string `json:"phase"`
	Nodes []struct {
		ID string `json:"id"`
	} `json:"nodes"`
	Lanes []struct {
		ID string `json:"id"`
	} `json:"lanes"`
}

// TestRunDetailEndpoint drives the full endpoint: the warm fold projects one
// run's detail graph (root + step) off the same tailer the summary uses.
func TestRunDetailEndpoint(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath,
		runDetailRootEvent(1, "run1", "mol-adopt-pr-v2"),
		runDetailStepEvent(2, "run1.1", "run1", "preflight", "in_progress"),
	)

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	resp := getRunDetail(t, p, "alpha", "run1", http.StatusOK)
	if resp.RunID != "run1" {
		t.Errorf("runId = %q, want run1", resp.RunID)
	}
	if resp.ScopeRef != "demo" {
		t.Errorf("scopeRef = %q, want demo", resp.ScopeRef)
	}
	if resp.Title != "mol-adopt-pr-v2" {
		t.Errorf("title = %q, want mol-adopt-pr-v2", resp.Title)
	}
	if resp.Formula.Kind != "known" || resp.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("formula = %+v, want known/mol-adopt-pr-v2", resp.Formula)
	}
	if len(resp.Nodes) != 2 {
		t.Errorf("nodes = %d, want 2 (root + preflight)", len(resp.Nodes))
	}
	if len(resp.Lanes) != 1 || resp.Lanes[0].ID != "demo" {
		t.Errorf("lanes = %+v, want one lane 'demo'", resp.Lanes)
	}
	if resp.Phase == "" {
		t.Errorf("phase is empty, want a classified phase")
	}
}

// TestRunDetailEndpointUnknownCity404 confirms an unresolvable city 404s.
func TestRunDetailEndpointUnknownCity404(t *testing.T) {
	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{}}})
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/ghost/runs/run1/detail", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404 for unknown city", rec.Code)
	}
}

// TestRunDetailEndpointUnknownRun404 confirms a missing run 404s once the tailer
// is warm.
func TestRunDetailEndpointUnknownRun404(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	writeEventLog(t, logPath, runDetailRootEvent(1, "run1", "mol-adopt-pr-v2"))

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	// Warm the tailer first (a summary read blocks on the cold replay), so the
	// missing run is a true 404, not a warming 503.
	_ = getRunSummary(t, p, "alpha")
	getRunDetailExpectStatus(t, p, "alpha", "missing", http.StatusNotFound)
}

// TestRunDetailEndpointNonGraphV2DegradesToPartial pins the graceful-degradation
// contract: a non-graph.v2 (v1/wisp) run does NOT dead-end the page with a 422
// error body. It projects a best-effort 200 detail flagged partial with the
// not_graph_v2_run reason, so the run-detail page renders the run shell with
// whatever beads/phase it can derive.
func TestRunDetailEndpointNonGraphV2DegradesToPartial(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	// A molecule run marker but NO gc.formula_contract=graph.v2 and no scope.
	writeEventLog(t, logPath, beadCreatedEvent(1, beads.Bead{
		ID:        "v1run",
		Title:     "legacy v1 run",
		Status:    "open",
		Type:      "molecule",
		CreatedAt: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC),
		Metadata:  map[string]string{"gc.kind": "run"},
	}))

	p := New(Deps{Resolver: fakeResolver{paths: map[string]string{"alpha": dir}}})
	p.Start(t.Context())
	defer p.Stop()

	rec := getRunDetailRaw(t, p, "alpha", "v1run")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (partial, not a 422 dead-end); body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		RunID        string `json:"runId"`
		Completeness struct {
			Kind    string   `json:"kind"`
			Reasons []string `json:"reasons"`
		} `json:"completeness"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode detail body: %v; body=%s", err, rec.Body.String())
	}
	if body.RunID != "v1run" {
		t.Errorf("runId = %q, want v1run", body.RunID)
	}
	if body.Completeness.Kind != "partial" {
		t.Fatalf("completeness = %q, want partial; body=%s", body.Completeness.Kind, rec.Body.String())
	}
	if !containsStr(body.Completeness.Reasons, "not_graph_v2_run") {
		t.Errorf("reasons = %v, want to include not_graph_v2_run", body.Completeness.Reasons)
	}
}

func containsStr(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}

// sourceBead builds a source (mc-*) bead for a graph.v2 run whose gcg-* root
// never folded, carrying the run identity plus scope ONLY under
// pr_review.workflow_store=rig:<name> — the exact live shape that broke every
// run-detail page.
func sourceBead(id, root string, withWorkflowStore bool) beads.Bead {
	md := map[string]string{
		"pr_review.workflow_root_id": root,
		"pr_review.workflow_formula": "mol-adopt-pr-v2",
		"pr_review.workflow_status":  "running",
		"pr_review.rig":              "gascity",
	}
	if withWorkflowStore {
		md["pr_review.workflow_store"] = "rig:gascity"
	}
	return beads.Bead{
		ID:        id,
		Title:     "adopt pr source",
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Date(2026, 7, 1, 13, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 1, 15, 0, 0, 0, time.UTC),
		Metadata:  md,
	}
}

func sourceAttributedEvent(seq uint64, id, root string, withWorkflowStore bool) events.Event {
	return beadCreatedEvent(seq, sourceBead(id, root, withWorkflowStore))
}

// TestRunDetailEndpointResolvesScopeFromWorkflowStore pins the primary fix at the
// HTTP boundary: a source-attributed graph.v2 run whose only scope signal is
// pr_review.workflow_store=rig:gascity projects a complete 200 detail with the
// resolved rig/gascity scope — no 422.
func TestRunDetailEndpointResolvesScopeFromWorkflowStore(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	source := sourceBead("mc-ht7k7", "gcg-40700", true)
	writeEventLog(t, logPath, beadCreatedEvent(1, source))

	// The graph endpoint returns the run's authoritative bead set (here the same
	// source bead) so completeness stays complete rather than the graph-fetch
	// fallback partial.
	srv := fakeSupervisor(t, map[string][]beads.Bead{"gcg-40700": {source}})
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: srv.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	rec := getRunDetailRaw(t, p, "alpha", "gcg-40700")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		RunID        string `json:"runId"`
		ScopeKind    string `json:"scopeKind"`
		ScopeRef     string `json:"scopeRef"`
		RootStoreRef string `json:"rootStoreRef"`
		Completeness struct {
			Kind string `json:"kind"`
		} `json:"completeness"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if body.RunID != "gcg-40700" || body.ScopeKind != "rig" || body.ScopeRef != "gascity" {
		t.Errorf("got run=%s scope=%s/%s, want gcg-40700 rig/gascity", body.RunID, body.ScopeKind, body.ScopeRef)
	}
	if body.RootStoreRef != "rig:gascity" {
		t.Errorf("rootStoreRef = %q, want rig:gascity", body.RootStoreRef)
	}
	if body.Completeness.Kind != "complete" {
		t.Errorf("completeness = %q, want complete", body.Completeness.Kind)
	}
}

// TestRunDetailEndpointHonorsScopeQueryFallback pins the query-param fallback: a
// source-attributed run carrying NO store ref in metadata resolves scope from the
// ?scope_kind=&scope_ref= the summary lane threads in, and completes.
func TestRunDetailEndpointHonorsScopeQueryFallback(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	// withWorkflowStore=false → no gc.root_store_ref and no pr_review.workflow_store.
	source := sourceBead("mc-noscope", "gcg-noscope", false)
	writeEventLog(t, logPath, beadCreatedEvent(1, source))

	// Graph endpoint returns the scopeless run so membership is complete; scope
	// still can't resolve from metadata (only the query hint can).
	srv := fakeSupervisor(t, map[string][]beads.Bead{"gcg-noscope": {source}})
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: srv.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	// Warm the tailer, then read WITHOUT a hint → partial (scope unresolved).
	_ = getRunSummary(t, p, "alpha")
	recPartial := getRunDetailRaw(t, p, "alpha", "gcg-noscope")
	if recPartial.Code != http.StatusOK {
		t.Fatalf("no-hint status = %d, want 200; body=%s", recPartial.Code, recPartial.Body.String())
	}

	// With the hint the scope resolves and the detail completes.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/city/alpha/runs/gcg-noscope/detail?scope_kind=rig&scope_ref=gascity", nil)
	p.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("hinted status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		ScopeKind    string `json:"scopeKind"`
		ScopeRef     string `json:"scopeRef"`
		RootStoreRef string `json:"rootStoreRef"`
		Completeness struct {
			Kind string `json:"kind"`
		} `json:"completeness"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if body.ScopeKind != "rig" || body.ScopeRef != "gascity" {
		t.Errorf("hinted scope = %s/%s, want rig/gascity", body.ScopeKind, body.ScopeRef)
	}
	if body.RootStoreRef != "rig:gascity" {
		t.Errorf("hinted rootStoreRef = %q, want rig:gascity (derived from hint)", body.RootStoreRef)
	}
	if body.Completeness.Kind != "complete" {
		t.Errorf("hinted completeness = %q, want complete", body.Completeness.Kind)
	}
}

// TestRunDetailEndpointProjectsFullGraphNotJustFold is the acceptance test for
// this fix: the event fold sees only the run root (1 bead), but the beads/graph
// endpoint serves the full step set. The detail must project the graph's steps —
// its node count must reflect the graph beads, NOT the 1-bead fold.
func TestRunDetailEndpointProjectsFullGraphNotJustFold(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")

	// The EVENT FOLD sees only the root (the graph.v2 steps never reach the log).
	root := graphRootBead("gcg-big", "mol-adopt-pr-v2", "rig:gascity")
	writeEventLog(t, logPath, beadCreatedEvent(1, root))

	// The GRAPH endpoint returns the root plus many distinct steps.
	stepKinds := []string{"scope-check", "spec", "retry", "ralph", "cleanup", "workflow-finalize"}
	graph := []beads.Bead{root}
	for i, kind := range stepKinds {
		id := "gcg-big-step-" + strconv.Itoa(i)
		graph = append(graph, graphStepBead(id, "gcg-big", kind, "open"))
	}
	if len(graph) != len(stepKinds)+1 {
		t.Fatalf("graph fixture wrong size: %d", len(graph))
	}

	srv := fakeSupervisor(t, map[string][]beads.Bead{"gcg-big": graph})
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: srv.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	// Warm the tailer and confirm the fold alone would show only the root.
	_ = getRunSummary(t, p, "alpha")

	rec := getRunDetailRaw(t, p, "alpha", "gcg-big")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		RunID string `json:"runId"`
		Nodes []struct {
			ID string `json:"id"`
		} `json:"nodes"`
		Completeness struct {
			Kind string `json:"kind"`
		} `json:"completeness"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	// The graph has root + 6 steps. The fold had only the root (1 node). The
	// detail must reflect the graph's steps, so node count must exceed the fold's.
	if len(body.Nodes) <= 1 {
		t.Fatalf("nodes = %d, want > 1 (the graph steps, not just the folded root); body=%s",
			len(body.Nodes), rec.Body.String())
	}
	if len(body.Nodes) < len(stepKinds) {
		t.Errorf("nodes = %d, want >= %d (one per graph step)", len(body.Nodes), len(stepKinds))
	}
	if body.Completeness.Kind != "complete" {
		t.Errorf("completeness = %q, want complete (graph fetch succeeded)", body.Completeness.Kind)
	}
}

// TestRunDetailEndpointGraphFetchFailureDegradesPartial pins the fallback: when
// the beads/graph endpoint is unreachable, the detail falls back to the
// (incomplete) event fold and is marked partial with graph_fetch_failed — it does
// NOT report complete on a truncated view.
func TestRunDetailEndpointGraphFetchFailureDegradesPartial(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, ".gc", "events.jsonl")
	root := graphRootBead("gcg-partial", "mol-adopt-pr-v2", "rig:gascity")
	writeEventLog(t, logPath, beadCreatedEvent(1, root))

	// Supervisor serves NO graph for gcg-partial (empty map → 404), so the fetch
	// fails and the detail falls back to the event fold.
	srv := fakeSupervisor(t, map[string][]beads.Bead{})
	p := New(Deps{
		Resolver:          fakeResolver{paths: map[string]string{"alpha": dir}},
		SupervisorBaseURL: srv.URL,
	})
	p.Start(t.Context())
	defer p.Stop()

	_ = getRunSummary(t, p, "alpha")
	rec := getRunDetailRaw(t, p, "alpha", "gcg-partial")
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200; body=%s", rec.Code, rec.Body.String())
	}
	var body struct {
		Completeness struct {
			Kind    string   `json:"kind"`
			Reasons []string `json:"reasons"`
		} `json:"completeness"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v; body=%s", err, rec.Body.String())
	}
	if body.Completeness.Kind != "partial" {
		t.Fatalf("completeness = %q, want partial (graph fetch failed); body=%s", body.Completeness.Kind, rec.Body.String())
	}
	if !containsStr(body.Completeness.Reasons, "graph_fetch_failed") {
		t.Errorf("reasons = %v, want to include graph_fetch_failed", body.Completeness.Reasons)
	}
}

func getRunDetailRaw(t *testing.T, p *Plane, city, runID string) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	p.Handler().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/city/"+city+"/runs/"+runID+"/detail", nil))
	return rec
}

func getRunDetailExpectStatus(t *testing.T, p *Plane, city, runID string, want int) {
	t.Helper()
	rec := getRunDetailRaw(t, p, city, runID)
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
	}
}

func getRunDetail(t *testing.T, p *Plane, city, runID string, want int) runDetailWire {
	t.Helper()
	rec := getRunDetailRaw(t, p, city, runID)
	if rec.Code != want {
		t.Fatalf("status = %d, want %d; body=%s", rec.Code, want, rec.Body.String())
	}
	var resp runDetailWire
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response: %v; body=%s", err, rec.Body.String())
	}
	return resp
}
