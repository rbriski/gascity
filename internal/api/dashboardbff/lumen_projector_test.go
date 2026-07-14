package dashboardbff

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runproj"
)

// fakeLumenProj is a controllable LumenRunProjector for the seam tests.
type fakeLumenProj struct {
	lanes    []runproj.RunLane
	detail   runproj.FormulaRunDetail
	isLumen  bool
	summErr  error
	detErr   error
	gotCity  string
	gotRoot  string
	gotRunID string
}

func (f *fakeLumenProj) SummaryLanes(_ context.Context, cityName, cityRoot string, _ []beads.Bead) ([]runproj.RunLane, error) {
	f.gotCity, f.gotRoot = cityName, cityRoot
	return f.lanes, f.summErr
}

func (f *fakeLumenProj) Detail(_ context.Context, cityName, cityRoot, runID string, _ []beads.Bead) (runproj.FormulaRunDetail, bool, error) {
	f.gotCity, f.gotRoot, f.gotRunID = cityName, cityRoot, runID
	return f.detail, f.isLumen, f.detErr
}

func tailerWith(proj LumenRunProjector) *cityRunTailer {
	return &cityRunTailer{
		name: "c1",
		mgr: &runTailerManager{
			deps: Deps{
				Resolver:  fakeResolver{paths: map[string]string{"c1": "/city/c1"}},
				LumenRuns: proj,
			},
		},
	}
}

// TestMergeLumenLanesNilSeamIsNoop proves an upstream build (no LumenRuns seam)
// leaves the summary byte-identical.
func TestMergeLumenLanesNilSeamIsNoop(t *testing.T) {
	base := runproj.RunSummary{TotalActive: 1, Lanes: []runproj.RunLane{{ID: "bead-lane", Phase: "implementation"}}}
	tl := tailerWith(nil)
	got := tl.mergeLumenLanes(context.Background(), base, nil)
	if got.TotalActive != 1 || len(got.Lanes) != 1 || got.Lanes[0].ID != "bead-lane" {
		t.Fatalf("nil seam mutated the summary: %+v", got)
	}
}

// TestMergeLumenLanesBucketsByPhase proves an active + a complete Lumen lane land
// in the right buckets with the counts bumped.
func TestMergeLumenLanesBucketsByPhase(t *testing.T) {
	proj := &fakeLumenProj{lanes: []runproj.RunLane{
		{ID: "gcg-active", Phase: "implementation"},
		{ID: "gcg-done", Phase: "complete"},
	}}
	base := runproj.RunSummary{
		TotalActive: 1,
		Lanes:       []runproj.RunLane{{ID: "bead-lane", Phase: "review"}},
	}
	tl := tailerWith(proj)
	got := tl.mergeLumenLanes(context.Background(), base, nil)

	if proj.gotCity != "c1" || proj.gotRoot != "/city/c1" {
		t.Fatalf("projector got city/root = %q/%q, want c1//city/c1", proj.gotCity, proj.gotRoot)
	}
	// Active + blocked lane membership survives (EnrichRunSummary recomputes the
	// active/blocked counts + RunCounts from these slices, so merge only appends).
	if len(got.Lanes) != 2 || got.Lanes[1].ID != "gcg-active" {
		t.Fatalf("active Lumen lane not appended: %+v", laneIDsOf(got.Lanes))
	}
	// TotalHistorical + HistoricalLanes are the ONLY counts merge must set (enrich
	// leaves them untouched).
	if got.TotalHistorical != 1 || len(got.HistoricalLanes) != 1 || got.HistoricalLanes[0].ID != "gcg-done" {
		t.Fatalf("complete Lumen lane not in historical: %+v", got)
	}
}

// TestMergeLumenLanesCopyOnWrite proves the merge never mutates the cached
// summary's backing arrays (the P1 race): the input slices are unchanged and the
// output uses fresh backing.
func TestMergeLumenLanesCopyOnWrite(t *testing.T) {
	proj := &fakeLumenProj{lanes: []runproj.RunLane{{ID: "gcg-active", Phase: "implementation"}}}
	// A base whose Lanes has spare capacity (len 1, cap 4) — the aliasing hazard.
	backing := make([]runproj.RunLane, 1, 4)
	backing[0] = runproj.RunLane{ID: "bead-lane", Phase: "implementation"}
	base := runproj.RunSummary{Lanes: backing}

	got := tailerWith(proj).mergeLumenLanes(context.Background(), base, nil)

	if len(backing) != 1 || backing[0].ID != "bead-lane" {
		t.Fatalf("merge mutated the caller's backing array: %+v", backing)
	}
	// Writing into the returned slice must not touch the original backing[1] slot.
	if len(got.Lanes) >= 2 {
		got.Lanes[1].ID = "MUTATED"
	}
	if cap(backing) >= 2 && backing[:2][1].ID == "MUTATED" {
		t.Fatal("merge appended into the shared backing array (data race with the cache)")
	}
}

// TestEnrichedSummaryMergesLumenBeforeEnrich pins the design-critical ordering:
// mergeLumenLanes runs BEFORE EnrichRunSummary, so the Lumen active lane is
// counted by enrich's recomputed TotalActive/census. Moving the merge after
// enrich (the mutant) would leave TotalActive at 1.
func TestEnrichedSummaryMergesLumenBeforeEnrich(t *testing.T) {
	proj := &fakeLumenProj{lanes: []runproj.RunLane{{ID: "gcg-active", Phase: "implementation"}}}
	// newRunTailerManager initializes the caches; empty SupervisorBaseURL makes
	// sessions unavailable (so no lane is stale-demoted).
	mgr := newRunTailerManager(Deps{
		Resolver:  fakeResolver{paths: map[string]string{"c1": "/city/c1"}},
		LumenRuns: proj,
	})
	tl := &cityRunTailer{
		name:    "c1",
		mgr:     mgr,
		readyCh: make(chan struct{}),
		ready:   true,
		summary: runproj.RunSummary{
			TotalActive: 1,
			Lanes:       []runproj.RunLane{{ID: "bead-lane", Phase: "implementation"}},
		},
	}
	close(tl.readyCh) // avoid the 5s cold-load wait

	enriched := tl.enrichedSummary(context.Background())
	if enriched.TotalActive != 2 {
		t.Fatalf("enriched.TotalActive = %d, want 2 — the Lumen lane must be merged BEFORE enrich (else enrich's recount omits it)", enriched.TotalActive)
	}
	if _, ok := laneByID(enriched.Lanes, "gcg-active"); !ok {
		t.Fatalf("Lumen lane absent from enriched lanes: %v", laneIDsOf(enriched.Lanes))
	}
}

// TestDetailRouteTriesLumenBeforeWarming503 pins the design-critical ordering in
// registerRunDetail: a run whose root is absent from the bead fold is offered to
// the Lumen projector BEFORE the not-ready warming 503, since the journal is
// readable regardless of events-fold warmth. A never-started (not-ready) tailer
// would otherwise 503; with the Lumen seam it serves 200. (The 5s cold-load wait
// is the price of exercising the genuinely not-ready path.)
func TestDetailRouteTriesLumenBeforeWarming503(t *testing.T) {
	proj := &fakeLumenProj{isLumen: true, detail: validDetail(t)}
	p := New(Deps{
		Resolver:  fakeResolver{paths: map[string]string{"c1": t.TempDir()}},
		LumenRuns: proj,
	})
	// Do NOT call Start — the tailer stays never-ready, the exact state whose
	// warming-503 the Lumen try must precede.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/city/c1/runs/gcg-x/detail", nil)
	p.mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("detail route status = %d, want 200 (Lumen must be tried before the warming 503); body=%s", rec.Code, rec.Body.String())
	}
	if proj.gotRunID != "gcg-x" {
		t.Fatalf("Lumen projector got runID %q, want gcg-x", proj.gotRunID)
	}
}

func laneByID(lanes []runproj.RunLane, id string) (runproj.RunLane, bool) {
	for _, l := range lanes {
		if l.ID == id {
			return l, true
		}
	}
	return runproj.RunLane{}, false
}

// TestMergeLumenLanesErrorIsBestEffort proves a Lumen fold failure leaves the
// bead lanes intact.
func TestMergeLumenLanesErrorIsBestEffort(t *testing.T) {
	proj := &fakeLumenProj{summErr: context.DeadlineExceeded}
	base := runproj.RunSummary{TotalActive: 1, Lanes: []runproj.RunLane{{ID: "bead-lane"}}}
	got := tailerWith(proj).mergeLumenLanes(context.Background(), base, nil)
	if got.TotalActive != 1 || len(got.Lanes) != 1 {
		t.Fatalf("a Lumen error disturbed the bead lanes: %+v", got)
	}
}

// TestLumenDetailSeam proves the detail seam: nil → miss; isLumen=false → miss;
// isLumen=true → marshaled bytes.
func TestLumenDetailSeam(t *testing.T) {
	if _, ok := tailerWith(nil).lumenDetail(context.Background(), "gcg-x"); ok {
		t.Fatal("nil seam reported a Lumen detail")
	}

	miss := &fakeLumenProj{isLumen: false}
	if _, ok := tailerWith(miss).lumenDetail(context.Background(), "gcg-x"); ok {
		t.Fatal("isLumen=false reported a Lumen detail")
	}

	hit := &fakeLumenProj{isLumen: true, detail: validDetail(t)}
	body, ok := tailerWith(hit).lumenDetail(context.Background(), "gcg-x")
	if !ok {
		t.Fatal("isLumen=true did not report a Lumen detail")
	}
	if hit.gotRunID != "gcg-x" {
		t.Fatalf("projector got runID %q, want gcg-x", hit.gotRunID)
	}
	if len(body) == 0 {
		t.Fatal("empty detail body")
	}
}

// validDetail builds a real, marshalable FormulaRunDetail from a minimal
// graph.v2 run so the seam test exercises the true marshal path (a hand-built
// zero-value detail intentionally fails marshal — its union discriminators are
// empty).
func validDetail(t *testing.T) runproj.FormulaRunDetail {
	t.Helper()
	beadList := []beads.Bead{{
		ID: "gcg-x", Type: "task", Status: "open", Title: "run",
		Metadata: beads.StringMap{
			"gc.formula_contract": "graph.v2",
			"gc.kind":             "run",
			"gc.formula":          "f.lumen",
			"gc.scope_kind":       "city",
			"gc.scope_ref":        "c1",
			"gc.root_store_ref":   "city:c1",
		},
	}}
	d, _, _, _, _, _, err := runproj.BuildRunDetailForRun(
		beadList, "gcg-x", 1, 0, nil, nil, runproj.FormulaDetailUpstreamError)
	if err != nil {
		t.Fatalf("build valid detail: %v", err)
	}
	return d
}
