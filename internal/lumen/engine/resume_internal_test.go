package engine

import "testing"

// TestReconstructOutputsPicksMaxNumericAttempt (T-K2) pins the S6 fix: a node
// re-attempted by a retry/repeat loop reuses ONE bare node id across activations
// b:0…b:N, and its authoritative output is the highest-numbered attempt's.
// activationKeys() is LEXICOGRAPHIC ("b:10" < "b:2"), so a plain last-write-wins
// walk seeds the WRONG attempt once a node has more than ten attempts (the loop
// cap is 32). reconstructOutputs must order by the numeric attempt suffix and let
// the max attempt win. This test FAILS on the pre-L5a lexicographic ordering.
func TestReconstructOutputsPicksMaxNumericAttempt(t *testing.T) {
	s := &lumenState{
		RootID: "gcg-run-x",
		Nodes: map[string]*nodeState{
			"b:2":  {NodeID: "b", Kind: "exec", Settled: true, Outcome: OutcomePass, Output: "two"},
			"b:10": {NodeID: "b", Kind: "exec", Settled: true, Outcome: OutcomePass, Output: "ten"},
		},
	}

	nodeOutputs, scope := reconstructOutputs(s)

	if got := scope["b"]; got != "ten" {
		t.Errorf("scope[b] = %q, want ten (attempt :10, the max numeric attempt — not :2 by lexical order)", got)
	}
	if got := nodeOutputs["b"]; got != "ten" {
		t.Errorf("nodeOutputs[b] = %q, want ten (the highest-numbered attempt)", got)
	}
}
