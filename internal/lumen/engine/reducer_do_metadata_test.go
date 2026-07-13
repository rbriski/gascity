package engine

import (
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// applyActivatedMeta folds a pool node.activated (with the given metadata map) onto a
// fresh post-run.started state and returns the resulting lumenState.
func applyActivatedMeta(t *testing.T, meta map[string]string) *lumenState {
	t.Helper()
	base := &lumenState{
		RootID:    "gcg-run-meta",
		Name:      "meta",
		CreatedAt: "2026-07-08T00:00:00Z",
		Nodes:     map[string]*nodeState{},
	}
	body, err := canonPayload(nodeActivatedPayload{
		NodeID:       "hello",
		Activation:   "hello:0",
		Kind:         "do",
		DispatchMode: DispatchModePool,
		Route:        "workers",
		Prompt:       "Say hello.",
		Metadata:     meta,
	})
	if err != nil {
		t.Fatalf("canon payload: %v", err)
	}
	next, _, err := lumenReducer{}.Apply(base, fold.Event{
		StreamID: base.RootID,
		Seq:      2,
		Engine:   Engine,
		Type:     EventNodeActivated,
		Payload:  body,
	})
	if err != nil {
		t.Fatalf("Apply node.activated: %v", err)
	}
	return next.(*lumenState)
}

// TestNodeActivatedMetadataFoldTransparent is the reducerVersion-stays-4 pin: the
// node.activated metadata field is payload-only (the Duration precedent). A metadata-
// bearing activation and the SAME activation with no metadata must fold to a
// byte-identical lumenState — identical StateHash — because applyNodeActivated never
// reads it. Mutation (i) — making the reducer fold Metadata into nodeState — diverges
// the two hashes and turns this pin RED, which is exactly when reducerVersion would
// need a bump.
func TestNodeActivatedMetadataFoldTransparent(t *testing.T) {
	withMeta := applyActivatedMeta(t, map[string]string{"gc.continuation_group": "main"})
	without := applyActivatedMeta(t, nil)

	if withMeta.StateHash() != without.StateHash() {
		t.Fatalf("reducer folded node.activated metadata: StateHash diverged (with != without) — reducerVersion would need a bump")
	}
	if got := Reducer().ReducerVersion(); got != 4 {
		t.Fatalf("ReducerVersion() = %d, want 4 (metadata passthrough is payload-only, unfolded)", got)
	}
}
