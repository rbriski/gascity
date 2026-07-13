package engine

import (
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// This file exposes a test-only fold comparator to the external stepper harness
// (stepper_test.go, package engine_test). It is a _test.go file, so it compiles ONLY
// into the test binary and adds nothing to the production API.

// NormalizedFoldHashForTest folds a full journal (from seq 1) to its terminal
// lumenState, blanks the run-identity fields (RootID, CreatedAt) that NECESSARILY differ
// between two independent runs (the stream nonce and the wall-clock enqueue stamp), and
// returns the canonical StateHash over what remains — the DAG of settled activations.
//
// It is the determinism-oracle comparator: a v1 stepper run and an engine.Run scripting
// the same do outcomes fold to the SAME normalized hash, because the reducer's Apply is a
// pure function of (state, event) that never reads WHO appended an event, and the only
// per-run provenance the stepper carries (the effect.settled session ref) folds to a
// no-op and never enters the state.
func NormalizedFoldHashForTest(t *testing.T, events []graphstore.StoredEvent) [32]byte {
	t.Helper()
	fev := make([]fold.Event, len(events))
	for i, e := range events {
		fev[i] = storedToFoldEvent(e)
	}
	state, _, err := fold.Fold(lumenReducer{}, nil, fev)
	if err != nil {
		t.Fatalf("NormalizedFoldHashForTest: fold: %v", err)
	}
	ls, ok := state.(*lumenState)
	if !ok {
		t.Fatalf("NormalizedFoldHashForTest: state is %T, want *lumenState", state)
	}
	ls.RootID = ""
	ls.CreatedAt = ""
	return ls.StateHash()
}
