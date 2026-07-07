package main

import (
	"strings"

	"github.com/gastownhall/gascity/internal/config"
)

// The default work_query and pool-demand (count-form) scripts shell out to the
// single-store `bd` CLI, so on a split city they only ever see the work store —
// graph-class step beads in the infra store are invisible, and a worker/reconciler
// concludes "no work"/"no demand" (fail-open). These helpers rewrite the two
// single-store read verbs in the DEFAULT generated scripts to the composite
// `gc ready` command (which federates work ∪ infra in-process) when the city is
// split. A custom work_query/scale_check is caller-owned and left untouched; a
// legacy single-store city is byte-identical to before.

// splitCityWorkQuery returns the agent's effective work_query, rewritten to the
// composite `gc ready` on a split city. A custom work_query is returned unchanged.
func splitCityWorkQuery(cityPath string, a *config.Agent, beadsCfg config.BeadsConfig) string {
	query := a.EffectiveWorkQueryForBeads(beadsCfg)
	if strings.TrimSpace(a.WorkQuery) != "" {
		return query
	}
	return rewriteDefaultReadyForSplitCity(cityPath, query)
}

// splitCityPoolDemandQuery is splitCityWorkQuery for the reconciler count-form,
// keeping the reconciler's spawn-demand read symmetric with the worker's claim
// read (the "scale_check ↔ work_query correspondence" invariant). A custom
// scale_check is returned unchanged.
func splitCityPoolDemandQuery(cityPath string, a *config.Agent, beadsCfg config.BeadsConfig) string {
	query := a.EffectivePoolDemandQueryForBeads(beadsCfg)
	if strings.TrimSpace(a.ScaleCheck) != "" {
		return query
	}
	return rewriteDefaultReadyForSplitCity(cityPath, query)
}

// rewriteDefaultReadyForSplitCity swaps the single-store bd read verbs in a
// default work/demand script for the composite `gc ready` when cityPath is split.
// The two tokens are unambiguous command verbs in the generated script:
//   - "bd list --status in_progress --assignee=" (assigned in-progress crash
//     recovery) → "gc ready --status in_progress --assignee="
//   - "bd ready" (assigned-ready, routed-pool, and migration tiers) → "gc ready"
//
// "bd query" (the legacy-ephemeral fallback tiers) is deliberately NOT rewritten:
// it targets a retirement window (ga-dhf44) and reads ephemeral wisps, not the
// graph step path P0 needs. Returns query unchanged on a legacy single-store city.
func rewriteDefaultReadyForSplitCity(cityPath, query string) string {
	if !cityHasInfraStore(cityPath) {
		return query
	}
	query = strings.ReplaceAll(query,
		"bd list --status in_progress --assignee=",
		"gc ready --status in_progress --assignee=")
	query = strings.ReplaceAll(query, "bd ready", "gc ready")
	return query
}
