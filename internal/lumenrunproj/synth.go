// Package lumenrunproj projects a Lumen run — whose orchestration lives in the
// graph journal, not in molecule beads — into the existing dashboard run-view
// DTOs, so a Lumen run appears in the gc dashboard runs list and detail graph
// with no frontend change.
//
// It is fork-owned and read-only. It does NOT modify internal/runproj (the
// TS-parity port) and persists nothing: for each run it materializes an
// EPHEMERAL in-memory bead graph from the run's journal-stream projection
// (engine.FoldRunView) joined to the live do beads, then feeds that graph
// through runproj's ordinary BuildRunSummary / BuildRunDetailForRun. The
// synthetic beads exist only for the duration of one projection call and are
// discarded — nothing is written to any store, so this is not a shadow-bead
// scheme (the journal remains the sole source of truth).
package lumenrunproj

import (
	"strconv"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// syntheticBeads materializes the ephemeral in-memory bead graph runproj folds
// for one Lumen run. view supplies the run topology + settled status (from the
// per-run journal stream); doByActivation supplies the live per-step status,
// session, and timestamps (from the tailer's folded do beads, keyed by the do
// bead's gc.lumen_activation). cityName scopes the run.
//
// The metadata is chosen so runproj's reconstruction is identity: exactly the
// markers that qualify the run (gc.formula_contract=graph.v2) and identify each
// step (gc.step_id = bare node id) without any of the disambiguation keys
// (gc.iteration / gc.logical_bead_id / gc.scope_ref / gc.kind / …) that would
// collapse, rename, or hide a node.
func syntheticBeads(view engine.RunView, doByActivation map[string]beads.Bead, cityName string) []beads.Bead {
	streamID := view.RootID

	root := beads.Bead{
		ID:    streamID,
		Title: firstNonEmpty(view.Name, streamID),
		Type:  "task",
		Metadata: beads.StringMap{
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.KindMetadataKey:            "run",
			beadmeta.ScopeKindMetadataKey:       "city",
			beadmeta.ScopeRefMetadataKey:        cityName,
			beadmeta.RootStoreRefMetadataKey:    "city:" + cityName,
		},
	}
	if view.FormulaRef != "" {
		root.Metadata[beadmeta.FormulaMetadataKey] = view.FormulaRef
	}
	if view.Closed {
		root.Status = "closed"
		if runOutcomeFailed(view.Outcome) {
			root.Metadata[beadmeta.OutcomeMetadataKey] = engine.OutcomeFailed
		}
	} else {
		root.Status = "open"
	}

	// Root FIRST: snapshotDeps roots the display "parent" edges on members[0].
	out := make([]beads.Bead, 0, len(view.Activations)+1)
	out = append(out, root)

	var earliest, latest time.Time
	track := func(t time.Time) {
		if t.IsZero() {
			return
		}
		if earliest.IsZero() || t.Before(earliest) {
			earliest = t
		}
		if t.After(latest) {
			latest = t
		}
	}

	for _, a := range view.Activations {
		id := streamID + "." + a.Activation
		do, hasDo := doByActivation[a.Activation]

		step := beads.Bead{
			ID:       id,
			ParentID: streamID,
			Type:     "task",
			Title:    a.NodeID,
			Status:   stepStatus(a, do, hasDo),
			Metadata: beads.StringMap{
				beadmeta.RootBeadIDMetadataKey: streamID,
				beadmeta.StepIDMetadataKey:     a.NodeID,
				// 1-based: detail_nodeshape's numericFieldRe is ^[1-9]\d*$, so a
				// 0-based "0" is silently dropped to an untracked attempt.
				beadmeta.AttemptMetadataKey: strconv.Itoa(a.Attempt + 1),
			},
		}
		if badge := stepOutcomeBadge(a); badge != "" {
			step.Metadata[beadmeta.OutcomeMetadataKey] = badge
		}
		if hasDo {
			step.Assignee = do.Assignee
			if v := do.Metadata[beadmeta.SessionIDMetadataKey]; v != "" {
				step.Metadata[beadmeta.SessionIDMetadataKey] = v
			}
			if v := do.Metadata[beadmeta.SessionNameMetadataKey]; v != "" {
				step.Metadata[beadmeta.SessionNameMetadataKey] = v
			}
			step.CreatedAt = do.CreatedAt
			step.UpdatedAt = do.UpdatedAt
			track(do.CreatedAt)
			track(do.UpdatedAt)
		}
		// After → dependency edges (empty type → runproj renders "dependency");
		// Members → member edges. Endpoints are the peer activations' synthetic
		// ids; snapshotDeps drops any edge whose endpoint is not a member.
		for _, dep := range a.After {
			step.Dependencies = append(step.Dependencies, beads.Dep{
				DependsOnID: streamID + "." + dep,
				IssueID:     id,
				Type:        "",
			})
		}
		for _, m := range a.Members {
			step.Dependencies = append(step.Dependencies, beads.Dep{
				DependsOnID: streamID + "." + m,
				IssueID:     id,
				Type:        "member",
			})
		}
		out = append(out, step)
	}

	// Root timestamps must be non-zero or the lane's updatedAt union degrades
	// and compareLanes zeroes it.
	rootCreated := parseRunTime(view.CreatedAt)
	if rootCreated.IsZero() {
		rootCreated = earliest
	}
	if rootCreated.IsZero() {
		rootCreated = time.Now()
	}
	rootUpdated := latest
	if rootUpdated.IsZero() {
		rootUpdated = rootCreated
	}
	out[0].CreatedAt = rootCreated
	out[0].UpdatedAt = rootUpdated
	for i := 1; i < len(out); i++ {
		if out[i].CreatedAt.IsZero() {
			out[i].CreatedAt = rootCreated
		}
		if out[i].UpdatedAt.IsZero() {
			out[i].UpdatedAt = out[i].CreatedAt
		}
	}
	return out
}

// stepStatus maps an activation's fold status (overlaid with the live do bead)
// to a beads-vocabulary status. runproj's phase logic matches "closed" exactly,
// so a settled activation must be "closed" (the outcome badge, not the status,
// carries fail/skip/degrade).
func stepStatus(a engine.RunActivationView, do beads.Bead, hasDo bool) string {
	if a.Settled {
		return "closed"
	}
	if hasDo && do.Status != "" {
		return do.Status
	}
	return "open"
}

// stepOutcomeBadge returns the gc.outcome to stamp on a settled step so the
// display honestly reflects a failed/skipped/degraded settle; a pass or pending
// settle carries no badge (closed == success).
func stepOutcomeBadge(a engine.RunActivationView) string {
	if !a.Settled {
		return ""
	}
	switch a.Outcome {
	case engine.OutcomeFailed, engine.OutcomeCanceled:
		return engine.OutcomeFailed
	case engine.OutcomeSkipped:
		return engine.OutcomeSkipped
	case engine.OutcomeDegraded:
		return engine.OutcomeDegraded
	default:
		return ""
	}
}

// runOutcomeFailed reports whether a terminal run outcome is a failure.
func runOutcomeFailed(outcome string) bool {
	return outcome == engine.OutcomeFailed || outcome == engine.OutcomeCanceled
}

// parseRunTime parses a run.started timestamp, tolerating the RFC3339 variants
// the engine emits. It returns the zero time on failure (callers fall back).
func parseRunTime(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t
		}
	}
	return time.Time{}
}

// firstNonEmpty returns a if non-empty, else b.
func firstNonEmpty(a, b string) string {
	if a != "" {
		return a
	}
	return b
}
