package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
	"github.com/gastownhall/gascity/internal/sourceworkflow"
)

// reapSourceTerminalWorkflows finalizes graph.v2 workflow roots whose tracked
// source work is already terminal (merged and closed) but whose own steps are
// still open. This is the controller-tick safety net for the stale-workflow
// re-dispatch bug (ga-tum): a mol-polecat-work molecule whose source bead the
// refinery already merged and closed keeps a non-terminal submit-and-exit step,
// which the reconciler otherwise counts as pool demand forever — re-dispatching
// polecats that can only fail submit-and-exit's branch-shape gate.
//
// The reactive close-hook reapers miss this shape: autocloseRootsForSourceBead
// requires the whole subtree terminal (the open step blocks it), and pool-routed
// graph.v2 roots clear gc.source_bead_id so ListLiveRoots never finds them. This
// pass keys on SOURCE terminality via sourceworkflow.WorkflowSourceTerminal
// instead, so an open step no longer protects an orphaned workflow. It is
// idempotent and best-effort — store errors are logged and skipped so a
// misbehaving store never wedges the tick — and it runs before the demand
// snapshot so a finalized root is already closed when demand is computed.
//
// stores is the set of work stores that can host workflow roots (the graph
// store plus every rig work store); duplicates and nils are ignored, and the
// remaining stores are threaded as member-probe stores so a source bead that
// lives in a different per-class store than its root still resolves. Returns the
// number of beads closed.
func reapSourceTerminalWorkflows(stores []beads.Store, stderr io.Writer) int {
	deduped := dedupeReapStores(stores)
	reaped := 0
	for i, store := range deduped {
		roots, err := listSourceTerminalReapCandidates(store)
		if err != nil {
			fmt.Fprintf(stderr, "stale-workflow reaper: listing graph.v2 roots: %v\n", err) //nolint:errcheck // best-effort infra
			continue
		}
		memberStores := reapStoresExcept(deduped, i)
		for _, root := range roots {
			terminal, err := sourceworkflow.WorkflowSourceTerminal(store, root, memberStores...)
			if err != nil || !terminal {
				continue
			}
			closed, err := sourceworkflow.CloseWorkflowSubtree(store, root.ID)
			if err != nil {
				fmt.Fprintf(stderr, "stale-workflow reaper: finalizing %s: %v\n", root.ID, err) //nolint:errcheck // best-effort infra
				continue
			}
			if closed > 0 {
				reaped += closed
				fmt.Fprintf(stderr, "stale-workflow reaper: finalized graph.v2 workflow %s (source terminal); closed %d bead(s)\n", root.ID, closed) //nolint:errcheck // best-effort infra
			}
		}
	}
	return reaped
}

// listSourceTerminalReapCandidates returns the open graph.v2 workflow roots in
// store that carry a source link (gc.input_convoy_id or gc.source_bead_id).
// Requiring a source link keeps step and control beads — which carry only
// gc.root_bead_id — out of the candidate set and guarantees
// WorkflowSourceTerminal has something to resolve, so the reaper never
// force-closes a step out from under a live root.
func listSourceTerminalReapCandidates(store beads.Store) ([]beads.Bead, error) {
	if store == nil {
		return nil, nil
	}
	matches, err := store.ListByMetadata(
		map[string]string{beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2},
		0, beads.WithBothTiers,
	)
	if err != nil {
		return nil, err
	}
	roots := make([]beads.Bead, 0, len(matches))
	for _, bead := range matches {
		if convoycore.IsTerminalStatus(bead.Status) {
			continue
		}
		if !sourceworkflow.IsWorkflowRoot(bead) {
			continue
		}
		if strings.TrimSpace(bead.Metadata[beadmeta.InputConvoyIDMetadataKey]) == "" &&
			strings.TrimSpace(bead.Metadata[beadmeta.SourceBeadIDMetadataKey]) == "" {
			continue
		}
		roots = append(roots, bead)
	}
	return roots, nil
}

// dedupeReapStores drops nil stores and collapses duplicate store values
// (bead stores are pointer-backed interfaces, so == compares identity). The
// graph store and a rig work store are byte-identical at the default backend,
// so this keeps the reaper from scanning the same store twice.
func dedupeReapStores(stores []beads.Store) []beads.Store {
	out := make([]beads.Store, 0, len(stores))
	for _, store := range stores {
		if store == nil {
			continue
		}
		seen := false
		for _, existing := range out {
			if existing == store {
				seen = true
				break
			}
		}
		if !seen {
			out = append(out, store)
		}
	}
	return out
}

// reapStoresExcept returns every store except the one at idx, for use as the
// cross-store member-probe set when resolving a root's source.
func reapStoresExcept(stores []beads.Store, idx int) []beads.Store {
	if len(stores) <= 1 {
		return nil
	}
	out := make([]beads.Store, 0, len(stores)-1)
	for i, store := range stores {
		if i == idx {
			continue
		}
		out = append(out, store)
	}
	return out
}
