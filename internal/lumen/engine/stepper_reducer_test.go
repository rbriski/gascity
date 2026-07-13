package engine

import (
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// applyRunStartedDriver folds a run.started carrying the given driver discriminator
// onto a zero state and returns the resulting lumenState.
func applyRunStartedDriver(t *testing.T, driver string) *lumenState {
	t.Helper()
	body, err := canonPayload(runStartedPayload{
		RootID:    "gcg-run-driver",
		Name:      "driver",
		CreatedAt: "2026-07-13T00:00:00Z",
		Driver:    driver,
	})
	if err != nil {
		t.Fatalf("canon payload: %v", err)
	}
	next, _, err := lumenReducer{}.Apply(&lumenState{}, fold.Event{
		StreamID: "gcg-run-driver",
		Seq:      1,
		Engine:   Engine,
		Type:     EventRunStarted,
		Payload:  body,
	})
	if err != nil {
		t.Fatalf("Apply run.started: %v", err)
	}
	return next.(*lumenState)
}

// TestRunStartedDriverFoldTransparent is the reducerVersion-stays-4 pin for the v1
// stepper's Driver discriminator: the run.started `driver` field is payload-only
// (the DefaultRoute precedent). A run.started stamped Driver="self" and the SAME
// run.started with no driver must fold to a byte-identical lumenState — identical
// StateHash — because applyRunStarted never reads it. Mutation (ii) — folding Driver
// into lumenState/nodeState — diverges the two hashes and turns this pin RED, which
// is exactly when reducerVersion would need a bump.
func TestRunStartedDriverFoldTransparent(t *testing.T) {
	self := applyRunStartedDriver(t, "self")
	pool := applyRunStartedDriver(t, "")

	if self.StateHash() != pool.StateHash() {
		t.Fatalf("reducer folded run.started driver: StateHash diverged (self != pool) — reducerVersion would need a bump")
	}
	if got := Reducer().ReducerVersion(); got != 4 {
		t.Fatalf("ReducerVersion() = %d, want 4 (Driver discriminator is payload-only, unfolded)", got)
	}
}
