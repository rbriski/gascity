package main

import (
	"context"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// journalFrontierTimeout bounds the in-process ControlFrontier SELECT. It is far
// tighter than the legacy 60s hookWorkQueryTimeout (an indexed local SQLite read
// is sub-millisecond) so a wedged WAL cannot stall the serve tick longer than the
// legacy shell path could. Injectable for tests.
var journalFrontierTimeout = 5 * time.Second

// journalFrontierFunc reads the journal-resident ready control beads for one
// serve tick. opted reports whether the city has a journal graph scope: a
// non-opted city returns (nil, false, nil) and the serve tick stays byte-identical
// to the pre-P2 legacy path. A hard read error (opted city, journal leg present)
// returns a non-nil err that MUST hard-fail the tick — never flattened to "no
// journal work".
type journalFrontierFunc func() (rows []hookBead, opted bool, err error)

// workflowServeJournalList is the injectable journal-frontier reader. Production
// resolves the city's journal leg via the P1.5 one-shot cache; tests swap it to
// feed synthetic frontiers or to assert it is never invoked on the legacy path.
var workflowServeJournalList = journalControlFrontierList

// journalControlFrontierList resolves the city's journal graph leg, probes the
// ControlFrontier capability, and runs the indexed SELECT for the tick's params.
// A city with no .gc/graph scope resolves to a nil leg — (nil, false, nil), the
// INERT signal — so the serve tick is byte-identical for non-opted cities. An
// OPTED city whose leg cannot be opened/probed returns (nil, true, err) so the
// serve tick hard-fails rather than silently stranding journal-resident roots on
// a legacy-only queue (MEDIUM-1); shadow mode degrades that error to legacy in
// composeWorkflowServeQueue, only serve mode hard-fails.
func journalControlFrontierList(cityPath string, params beads.ControlFrontierParams) ([]hookBead, bool, error) {
	store, opted, err := cachedCityGraphJournalResult(cityPath)
	if err != nil {
		// Opted city whose journal leg could not be opened/probed. This is NOT
		// "no journal work": report opted=true with the error so serve mode fails
		// loudly (MEDIUM-1) instead of routing journal roots to legacy silently.
		return nil, true, err
	}
	if store == nil || !opted {
		// Genuinely not opted: the journal leg is absent, so skip it entirely and
		// keep the serve tick byte-identical for non-opted cities.
		return nil, false, nil
	}
	frontier, ok := beads.ControlFrontierStoreFor(store)
	if !ok {
		// Opted city whose leg does not expose the capability: treat as absent
		// rather than erroring — no journal-resident frontier is available.
		return nil, false, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), journalFrontierTimeout)
	defer cancel()
	rows, err := frontier.ControlFrontier(ctx, params)
	if err != nil {
		return nil, true, err
	}
	return hookBeadsFromBeads(rows), true, nil
}

// makeJournalFrontierFn builds the per-tick journal-frontier closure for a
// control-dispatcher serve loop. It derives the ControlFrontierParams once (env
// and identity are stable for the session) and delegates each tick to
// workflowServeJournalList. Returns nil when the agent is not the control
// dispatcher, so the drain loop's journal path is skipped wholesale for ordinary
// worker serves.
func makeJournalFrontierFn(cityPath string, agentCfg config.Agent, beadsCfg config.BeadsConfig, controlSessionName string, workEnv map[string]string) journalFrontierFunc {
	envLookup := func(key string) string {
		if workEnv != nil {
			if v, ok := workEnv[key]; ok {
				return v
			}
		}
		return os.Getenv(key)
	}
	params := controlFrontierInputs(agentCfg, beadsCfg, controlSessionName, envLookup)
	return func() ([]hookBead, bool, error) {
		return workflowServeJournalList(cityPath, params)
	}
}

// controlFrontierInputs maps the exact same levers the legacy `bd | jq` serve-tick
// shell derives (workflowServeControlReadyQueryForBeads) into a typed
// ControlFrontierParams, so the journal SELECT and the shell frontier can never
// disagree about WHO is being asked for. Line numbers rot as the file shifts;
// these anchors name the stable pieces of workflowServeControlReadyQueryForBeads.
//
// Mapping (verified against the shell):
//   - AssigneeCandidates: the shell's `for id in ...` assignee loop walks, in
//     order, [GC_CONTROL_SESSION_NAME, GC_SESSION_NAME, GC_ALIAS, GC_CONTROL_TARGET,
//     GC_SESSION_ID] and, for each non-blank id, tries "id" then its legacy alias
//     "${id%control-dispatcher}workflow-control". We build that interleaved list;
//     blank/repeat skipping is done by ControlFrontier's own $seen-equivalent dedupe.
//   - Routes: [GC_CONTROL_TARGET, GC_CONTROL_LEGACY_TARGET, GC_CONTROL_BARE_TARGET],
//     in that order (the shell's routed_ready call sequence).
//   - RouteMetadataKeys: {gc.run_target, gc.routed_to} — run_target first, matching
//     the shell's per-route emit order (RunTargetMetadataKey then RoutedToMetadataKey)
//     so first-wins dedupe prefers a run_target match.
//   - InstantiatingMetadataKey: the jq instantiating-drop key (the reduce filter in
//     workflowServeControlReadyQueryForBeads).
//   - IncludeEphemeral: UsesBD105ReadySemantics (the `--include-ephemeral` toggle).
//   - LimitPerTier: workflowServeScanLimit, the per-`bd ready` `--limit`.
func controlFrontierInputs(agentCfg config.Agent, beadsCfg config.BeadsConfig, controlSessionName string, envLookup func(string) string) beads.ControlFrontierParams {
	target := strings.TrimSpace(agentCfg.QualifiedName())
	if target == "" {
		target = config.ControlDispatcherAgentName
	}

	ids := []string{
		strings.TrimSpace(controlSessionName), // GC_CONTROL_SESSION_NAME
		strings.TrimSpace(envLookup("GC_SESSION_NAME")),
		strings.TrimSpace(envLookup("GC_ALIAS")),
		target, // GC_CONTROL_TARGET
		strings.TrimSpace(envLookup("GC_SESSION_ID")),
	}
	var candidates []string
	for _, id := range ids {
		if id == "" {
			continue
		}
		candidates = append(candidates, id)
		if legacy := controlFrontierLegacyAssigneeAlias(id); legacy != "" {
			candidates = append(candidates, legacy)
		}
	}

	routes := []string{target}
	if legacy := workflowServeLegacyControlRoute(target); legacy != "" {
		routes = append(routes, legacy)
	}
	if bare := controlDispatcherBareRoute(target); bare != "" {
		routes = append(routes, bare)
	}

	return beads.ControlFrontierParams{
		AssigneeCandidates:       candidates,
		Routes:                   routes,
		RouteMetadataKeys:        []string{beadmeta.RunTargetMetadataKey, beadmeta.RoutedToMetadataKey},
		InstantiatingMetadataKey: beadmeta.InstantiatingMetadataKey,
		IncludeEphemeral:         beadsCfg.UsesBD105ReadySemantics(),
		LimitPerTier:             workflowServeScanLimit,
	}
}

// controlFrontierLegacyAssigneeAlias mirrors the shell's per-candidate legacy
// alias: an id ending in the literal "control-dispatcher" also probes
// "${id%control-dispatcher}workflow-control" (the `legacy="${id%...}"` case in
// workflowServeControlReadyQueryForBeads). Note this is a plain literal suffix
// strip, distinct from workflowServeLegacyControlRoute's route-form
// ("/control-dispatcher") handling used for the routed tiers.
func controlFrontierLegacyAssigneeAlias(id string) string {
	const suffix = "control-dispatcher"
	if strings.HasSuffix(id, suffix) {
		return strings.TrimSuffix(id, suffix) + "workflow-control"
	}
	return ""
}

// hookBeadsFromBeads projects the ControlFrontier's typed beads onto the hookBead
// shape the serve tick's per-bead processing consumes. Only the id and metadata
// (used for the kind switch and instantiating filter) are needed downstream.
func hookBeadsFromBeads(in []beads.Bead) []hookBead {
	out := make([]hookBead, 0, len(in))
	for _, b := range in {
		md := make(hookBeadMetadata, len(b.Metadata))
		for k, v := range b.Metadata {
			md[k] = v
		}
		out = append(out, hookBead{ID: b.ID, Metadata: md})
	}
	return out
}

// mergeDedupeHookBeads unions the legacy `bd | jq` frontier with the journal
// frontier for serve mode: legacy rows first (preserving today's processing
// order for every legacy-resident bead), then journal rows whose id is not
// already present. Residence makes the two id sets disjoint (journal ids are
// gcg-j*), so the dedupe is a belt over that suspenders.
func mergeDedupeHookBeads(legacy, journal []hookBead) []hookBead {
	if len(journal) == 0 {
		return legacy
	}
	seen := make(map[string]struct{}, len(legacy))
	for _, b := range legacy {
		seen[b.ID] = struct{}{}
	}
	out := legacy
	for _, b := range journal {
		if _, ok := seen[b.ID]; ok {
			continue
		}
		out = append(out, b)
	}
	return out
}

// frontierIDSetDiff returns the ids present in exactly one of the two frontiers,
// each sorted for a deterministic event payload.
func frontierIDSetDiff(legacy, journal []hookBead) (onlyInLegacy, onlyInJournal []string) {
	legacySet := make(map[string]struct{}, len(legacy))
	for _, b := range legacy {
		legacySet[b.ID] = struct{}{}
	}
	journalSet := make(map[string]struct{}, len(journal))
	for _, b := range journal {
		journalSet[b.ID] = struct{}{}
	}
	for id := range legacySet {
		if _, ok := journalSet[id]; !ok {
			onlyInLegacy = append(onlyInLegacy, id)
		}
	}
	for id := range journalSet {
		if _, ok := legacySet[id]; !ok {
			onlyInJournal = append(onlyInJournal, id)
		}
	}
	sort.Strings(onlyInLegacy)
	sort.Strings(onlyInJournal)
	return onlyInLegacy, onlyInJournal
}

// frontiersShareAnyID reports whether the two frontiers have at least one bead id
// in common — the residence overlap. At P2 the legacy leg (bd ready over the
// city/rig store) and the journal leg (ControlFrontier over .gc/graph/journal.db)
// hold residence-DISJOINT populations, so this is false and the shadow-tee stays
// dormant (HIGH-2).
func frontiersShareAnyID(legacy, journal []hookBead) bool {
	// Index the smaller side to keep the probe cheap.
	small, large := legacy, journal
	if len(journal) < len(legacy) {
		small, large = journal, legacy
	}
	seen := make(map[string]struct{}, len(small))
	for _, b := range small {
		seen[b.ID] = struct{}{}
	}
	for _, b := range large {
		if _, ok := seen[b.ID]; ok {
			return true
		}
	}
	return false
}

// emitFrontierShadowDivergence compares the legacy and journal frontiers for a
// shadow-mode tick. It records a typed frontier.shadow.divergence event and a
// workflowTracef line (returning true) only when the two legs share a bead
// population AND their ready-bead id sets differ within it; otherwise it emits
// nothing and returns false. The caller always serves the legacy result regardless.
//
// HIGH-2 — dormant-correct at P2: until the P3 dual-write mirror exists, the
// legacy and journal legs are residence-DISJOINT (a bead lives in exactly one
// leg), so a naive full-set symmetric difference would flag the ENTIRE legacy
// ready set as only_in_legacy on every drain — a spurious divergence every tick,
// making "zero divergence over a soak" unreachable. Restricting the emit to a
// non-empty residence overlap makes the tee silent under the P2 reality and live
// only once a genuinely shared population appears.
//
// NOTE: a MEANINGFUL live shadow-compare — seed-identical state, then assert the
// journal ControlFrontier == bd-ready over the SAME beads — requires the P3
// dual-write mirror. That end-to-end Layer-2 parity gate is a P3 integration
// deliverable; this tee is only the dormant plumbing until then (see the P2
// frontier-cutover design, §8).
func emitFrontierShadowDivergence(cityPath string, stderr io.Writer, agent string, legacy, journal []hookBead) bool {
	if !frontiersShareAnyID(legacy, journal) {
		// Residence-disjoint (the P2 reality): the two legs share no bead, so there
		// is nothing to reconcile and no real divergence to report.
		return false
	}
	onlyInLegacy, onlyInJournal := frontierIDSetDiff(legacy, journal)
	if len(onlyInLegacy) == 0 && len(onlyInJournal) == 0 {
		return false
	}
	workflowTracef("serve shadow-divergence agent=%s only_legacy=%d only_journal=%d legacy=%d journal=%d",
		agent, len(onlyInLegacy), len(onlyInJournal), len(legacy), len(journal))
	rec := openCityRecorderAt(cityPath, stderr)
	if closer, ok := rec.(interface{ Close() error }); ok {
		defer closer.Close() //nolint:errcheck // best-effort event recorder cleanup
	}
	rec.Record(events.Event{
		Type:    events.FrontierShadowDivergence,
		Actor:   eventActor(),
		Subject: agent,
		Message: "control-dispatcher journal frontier diverged from bd|jq frontier",
		Payload: events.FrontierShadowDivergencePayloadJSON(agent, onlyInLegacy, onlyInJournal, len(legacy), len(journal)),
	})
	return true
}
