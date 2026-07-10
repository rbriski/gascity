package engine_test

import (
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestLumenOutcomeForGCOutcome pins the do-outcome value map: the raw gc.outcome
// value a worker closes with maps to a Lumen outcome, with everything unknown
// (bare/empty/case-variant/control-plane skipped) fail-closed to failed, and only
// the recognized pass/fail plus the Lumen-native degraded passing.
func TestLumenOutcomeForGCOutcome(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"pass", engine.OutcomePass},
		{"fail", engine.OutcomeFailed},
		{"degraded", engine.OutcomeDegraded},
		{"", engine.OutcomeFailed},
		{"skipped", engine.OutcomeFailed},
		{"shipped", engine.OutcomeFailed},
		{"PASS", engine.OutcomeFailed},
	}
	for _, c := range cases {
		if got := engine.LumenOutcomeForGCOutcome(c.in); got != c.want {
			t.Errorf("LumenOutcomeForGCOutcome(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}
