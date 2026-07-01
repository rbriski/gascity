package runproj

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

// sourceAttributedFixture reproduces the LIVE deploy shape: a graph.v2
// mol-adopt-pr-v2 run whose gcg-* run-root bead lives only in the SQLite
// graph_store and was NEVER emitted as a bead event, so it is absent from the
// fold. The only beads that folded are the source (mc-*) beads, which carry the
// run identity via pr_review.workflow_root_id / workflow_formula / workflow_status
// plus the scope/store-ref metadata a graph-resident bead is routed with.
//
// It also includes a plain task group with NO pr_review.* metadata, to prove the
// source-attribution loosening does NOT widen grouping to ordinary work.
const gcgRoot = "gcg-adopt-pr-9f2a"

func sourceAttributedFixture() []beads.Bead {
	ts := func(s string) time.Time {
		t, err := time.Parse(time.RFC3339, s)
		if err != nil {
			panic(err)
		}
		return t
	}
	scopeMeta := func(extra map[string]string) map[string]string {
		m := map[string]string{
			"gc.scope_kind":              "rig",
			"gc.scope_ref":               "gascity-packs",
			"gc.root_store_ref":          "rig:gascity-packs",
			"pr_review.workflow_root_id": gcgRoot,
			"pr_review.workflow_formula": "mol-adopt-pr-v2",
			"pr_review.workflow_status":  "running",
			"pr_review.pr_number":        "512",
			"pr_review.pr_url":           "https://github.com/gastownhall/gascity/pull/512",
			"pr_review.github_title":     "source-attributed run",
		}
		for k, v := range extra {
			m[k] = v
		}
		return m
	}

	return []beads.Bead{
		// Source review-loop step — in progress, carries the run identity + a
		// gc.step_id so the structured phase resolves to the review stage.
		{
			ID:        "mc-review-1",
			Title:     "review loop",
			Status:    "in_progress",
			Type:      "task",
			CreatedAt: ts("2026-06-30T10:00:00Z"),
			UpdatedAt: ts("2026-06-30T12:00:00Z"),
			Assignee:  "polecat-7",
			Metadata: scopeMeta(map[string]string{
				"gc.kind":    "step",
				"gc.step_id": "review-loop",
			}),
		},
		// A second source step, closed, same run.
		{
			ID:        "mc-preflight-1",
			Title:     "preflight",
			Status:    "closed",
			Type:      "task",
			CreatedAt: ts("2026-06-30T10:00:00Z"),
			UpdatedAt: ts("2026-06-30T10:05:00Z"),
			Metadata: scopeMeta(map[string]string{
				"gc.kind":    "step",
				"gc.step_id": "preflight",
			}),
		},
		// An unrelated plain task group with NO pr_review.* metadata: it must NOT
		// appear as a run lane (grouping stays tight).
		{
			ID:        "plain-1",
			Title:     "just a task",
			Status:    "in_progress",
			Type:      "task",
			CreatedAt: ts("2026-06-30T09:00:00Z"),
			UpdatedAt: ts("2026-06-30T09:30:00Z"),
			Assignee:  "worker-2",
		},
	}
}

// TestSourceAttributedRunAppearsAsActivePrReviewLane pins the core fix: a
// graph.v2 run whose gcg-* root never folded is still surfaced as an active
// prReview lane, keyed on the workflow-root pointer its source beads carry.
func TestSourceAttributedRunAppearsAsActivePrReviewLane(t *testing.T) {
	summary := BuildRunSummary(sourceAttributedFixture())

	if summary.TotalActive != 1 {
		t.Fatalf("TotalActive = %d, want 1 (the source-attributed run)", summary.TotalActive)
	}
	if len(summary.Lanes) != 1 {
		t.Fatalf("len(Lanes) = %d, want 1", len(summary.Lanes))
	}
	lane := summary.Lanes[0]
	if lane.ID != gcgRoot {
		t.Errorf("lane.ID = %q, want %q (the workflow-root pointer)", lane.ID, gcgRoot)
	}
	if lane.Formula.Status != "known" || lane.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("lane.Formula = %+v, want known/mol-adopt-pr-v2", lane.Formula)
	}
	if summary.RunCounts.PrReview != 1 {
		t.Errorf("RunCounts.PrReview = %d, want 1", summary.RunCounts.PrReview)
	}
	// Scope must resolve from the source-bead metadata (not unavailable).
	if lane.Scope.Status != "available" || lane.Scope.Kind != "rig" || lane.Scope.Ref != "gascity-packs" {
		t.Errorf("lane.Scope = %+v, want available rig/gascity-packs", lane.Scope)
	}
}

// TestPlainTaskGroupIsNotARun proves the loosening did not widen grouping: a
// bead with no run marker and no pr_review.* pointer stays out of the run view.
func TestPlainTaskGroupIsNotARun(t *testing.T) {
	summary := BuildRunSummary(sourceAttributedFixture())
	for _, lane := range summary.Lanes {
		if lane.ID == "plain-1" {
			t.Fatalf("plain task 'plain-1' must not appear as a run lane")
		}
	}
	if summary.RunCounts.Other != 0 {
		t.Errorf("RunCounts.Other = %d, want 0 (no non-run groups leak in)", summary.RunCounts.Other)
	}
}

// TestSourceAttributedWorkflowStatusMapsTerminalPhases pins Step 2: the
// authoritative pr_review.workflow_status drives terminal/blocked phases when the
// gcg-* root never folded.
func TestSourceAttributedWorkflowStatusMapsTerminalPhases(t *testing.T) {
	setStatus := func(beadsIn []beads.Bead, status string) []beads.Bead {
		for i := range beadsIn {
			if beadsIn[i].Metadata != nil {
				if _, ok := beadsIn[i].Metadata["pr_review.workflow_status"]; ok {
					beadsIn[i].Metadata["pr_review.workflow_status"] = status
				}
			}
		}
		return beadsIn
	}

	t.Run("merged marks the run complete", func(t *testing.T) {
		summary := BuildRunSummary(setStatus(sourceAttributedFixture(), "merged"))
		if summary.TotalActive != 0 {
			t.Errorf("TotalActive = %d, want 0 (merged is historical)", summary.TotalActive)
		}
		if summary.TotalHistorical != 1 || len(summary.HistoricalLanes) != 1 {
			t.Fatalf("TotalHistorical = %d, HistoricalLanes = %d, want 1/1", summary.TotalHistorical, len(summary.HistoricalLanes))
		}
		if got := summary.HistoricalLanes[0].Phase; got != "complete" {
			t.Errorf("phase = %q, want complete", got)
		}
	})

	t.Run("finalize_blocked_nonrequired_ci marks the run blocked", func(t *testing.T) {
		summary := BuildRunSummary(setStatus(sourceAttributedFixture(), "finalize_blocked_nonrequired_ci"))
		if len(summary.BlockedLanes) != 1 {
			t.Fatalf("len(BlockedLanes) = %d, want 1", len(summary.BlockedLanes))
		}
		if got := summary.BlockedLanes[0].Phase; got != "blocked" {
			t.Errorf("phase = %q, want blocked", got)
		}
	})
}

// TestEnrichedSummaryEmitsArraysNotNull pins Step 4: the enriched wire shape
// always serializes historicalLanes / recentChanges / lanes / blockedLanes as
// [] rather than null — even when the input summary is the warming zero value.
func TestEnrichedSummaryEmitsArraysNotNull(t *testing.T) {
	// The warming case: a zero-value summary (all slices nil), enriched with no
	// sessions. This is exactly the shape the live BFF served as null.
	enriched := EnrichRunSummary(RunSummary{}, nil, false, time.Now().UnixMilli(), nil)

	got, err := json.Marshal(enriched)
	if err != nil {
		t.Fatalf("marshal enriched summary: %v", err)
	}
	js := string(got)
	for _, field := range []string{"lanes", "historicalLanes", "blockedLanes", "recentChanges"} {
		if strings.Contains(js, `"`+field+`":null`) {
			t.Errorf("field %q serialized as null, want []: %s", field, js)
		}
		if !strings.Contains(js, `"`+field+`":[`) {
			t.Errorf("field %q must serialize as an array: %s", field, js)
		}
	}
}

// TestSourceAttributedRunDetailProjects pins Step 3: /runs/{id}/detail does not
// 500 with "detail run root not found" when the gcg-* root is absent — a phantom
// root is synthesized from the source metadata and the graph.v2 detail projects.
func TestSourceAttributedRunDetailProjects(t *testing.T) {
	detail, err := BuildRunDetail(sourceAttributedFixture(), gcgRoot, 1, 100)
	if err != nil {
		t.Fatalf("BuildRunDetail: %v", err)
	}
	if detail.RunID != gcgRoot {
		t.Errorf("RunID = %q, want %q", detail.RunID, gcgRoot)
	}
	if detail.Formula.Kind != "known" || detail.Formula.Name != "mol-adopt-pr-v2" {
		t.Errorf("Formula = %+v, want known/mol-adopt-pr-v2", detail.Formula)
	}
	if detail.ScopeKind != "rig" || detail.ScopeRef != "gascity-packs" {
		t.Errorf("scope = %s/%s, want rig/gascity-packs", detail.ScopeKind, detail.ScopeRef)
	}
	if detail.RootStoreRef != "rig:gascity-packs" {
		t.Errorf("RootStoreRef = %q, want rig:gascity-packs", detail.RootStoreRef)
	}
}
