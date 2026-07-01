package runproj

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// workflowStoreScopedFixture reproduces the EXACT live deploy shape that broke
// every run-detail page: a graph.v2 mol-adopt-pr-v2 run whose gcg-* root lives
// only in the SQLite graph_store (absent from the fold), where the source (mc-*)
// beads that DID fold carry the store ref ONLY under pr_review.workflow_store
// (format "rig:<name>") — NOT gc.root_store_ref / gc.scope_kind / gc.scope_ref.
// This is the key the previous fix missed, so the detail hard-errored with
// "run scope is missing or invalid".
const workflowStoreRoot = "gcg-adopt-pr-ws01"

func workflowStoreScopedFixture() []beads.Bead {
	ts := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return t
	}
	meta := map[string]string{
		"pr_review.workflow_root_id": workflowStoreRoot,
		"pr_review.workflow_formula": "mol-adopt-pr-v2",
		"pr_review.workflow_status":  "running",
		"pr_review.workflow_store":   "rig:gascity",
		"pr_review.rig":              "gascity",
		"pr_review.pr_number":        "3829",
		"pr_review.github_title":     "workflow-store-scoped run",
	}
	return []beads.Bead{
		{
			ID:        "mc-ht7k7",
			Title:     "adopt pr source",
			Status:    "open",
			Type:      "task",
			CreatedAt: ts("2026-07-01T13:00:00Z"),
			UpdatedAt: ts("2026-07-01T15:00:00Z"),
			Assignee:  "polecat-3",
			Metadata:  meta,
		},
	}
}

// TestDetailResolvesScopeFromWorkflowStore pins the primary fix: a run whose
// only scope signal is pr_review.workflow_store=rig:gascity resolves scope in the
// DETAIL path identically to the summary, and projects without a hard error.
func TestDetailResolvesScopeFromWorkflowStore(t *testing.T) {
	fixture := workflowStoreScopedFixture()

	// Summary and detail must agree — this is the inconsistency the bug was.
	summary := BuildRunSummary(fixture)
	if len(summary.Lanes) != 1 {
		t.Fatalf("len(Lanes) = %d, want 1", len(summary.Lanes))
	}
	laneScope := summary.Lanes[0].Scope
	if laneScope.Status != "available" || laneScope.Kind != "rig" || laneScope.Ref != "gascity" {
		t.Fatalf("summary scope = %+v, want available rig/gascity", laneScope)
	}

	detail, err := BuildRunDetail(fixture, workflowStoreRoot, 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	if detail.ScopeKind != "rig" || detail.ScopeRef != "gascity" {
		t.Errorf("detail scope = %s/%s, want rig/gascity", detail.ScopeKind, detail.ScopeRef)
	}
	if detail.RootStoreRef != "rig:gascity" || detail.ResolvedRootStore != "rig:gascity" {
		t.Errorf("detail store = %q/%q, want rig:gascity", detail.RootStoreRef, detail.ResolvedRootStore)
	}
	if detail.Completeness.Kind != "complete" {
		t.Errorf("completeness = %+v, want complete (scope resolved)", detail.Completeness)
	}
	if detail.Formula.Kind != "known" || detail.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("formula = %+v, want known/mol-adopt-pr-v2", detail.Formula)
	}
}

// TestDetailResolvesScopeFromQueryHint pins the fallback: a source-attributed run
// carrying NO store ref at all in metadata still projects with a complete scope
// when the endpoint threads the summary lane's ?scope_kind=&scope_ref= as a hint.
func TestDetailResolvesScopeFromQueryHint(t *testing.T) {
	fixture := workflowStoreScopedFixture()
	// Strip the workflow-store key so NO metadata scope resolves; only the hint can.
	for i := range fixture {
		delete(fixture[i].Metadata, "pr_review.workflow_store")
	}

	// Without a hint the run projects a partial (scope unresolved) — not an error.
	partial, err := BuildRunDetail(fixture, workflowStoreRoot, 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail (no hint): %v", err)
	}
	if partial.Completeness.Kind != "partial" {
		t.Errorf("no-hint completeness = %+v, want partial", partial.Completeness)
	}
	if !containsReason(partial.Completeness.Reasons, "run_scope_unresolved") {
		t.Errorf("no-hint reasons = %v, want run_scope_unresolved", partial.Completeness.Reasons)
	}

	// With the hint the scope resolves and the detail completes.
	hint := RunDetailScopeHint{ScopeKind: "rig", ScopeRef: "gascity"}
	detail, err := BuildRunDetailWithOptions(fixture, workflowStoreRoot, 1, 100, nil, hint)
	if err != nil {
		t.Fatalf("BuildRunDetailWithOptions (hint): %v", err)
	}
	if detail.ScopeKind != "rig" || detail.ScopeRef != "gascity" {
		t.Errorf("hinted scope = %s/%s, want rig/gascity", detail.ScopeKind, detail.ScopeRef)
	}
	if detail.RootStoreRef != "rig:gascity" {
		t.Errorf("hinted store = %q, want rig:gascity (derived from scope pair)", detail.RootStoreRef)
	}
	if detail.Completeness.Kind != "complete" {
		t.Errorf("hinted completeness = %+v, want complete", detail.Completeness)
	}
}

// TestScopelessRunDegradesToPartial pins graceful degradation: a v1/wisp molecule
// that folded as a real root but is NOT graph.v2 and carries no scope must render
// a best-effort partial detail, NOT a hard not_run_view / invalid_snapshot error.
func TestScopelessRunDegradesToPartial(t *testing.T) {
	ts, _ := time.Parse(time.RFC3339, "2026-07-01T12:00:00Z")
	fixture := []beads.Bead{
		{
			ID:        "ga-scopeless",
			Title:     "wendy patrol",
			Status:    "open",
			Type:      "molecule",
			CreatedAt: ts,
			UpdatedAt: ts,
			Metadata: map[string]string{
				"gc.formula_name": "mol-wendy-patrol",
			},
		},
		{
			ID:        "ga-scopeless.step1",
			Title:     "patrol step",
			Status:    "open",
			Type:      "step",
			ParentID:  "ga-scopeless",
			CreatedAt: ts,
			UpdatedAt: ts,
			Metadata:  map[string]string{"gc.step_ref": "patrol.check"},
		},
	}

	detail, err := BuildRunDetail(fixture, "ga-scopeless", 1, 100)
	if err != nil {
		t.Fatalf("scopeless run must degrade, not error: %v", err)
	}
	if detail.RunID != "ga-scopeless" {
		t.Errorf("RunID = %q, want ga-scopeless", detail.RunID)
	}
	if detail.Completeness.Kind != "partial" {
		t.Fatalf("completeness = %+v, want partial", detail.Completeness)
	}
	for _, want := range []string{"not_graph_v2_run", "run_store_ref_unresolved", "run_scope_unresolved"} {
		if !containsReason(detail.Completeness.Reasons, want) {
			t.Errorf("reasons %v missing %q", detail.Completeness.Reasons, want)
		}
	}
}

// TestUnknownRunStill404s pins the one genuine hard case: a run id with NO members
// in the fold has nothing to project and must surface the not-found error.
func TestUnknownRunStill404s(t *testing.T) {
	_, err := BuildRunDetail(workflowStoreScopedFixture(), "gcg-does-not-exist", 1, 100)
	if err == nil {
		t.Fatal("unknown run id must error (no members to project)")
	}
	var unsupported *UnsupportedRunError
	if asUnsupported(err, &unsupported) {
		t.Errorf("unknown run should be a plain not-found, not UnsupportedRunError: %v", err)
	}
}

func containsReason(reasons []string, want string) bool {
	for _, r := range reasons {
		if r == want {
			return true
		}
	}
	return false
}

func asUnsupported(err error, target **UnsupportedRunError) bool {
	u, ok := err.(*UnsupportedRunError)
	if ok {
		*target = u
	}
	return ok
}
