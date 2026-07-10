package engine

import "github.com/gastownhall/gascity/internal/beadmeta"

// LumenOutcomeForGCOutcome maps a raw gc.outcome metadata VALUE (as a pool
// worker's `gc bd` close writes it) onto a Lumen settlement outcome, fail-closed.
// The controller's observe seam (lumenObserveWork) applies it when it reads a
// dispatched work bead's terminal close, so the fold settles the do from an
// ordinary bead outcome.
//
// Policy: only the recognized control-plane pass/fail (beadmeta.OutcomePass /
// beadmeta.OutcomeFail) and the Lumen-native degraded (beadmeta.OutcomeDegraded)
// map through; EVERYTHING else — an empty/bare close, an unknown token, a case
// variant, and even the control-plane `skipped` (which has no Lumen worker-close
// meaning) — maps to OutcomeFailed. The mapping is total and case-exact: no
// laundering of an unrecognized value into a success. It is a pure value map, not a
// session-state firewall.
//
// This is STRICTER than the v2 dispatcher's beadOutcomeFailed, which treats a bare
// close as a failure only for beads that opted into gc.on_fail=abort_scope; a Lumen
// do has no such opt-out, so every unrecognized close is fail-closed here. A do that
// must succeed therefore has to stamp gc.outcome=pass explicitly.
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

// LumenFailRetryableForGCOutcome reports whether a work bead's raw gc.outcome VALUE
// is a RETRYABLE failure — an EXPLICIT gc.outcome=fail a worker stamped, which the
// fold's retry arm may re-attempt on a fresh bead (REDESIGN §5). It is the companion
// to LumenOutcomeForGCOutcome, applied by the observe seam onto WorkObservation.Retryable.
//
// A bare close (no gc.outcome) or an unknown token maps to failed too (fail-closed,
// above) but is NOT retryable (MEDIUM-2): a missing/garbled outcome is a definitive
// contract violation, not a transient infrastructure strand, so re-running under a
// retry loop could re-execute work the worker may already have completed. pass and
// degraded are non-failures, so retryability is moot (false). Case-exact, matching
// LumenOutcomeForGCOutcome.
func LumenFailRetryableForGCOutcome(gcOutcome string) bool {
	return gcOutcome == beadmeta.OutcomeFail
}
