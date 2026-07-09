package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// This file owns the REVERSE of the bead-id ⟷ (streamID, activation) bijection
// (blueprint 17 §5-L1 crux) plus the dispatch-outcome firewall — the two pure,
// engine-owned decode helpers the composition root (cmd/gc) calls to translate a
// worker's claim/close of a projected Tier-B bead back into journal coordinates.
//
// The FORWARD map (encode) already lives beside the reducer: an activation key is
// nodeID+":"+index, the projected bead id is the bare node id (activationNodeID),
// and nodeRowFor/nodeProjectedMeta stamp the projection. Keeping the reverse map
// here — next to the encoder, inside the engine — is what lets cmd/gc claim and
// close a Tier-B bead WITHOUT ever parsing an activation key or reading a metadata
// literal itself. It is deliberately NOT in internal/beads (the persistence
// substrate may not depend upward on the engine) and NOT in cmd/gc (activation
// keying is domain knowledge, not composition wiring).

// TierBWorkRef is the journal coordinates a projected Tier-B bead id decodes to.
// It is the single decode cmd/gc consumes to claim or close a pool work bead.
type TierBWorkRef struct {
	// BeadID is the bare node id (nodes.id) — the projected, claimable bead id.
	BeadID string
	// StreamID is nodes.stream_id: the run root / journal stream the claim and
	// settle appends target.
	StreamID string
	// Activation is the claim/settle handle (nodeID+":"+index). It is projected as
	// node_metadata only while the node is UNSETTLED; a settled node drops it
	// (nodeProjectedMeta), so Activation is "" once Settled — and must not be needed
	// (a settled re-close dedupes at the projection level, never via SettleTierBWork).
	Activation string
	// DispatchMode is the node's projected dispatch_mode marker. A bead is a
	// claimable Tier-B work bead only when this is DispatchModePool; an engine-driven
	// fold node resolves with DispatchMode "".
	DispatchMode string
	// Status is the projected nodes.status (open | in_progress | done | failed |
	// skipped).
	Status string
	// Assignee is the worker that claimed the bead (empty until claimed; retained
	// for provenance after settle).
	Assignee string
	// Outcome is the projected outcome metadata, set iff the node has settled.
	Outcome string
	// Retryable is the projected L5 attempt-loop classification: true iff the settle
	// was a firewall infrastructure strand (SettleTierBWorkAs retryable=true), projected
	// onto node metadata only when true (omit-when-false, L-1). A divergent-reclose
	// compare reads (Outcome, Retryable) so a firewall failed{retryable:true} strand is
	// never laundered into success under a worker's fail close (§4.3). A worker settle
	// (retryable=false) and every pre-L-1 settled row surface Retryable=false.
	Retryable bool
	// Settled reports whether the node has reached a terminal status.
	Settled bool
}

// ResolveTierBWorkRef maps a projected bead id back to its journal coordinates.
// It is the reverse of the encode pair (activation → bare node id). An absent or
// non-fold-owned id resolves to (zero, false, nil) — the caller's "not ours"
// signal, so a claim/close of an ordinary façade bead falls through untouched.
//
// It is total and unambiguous today because nodes.id is a global PK (one row per
// id ⇒ one stream) and P4.2/L0 mint exactly one activation per node (index 0). The
// L4 multi-run caveat (run-namespaced ids) is tracked at frontierRowFor; L1
// inherits the one-live-run-per-node-id-set constraint.
func ResolveTierBWorkRef(ctx context.Context, store *graphstore.Store, beadID string) (TierBWorkRef, bool, error) {
	if store == nil {
		return TierBWorkRef{}, false, fmt.Errorf("lumen tier-b: nil store")
	}
	if beadID == "" {
		return TierBWorkRef{}, false, nil
	}
	ref := TierBWorkRef{BeadID: beadID}
	err := store.ReadDB().QueryRowContext(ctx,
		`SELECT stream_id, status, COALESCE(assignee, '') FROM nodes WHERE id = ? AND fold_owned = 1`, beadID,
	).Scan(&ref.StreamID, &ref.Status, &ref.Assignee)
	if errors.Is(err, sql.ErrNoRows) {
		return TierBWorkRef{}, false, nil
	}
	if err != nil {
		return TierBWorkRef{}, false, fmt.Errorf("lumen tier-b: resolving node %q: %w", beadID, err)
	}

	rows, err := store.ReadDB().QueryContext(ctx,
		`SELECT key, value FROM node_metadata WHERE node_id = ?`, beadID)
	if err != nil {
		return TierBWorkRef{}, false, fmt.Errorf("lumen tier-b: reading metadata of %q: %w", beadID, err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var key, value string
		if err := rows.Scan(&key, &value); err != nil {
			return TierBWorkRef{}, false, fmt.Errorf("lumen tier-b: scanning metadata of %q: %w", beadID, err)
		}
		switch key {
		case "activation":
			ref.Activation = value
		case DispatchModeMetaKey:
			ref.DispatchMode = value
		case "outcome":
			ref.Outcome = value
		case "retryable":
			// Projected only when true (nodeProjectedMeta omit-when-false), so any
			// present value is the firewall-strand marker.
			ref.Retryable = value == "true"
		}
	}
	if err := rows.Err(); err != nil {
		return TierBWorkRef{}, false, fmt.Errorf("lumen tier-b: iterating metadata of %q: %w", beadID, err)
	}

	// Settlement is derived from the terminal status, matching readTierBNode: an
	// open or claimed (in_progress) node is live; anything else has settled.
	switch ref.Status {
	case "open", StatusClaimed:
		ref.Settled = false
	default:
		ref.Settled = true
	}
	return ref, true, nil
}

// LumenOutcomeForGCOutcome is the S7 dispatch-outcome firewall: it maps a raw
// gc.outcome metadata VALUE (as a pool worker's `gc bd` close writes it) onto a
// Lumen settlement outcome, fail-closed. It mirrors the v2 dispatch firewall
// (beadOutcomeFailed): only the recognized control-plane pass/fail
// (beadmeta.OutcomePass / beadmeta.OutcomeFail) and the Lumen-native degraded
// (beadmeta.OutcomeDegraded, engine vocab.go) map through; EVERYTHING else — an
// empty/bare close, an unknown token, a case variant, and even the control-plane
// `skipped` (which has no Lumen worker-close meaning) — maps to OutcomeFailed. The
// mapping is total and case-exact: no laundering of an unrecognized value into a
// success.
func LumenOutcomeForGCOutcome(gcOutcome string) string {
	switch gcOutcome {
	case beadmeta.OutcomePass:
		return OutcomePass
	case beadmeta.OutcomeFail:
		return OutcomeFailed
	case beadmeta.OutcomeDegraded:
		return OutcomeDegraded
	default:
		return OutcomeFailed
	}
}
