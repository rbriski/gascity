package engine

import "github.com/gastownhall/gascity/internal/beadmeta"

// LumenOutcomeForGCOutcome maps a raw gc.outcome metadata VALUE (as a pool
// worker's `gc bd` close writes it) onto a Lumen settlement outcome, fail-closed.
// The controller's observe seam (lumenObserveWork) applies it when it reads a
// dispatched work bead's terminal close, so the fold settles the do from an
// ordinary bead outcome.
//
// It mirrors the v2 dispatch firewall (internal/dispatch beadOutcomeFailed): only
// the recognized control-plane pass/fail (beadmeta.OutcomePass / beadmeta.OutcomeFail)
// and the Lumen-native degraded (beadmeta.OutcomeDegraded) map through; EVERYTHING
// else — an empty/bare close, an unknown token, a case variant, and even the
// control-plane `skipped` (which has no Lumen worker-close meaning) — maps to
// OutcomeFailed. The mapping is total and case-exact: no laundering of an
// unrecognized value into a success. It is a pure value map, not a session-state
// firewall.
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
