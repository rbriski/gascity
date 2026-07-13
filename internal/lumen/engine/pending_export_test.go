package engine

// This file exposes a test-only knob for the physical-cap harness (pending_test.go,
// package engine_test). Because it is a _test.go file it compiles ONLY into the test
// binary — it adds NOTHING to the production API. lumenLoopPhysicalCap is 512 in
// production and never reassigned there; this setter is its sole test-time writer.

// SetLoopPhysicalCapForTest shrinks lumenLoopPhysicalCap so a focused test can exercise
// the poll_cap bound without minting 512 attempts, and returns a restore func the caller
// must defer. It exists only in the test binary.
func SetLoopPhysicalCapForTest(n int) func() {
	prev := lumenLoopPhysicalCap
	lumenLoopPhysicalCap = n
	return func() { lumenLoopPhysicalCap = prev }
}

// LoopPhysicalCap reports the current physical cap (default 512) for tests that assert
// the production sizing.
func LoopPhysicalCap() int { return lumenLoopPhysicalCap }

// ConsumingCountBeforeForTest exposes the pure consuming-count core to the internal
// reducer test so the consuming-vs-physical semantics can be asserted over a
// constructed fold-node map without standing up a driver.
func ConsumingCountBeforeForTest(nodes map[string]NodeStateForTest, bodyNodeID string, k int) int {
	m := make(map[string]*nodeState, len(nodes))
	for act, ns := range nodes {
		m[act] = &nodeState{Settled: ns.Settled, Outcome: ns.Outcome}
	}
	return consumingCountBeforeIn(m, bodyNodeID, k)
}

// NodeStateForTest is the minimal settled-attempt shape ConsumingCountBeforeForTest
// folds — just the fields consumingCountBeforeIn reads.
type NodeStateForTest struct {
	Settled bool
	Outcome string
}
