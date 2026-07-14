package dashboardbff

import (
	"context"
	"encoding/json"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runproj"
)

// maxMergedHistoricalLanes bounds the historical lanes on the wire after merging
// Lumen runs into the bead-derived summary, mirroring runproj's own cap so the
// merge cannot blow the invariant.
const maxMergedHistoricalLanes = 50

// LumenRunProjector is the optional fork seam that folds a city's Lumen runs
// (whose orchestration lives in the graph journal, not in molecule beads) into
// the dashboard run-view DTOs. It is nil in upstream builds — the plane then
// behaves exactly as before, with zero Lumen awareness. internal/lumenrunproj
// provides the concrete implementation.
//
// foldedBeads is the tailer's warm run-bead slice; the projector joins each
// Lumen activation to its live do bead there (for per-step status and session
// links) rather than opening a second store.
type LumenRunProjector interface {
	// SummaryLanes returns the Lumen run lanes to merge into a city's run
	// summary. It returns an empty slice for a city with no Lumen runs.
	SummaryLanes(ctx context.Context, cityName, cityRoot string, foldedBeads []beads.Bead) ([]runproj.RunLane, error)
	// Detail projects a single Lumen run's detail. The bool reports whether
	// runID resolves to a Lumen run; false means "not a Lumen run" and the
	// caller keeps its existing 404.
	Detail(ctx context.Context, cityName, cityRoot, runID string, foldedBeads []beads.Bead) (runproj.FormulaRunDetail, bool, error)
}

// mergeLumenLanes folds the city's Lumen runs into base's lanes before
// enrichment, so the shared EnrichRunSummary pass covers them too. It is a no-op
// when the LumenRuns seam is unset (upstream builds) or the city root cannot be
// resolved, and is best-effort: a Lumen-fold failure never disturbs the
// bead-derived lanes.
func (t *cityRunTailer) mergeLumenLanes(ctx context.Context, base runproj.RunSummary, folded []beads.Bead) runproj.RunSummary {
	proj := t.mgr.deps.LumenRuns
	if proj == nil {
		return base
	}
	root, ok := t.mgr.deps.Resolver.CityPath(t.name)
	if !ok {
		return base
	}
	lanes, err := proj.SummaryLanes(ctx, t.name, root, folded)
	if err != nil || len(lanes) == 0 {
		// Best-effort: a Lumen-fold failure degrades the optional Lumen view but
		// never disturbs the bead-derived lanes (it is not a swallowed bug — the
		// journal read is genuinely optional to the bead projection).
		return base
	}

	var active, blocked, historical []runproj.RunLane
	for _, lane := range lanes {
		switch lane.Phase {
		case "complete":
			historical = append(historical, lane)
		case "blocked":
			blocked = append(blocked, lane)
		default:
			active = append(active, lane)
		}
	}

	// Copy-on-write: BuildRunSummary returns slices with spare capacity and base
	// aliases the tailer's cached summary, so appending in place would race a
	// concurrent enrichedSummary AND mutate the cache. Build fresh slices.
	if len(active) > 0 {
		base.Lanes = concatLanes(base.Lanes, active)
	}
	if len(blocked) > 0 {
		base.BlockedLanes = concatLanes(base.BlockedLanes, blocked)
	}
	if len(historical) > 0 {
		base.HistoricalLanes = capHistoricalLanes(concatLanes(base.HistoricalLanes, historical))
		base.TotalHistorical += len(historical)
	}
	// TotalActive, the blocked/kind counts, and RunCounts are recomputed by
	// EnrichRunSummary from base.Lanes/base.BlockedLanes, so they need no manual
	// bump here — only HistoricalLanes/TotalHistorical (which enrich leaves
	// untouched) are adjusted above.
	return base
}

// concatLanes returns a fresh slice holding a followed by b (never aliasing
// either input's backing array).
func concatLanes(a, b []runproj.RunLane) []runproj.RunLane {
	out := make([]runproj.RunLane, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)
	return out
}

// capHistoricalLanes keeps the most-recent maxMergedHistoricalLanes historical
// lanes (by updatedAt, most-recent first) when the merged set exceeds the cap,
// so a Lumen merge onto an already-full bead history cannot exceed the wire
// invariant or systematically drop the newer Lumen runs. The input is a fresh
// slice (from concatLanes), so the in-place sort is safe.
func capHistoricalLanes(lanes []runproj.RunLane) []runproj.RunLane {
	if len(lanes) <= maxMergedHistoricalLanes {
		return lanes
	}
	sort.SliceStable(lanes, func(i, j int) bool {
		return lanes[i].UpdatedAt.At > lanes[j].UpdatedAt.At
	})
	return lanes[:maxMergedHistoricalLanes]
}

// lumenDetail projects a Lumen run's detail as marshaled JSON bytes, or
// (nil, false) when there is no Lumen projector, the id is not a Lumen run, or
// the projection/marshal fails. The detail route consults it only after the
// bead-derived fold reports the run root absent.
func (t *cityRunTailer) lumenDetail(ctx context.Context, runID string) ([]byte, bool) {
	proj := t.mgr.deps.LumenRuns
	if proj == nil {
		return nil, false
	}
	root, ok := t.mgr.deps.Resolver.CityPath(t.name)
	if !ok {
		return nil, false
	}
	t.mu.RLock()
	folded := t.beads
	t.mu.RUnlock()

	detail, isLumen, err := proj.Detail(ctx, t.name, root, runID, folded)
	if err != nil || !isLumen {
		// Best-effort like mergeLumenLanes: a non-Lumen id or a Lumen-fold failure
		// falls through to the caller's existing 404/503 rather than fabricating a
		// detail. (Not a swallowed bug — the Lumen projection is optional.)
		return nil, false
	}
	raw, err := json.Marshal(detail)
	if err != nil {
		return nil, false
	}
	return raw, true
}
