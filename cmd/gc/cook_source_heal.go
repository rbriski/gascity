package main

import (
	"errors"
	"log"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// cookSourceHealAction is the terminal transition the heal applies to a
// stranded `gc formula cook --attach` source once its launched molecule root is
// known to be done.
type cookSourceHealAction int

const (
	cookSourceHealSkip cookSourceHealAction = iota
	// cookSourceHealClose closes the source: its molecule finished with
	// gc.outcome=pass, so the attached work is genuinely complete.
	cookSourceHealClose
	// cookSourceHealReopen returns the source to open (stamping gc.outcome=fail
	// when it carries none): the molecule failed, or its root was purged, so the
	// work is honestly incomplete and must be visible/re-slingable again.
	cookSourceHealReopen
)

// classifyCookMarkerRoot maps a cook source's marker-root Get result to the
// terminal transition for the source. It is fail-safe: any liveness ambiguity
// (root still open, or a non-not-found read error such as a degraded graph leg)
// yields Skip so a live or unknown molecule is never torn down.
func classifyCookMarkerRoot(root beads.Bead, err error) cookSourceHealAction {
	if err != nil {
		if errors.Is(err, beads.ErrNotFound) {
			// Purged root: the molecule is gone with no recorded outcome. Reopen
			// so the strand is visible again rather than stuck in_progress.
			//
			// LOAD-BEARING ASSUMPTION: a genuinely-down graph leg must surface as
			// a NON-ErrNotFound error here, not a false ErrNotFound — otherwise a
			// transient graph outage would false-reopen a LIVE cook source. This
			// holds while ClassGraph is the last (or only) distinct backend in
			// coordrouter Router.Get's iteration, so its down-error wins over any
			// earlier backend's ErrNotFound. If the infra-decoupling work later
			// registers Messaging/Sessions/Orders/Nudges as distinct backends
			// ordered after ClassGraph, harden Router.Get to prefer a
			// non-ErrNotFound error, or gate this reopen on graph-leg health.
			return cookSourceHealReopen
		}
		// Leg down / PartialResultError / any other read error: assume a live
		// root exists and do NOT heal.
		return cookSourceHealSkip
	}
	if root.Status != "closed" {
		// open / in_progress root — molecule still live.
		return cookSourceHealSkip
	}
	if strings.TrimSpace(root.Metadata[beadmeta.OutcomeMetadataKey]) == "pass" {
		return cookSourceHealClose
	}
	// closed with gc.outcome=fail, or closed with no recorded outcome: treat as
	// a failed run and reopen.
	return cookSourceHealReopen
}

// applyCookSourceHeal writes the terminal transition. It re-reads the source
// through its owning store first so a source a worker claimed (or that another
// observer already healed) between the scan and now is left alone — the
// no-assignee + in_progress + marker-present invariant is re-asserted against
// live state before any write. This re-read is what makes a partial/stale scan
// safe: at worst a heal is deferred, never wrongly applied.
func applyCookSourceHeal(store beads.Store, sourceID string, action cookSourceHealAction) (bool, error) {
	if store == nil || action == cookSourceHealSkip {
		return false, nil
	}
	fresh, err := store.Get(sourceID)
	if err != nil {
		// Source gone or its leg went down since the scan — skip this tick.
		return false, nil
	}
	if fresh.Status != "in_progress" || strings.TrimSpace(fresh.Assignee) != "" {
		return false, nil
	}
	if strings.TrimSpace(fresh.Metadata[beadmeta.CookAttachLaunchMetadataKey]) == "" {
		// Marker cleared by a concurrent heal — nothing to do.
		return false, nil
	}

	var status, outcome string
	switch action {
	case cookSourceHealClose:
		status, outcome = "closed", "pass"
	case cookSourceHealReopen:
		status, outcome = "open", "fail"
	default:
		return false, nil
	}

	// Always clear the marker so the pass is idempotent; preserve any outcome
	// the source already carries (mirrors closeSourceBeadPreservingOutcome /
	// reopenSourceBeadPreservingOutcome).
	metadata := map[string]string{beadmeta.CookAttachLaunchMetadataKey: ""}
	if strings.TrimSpace(fresh.Metadata[beadmeta.OutcomeMetadataKey]) == "" {
		metadata[beadmeta.OutcomeMetadataKey] = outcome
	}
	if err := store.Update(sourceID, beads.UpdateOpts{Status: &status, Metadata: metadata}); err != nil {
		return false, err
	}
	return true, nil
}

// collectStrandedCookSources scans the city store and each rig store for cook
// sources stranded in_progress: status in_progress, no assignee, carrying the
// gc.cook_attach_launch marker. Returns index-aligned (source beads, owning
// stores).
//
// A store whose in_progress list fails is skipped this tick. That is safe
// because a partial or stale scan can only DEFER a heal, never trigger a false
// one: applyCookSourceHeal re-asserts the live in_progress + no-assignee +
// marker invariant against the owning store before writing, and
// classifyCookMarkerRoot fails safe on any marker-root read error. This lets
// the heal stay a small fork-owned pass instead of threading capture state
// through the hot collectAssignedWorkBeads path.
func collectStrandedCookSources(cityStore beads.Store, rigStores map[string]beads.Store) ([]beads.Bead, []beads.Store) {
	var sources []beads.Bead
	var sourceStores []beads.Store
	scan := func(store beads.Store) {
		if store == nil {
			return
		}
		inProgress, err := listBothTiersForControllerDemand(store, beads.ListQuery{Status: "in_progress"})
		if err != nil {
			return
		}
		for _, b := range inProgress {
			if strings.TrimSpace(b.Assignee) != "" {
				continue
			}
			if strings.TrimSpace(b.Metadata[beadmeta.CookAttachLaunchMetadataKey]) == "" {
				continue
			}
			sources = append(sources, b)
			sourceStores = append(sourceStores, store)
		}
	}
	scan(cityStore)
	// Deterministic rig order keeps logs and tests stable.
	refs := make([]string, 0, len(rigStores))
	for ref := range rigStores {
		refs = append(refs, ref)
	}
	sort.Strings(refs)
	for _, ref := range refs {
		scan(rigStores[ref])
	}
	return sources, sourceStores
}

// healStrandedCookAttachSources resolves the terminal state of `gc formula cook
// --attach` source work beads that markCookSourceInProgress flipped to
// in_progress but whose launched molecule root finished — or was purged —
// without ever transitioning the source. The cook root carries no
// gc.source_bead_id back to its source, so neither finalize's close-on-pass nor
// its reopen-on-fail (both walk gc.source_bead_id) ever reach these sources;
// this pass is their sole terminal transition.
//
// For each candidate the marker root's status decides the transition (see
// classifyCookMarkerRoot); applyCookSourceHeal re-asserts the invariant against
// live state before writing. cityStore (the federated city Router) reads the
// gcg- marker root, which always lives in the city-scoped graph leg regardless
// of which store owns the source; source writes use each candidate's own
// index-aligned owning store. Returns the ids healed this tick, for logging.
func healStrandedCookAttachSources(cityStore beads.Store, sources []beads.Bead, sourceStores []beads.Store) []string {
	if cityStore == nil || len(sources) == 0 {
		return nil
	}
	storeAware := len(sourceStores) == len(sources)
	var healed []string
	for i, src := range sources {
		markerRoot := strings.TrimSpace(src.Metadata[beadmeta.CookAttachLaunchMetadataKey])
		if markerRoot == "" {
			continue
		}
		// The marker root is a gcg- graph bead in the city-scoped graph leg, so
		// read it through the federated city Router.
		root, err := cityStore.Get(markerRoot)
		action := classifyCookMarkerRoot(root, err)
		if action == cookSourceHealSkip {
			continue
		}
		srcStore := cityStore
		if storeAware && sourceStores[i] != nil {
			srcStore = sourceStores[i]
		}
		did, healErr := applyCookSourceHeal(srcStore, src.ID, action)
		if healErr != nil {
			log.Printf("healStrandedCookAttachSources: heal failed for %q (marker root %q): %v", src.ID, markerRoot, healErr)
			continue
		}
		if did {
			verb := "reopened"
			if action == cookSourceHealClose {
				verb = "closed"
			}
			log.Printf("healStrandedCookAttachSources: %s stranded cook source %q (marker root %q done)", verb, src.ID, markerRoot)
			healed = append(healed, src.ID)
		}
	}
	return healed
}
