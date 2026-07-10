package engine_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// retryLane renders a retry loop (distinct loop + body ids) over an exec body, for
// use as a scatter member — the mol-review-quorum lane shape.
func retryLane(loopID, bodyID, attempts, script string, retryable []int) string {
	body := execNodeExit(bodyID, script, []int{0}, retryable)
	return `{"kind":"retry","id":"` + loopID + `","name":"` + loopID + `","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},` +
		`"attempts":{"kind":"literal","value":` + attempts + `},"body":` + body + `}`
}

// TestRetryInScatterDrivesBothLanes (RN) proves two retry loops drive as scatter
// members and the scatter aggregates their outcomes: lane r1 passes on attempt 0;
// lane r2 retryable-fails to its budget (2 attempts) and exhausts; under
// on_fail=continue the scatter settles degraded (one ran-pass + one failed). The
// r2 retry actually CYCLED under the scatter (its 2 attempt.minted prove it).
func TestRetryInScatterDrivesBothLanes(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rn",
		scatterNode("lanes", nil, "continue",
			retryLane("r1", "b1", "2", "echo ok", nil),      // passes attempt 0
			retryLane("r2", "b2", "2", "exit 1", []int{1})), // retryable-fails, exhausts at budget 2
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	o1, _, _, _ := loopSettle(t, res.Events, "r1:0")
	if o1 != engine.OutcomePass {
		t.Errorf("lane r1 loop = %q, want pass", o1)
	}
	o2, reason2, rem2, _ := loopSettle(t, res.Events, "r2:0")
	if o2 != engine.OutcomeFailed || reason2 != "exhausted" {
		t.Errorf("lane r2 loop = {%q, %q}, want {failed, exhausted}", o2, reason2)
	}
	if rem2 == nil || *rem2 != 0 {
		t.Errorf("lane r2 retries_remaining = %v, want 0 (exhausted under the scatter)", rem2)
	}

	// r2 retried under the scatter (2 attempts); r1 ran once → 3 total attempt.minted.
	if n := countAttemptMinted(res.Events); n != 3 {
		t.Errorf("attempt.minted = %d, want 3 (r1:1 + r2:2 — the retry cycled under the scatter)", n)
	}

	// The scatter aggregated the two loop members: one pass + one failed under
	// on_fail=continue ⇒ degraded.
	assertSettled(t, settledIDs(t, res.Events), "lanes", engine.OutcomeDegraded)
}

// TestRetryInScatterBothPassSeals proves the happy path: two passing retry lanes
// under a scatter both settle pass and the scatter (hence the run) seals pass.
func TestRetryInScatterBothPassSeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rn2",
		scatterNode("lanes", nil, "continue",
			retryLane("r1", "b1", "3", "echo a", nil),
			retryLane("r2", "b2", "3", "echo b", nil)),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	assertSettled(t, settledIDs(t, res.Events), "lanes", engine.OutcomePass)
}

// TestRetryInScatterDropRefoldByteIdentity pins DET-T-17 for a retry-in-scatter:
// the live projection (attempt rows bodyID:0..N under a scatter member) equals a
// from-scratch drop+refold — the reducer folds no hidden state (reducerVersion 3).
func TestRetryInScatterDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("rn3",
		scatterNode("lanes", nil, "continue",
			retryLane("r1", "b1", "2", "echo a", nil),
			retryLane("r2", "b2", "2", "exit 1", []int{1})),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}
