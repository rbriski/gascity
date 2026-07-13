package engine_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestLumenOutcomeForGCOutcome pins the do-outcome value map: the raw gc.outcome
// value a worker closes with maps to a Lumen outcome, with everything unknown
// (bare/empty/case-variant/control-plane skipped) fail-closed to failed, and only
// the recognized pass/fail plus the Lumen-native degraded and pending passing.
func TestLumenOutcomeForGCOutcome(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pass", engine.OutcomePass},
		{"fail", engine.OutcomeFailed},
		{"degraded", engine.OutcomeDegraded},
		{"pending", engine.OutcomePending},
		{"", engine.OutcomeFailed},
		{"skipped", engine.OutcomeFailed},
		{"shipped", engine.OutcomeFailed},
		{"PASS", engine.OutcomeFailed},
		{"PENDING", engine.OutcomeFailed},
	}
	for _, c := range cases {
		if got := engine.LumenOutcomeForGCOutcome(c.in); got != c.want {
			t.Errorf("LumenOutcomeForGCOutcome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// TestLumenFailRetryableForGCOutcome pins that a pending close is NOT a retryable
// failure (only an explicit gc.outcome=fail is), so a pending poll never triggers a
// retry re-attempt.
func TestLumenFailRetryableForGCOutcome(t *testing.T) {
	for in, want := range map[string]bool{"fail": true, "pass": false, "degraded": false, "pending": false, "": false} {
		if got := engine.LumenFailRetryableForGCOutcome(in); got != want {
			t.Errorf("LumenFailRetryableForGCOutcome(%q) = %v, want %v", in, got, want)
		}
	}
}
