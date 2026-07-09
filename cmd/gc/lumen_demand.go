package main

import (
	"context"
	"fmt"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// lumenScaleCheckDemand is the native pool-demand probe for Lumen journal work:
// the shell `bd ready` probe cannot see the graph journal, so this composition-root
// source counts the OPEN, ready, unassigned pool-mode Tier-B rows in the frontier
// per pool template and returns per-template counts and demand (WorkBeadIDs +
// StoreRefs[id]=tierBHookStoreName, so the launch path resolves the row through the
// graph-journal store), plus the templates whose probe was partial (drain
// suppression) and any surfaced error.
//
// Three journal outcomes, mirroring the serve-mode hard-fail discipline:
//   - not opted (no .gc/graph scope): empty result, no partial — a legacy city has
//     no journal demand.
//   - opted and opened: the counted frontier.
//   - opted-but-unopenable (a transient/misconfigured journal): EVERY probed
//     template is marked partial and the error is surfaced, so the caller suppresses
//     drain decisions rather than treating a 0 as authoritative (a false 0 would
//     drain a pool whose journal work is merely unreadable this tick).
func lumenScaleCheckDemand(cityPath string, templates []string) (map[string]int, map[string]scaleCheckDemand, map[string]bool, error) {
	counts := map[string]int{}
	demand := map[string]scaleCheckDemand{}
	if len(templates) == 0 {
		return counts, demand, nil, nil
	}

	store, opted, err := cachedCityGraphJournalResult(cityPath)
	if err != nil {
		// Opted-but-unopenable: suppress drain on every probed template and surface
		// the error; do NOT assert an authoritative 0 count.
		partials := markLumenTemplatesPartial(templates)
		return counts, demand, partials, fmt.Errorf("lumen frontier demand: %w", err)
	}
	if !opted || store == nil {
		return counts, demand, nil, nil // not opted: no journal demand
	}
	surface, ok := beads.TierBClaimSurfaceStoreFor(store)
	if !ok {
		return counts, demand, nil, nil
	}

	// One read-only WAL snapshot over every probed route; the returned rows carry
	// gc.routed_to, so each is bucketed to its pool template.
	rows, err := surface.TierBRoutedFrontier(context.Background(), templates, 0)
	if err != nil {
		partials := markLumenTemplatesPartial(templates)
		return counts, demand, partials, fmt.Errorf("lumen frontier demand: %w", err)
	}
	for _, b := range rows {
		route := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
		if route == "" {
			continue
		}
		counts[route]++
		entry := demand[route]
		entry.Count++
		entry.WorkBeadIDs = append(entry.WorkBeadIDs, b.ID)
		if entry.Titles == nil {
			entry.Titles = make(map[string]string)
		}
		entry.Titles[b.ID] = b.Title
		if entry.StoreRefs == nil {
			entry.StoreRefs = make(map[string]string)
		}
		entry.StoreRefs[b.ID] = tierBHookStoreName
		demand[route] = entry
	}
	return counts, demand, nil, nil
}

// markLumenTemplatesPartial marks every template partial (drain suppression).
func markLumenTemplatesPartial(templates []string) map[string]bool {
	var partials map[string]bool
	for _, t := range templates {
		partials = markScaleCheckPartialTemplate(partials, t)
	}
	return partials
}

// templateNamesFromScaleTargets is the deduped set of pool template names the
// native default-scale probe already tracks — exactly the routes the Lumen frontier
// demand probes, so a pool relying solely on a custom shell scale_check (invisible
// to the native probe) is out of scope by construction.
func templateNamesFromScaleTargets(targets []defaultScaleCheckTarget) []string {
	seen := map[string]bool{}
	var out []string
	for _, t := range targets {
		name := strings.TrimSpace(t.template)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}
