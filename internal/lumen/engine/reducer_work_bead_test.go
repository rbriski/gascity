package engine

import (
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// poolNodeState returns a fold state carrying one activated, ready, unsettled
// pool-mode do node ("do:0") — the shape the real-bead dispatch arm folds over.
func poolNodeState() *lumenState {
	return &lumenState{
		RootID:    "gcg-run-wb",
		Name:      "wb",
		CreatedAt: "2026-07-08T00:00:00Z",
		Nodes: map[string]*nodeState{
			"do:0": {
				NodeID:       "do",
				Kind:         "do",
				DispatchMode: DispatchModePool,
				Route:        "workers",
				Prompt:       "greet the user",
				InFrontier:   true,
			},
		},
	}
}

// foldWorkBeadAdmit folds an owned.admitted event with the given payload through
// the reducer and returns the resulting state + delta.
func foldWorkBeadAdmit(t *testing.T, s *lumenState, p ownedAdmittedPayload) (*lumenState, fold.Delta) {
	t.Helper()
	body, err := canonPayload(p)
	if err != nil {
		t.Fatalf("canon payload: %v", err)
	}
	next, delta, err := lumenReducer{}.Apply(s, fold.Event{
		StreamID: s.RootID,
		Seq:      2,
		Engine:   Engine,
		Type:     EventOwnedAdmitted,
		Payload:  body,
	})
	if err != nil {
		t.Fatalf("Apply owned.admitted: %v", err)
	}
	return next.(*lumenState), delta
}

// TestReducerWorkBeadDispatchArm pins the real-bead dispatch fold (REDESIGN §1.3):
// owned.admitted{kind:work_bead, bead_id} records BeadID on a ready pool node, drops
// it out of the claimable frontier, re-projects it as a PLAIN step (no assignee, no
// dispatch_mode / routed_to marker), and carries only the bead_id join key.
func TestReducerWorkBeadDispatchArm(t *testing.T) {
	s := poolNodeState()
	next, delta := foldWorkBeadAdmit(t, s, ownedAdmittedPayload{
		Handle:     "do:0",
		Activation: "do:0",
		Kind:       OwnedKindWorkBead,
		BeadID:     "gc-42",
	})

	n := next.Nodes["do:0"]
	if n.BeadID != "gc-42" {
		t.Fatalf("BeadID = %q, want gc-42", n.BeadID)
	}
	if n.InFrontier {
		t.Errorf("InFrontier = true, want false (dispatch drops the claimable frontier row)")
	}

	// Frontier row is deleted by the bare node id.
	if len(delta.FrontierDelete) != 1 || delta.FrontierDelete[0] != "do" {
		t.Errorf("FrontierDelete = %v, want [do]", delta.FrontierDelete)
	}
	if len(delta.FrontierInsert) != 0 {
		t.Errorf("FrontierInsert = %v, want none", delta.FrontierInsert)
	}

	// Projected row is a plain step, not a claimable task; prompt is NOT copied
	// (the real bead in the work store carries it).
	if len(delta.NodeUpserts) != 1 {
		t.Fatalf("NodeUpserts = %d rows, want 1", len(delta.NodeUpserts))
	}
	row := delta.NodeUpserts[0]
	if row.BeadType != "step" {
		t.Errorf("projected bead_type = %q, want step", row.BeadType)
	}
	if row.Description != "" {
		t.Errorf("projected description = %q, want empty", row.Description)
	}
	if row.Assignee != "" {
		t.Errorf("projected assignee = %q, want empty", row.Assignee)
	}
	if got := row.Metadata["bead_id"]; got != "gc-42" {
		t.Errorf("projected bead_id meta = %q, want gc-42", got)
	}
	if _, ok := row.Metadata["dispatch_mode"]; ok {
		t.Errorf("projected dispatch_mode present, want dropped on the real-bead path")
	}
	if _, ok := row.Metadata["gc.routed_to"]; ok {
		t.Errorf("projected gc.routed_to present, want dropped on the real-bead path")
	}
}

// TestReducerWorkBeadDispatchTotality pins R-TOTAL: a dispatch fact that cannot
// take effect folds to a DEFINED no-op (never an error that poisons RebuildTierA),
// and re-dispatching an already-dispatched node is idempotent.
func TestReducerWorkBeadDispatchTotality(t *testing.T) {
	cases := []struct {
		name  string
		mut   func(*lumenState)
		p     ownedAdmittedPayload
		field string // expected BeadID on "do:0" after fold ("" = untouched)
	}{
		{
			name:  "empty bead id",
			p:     ownedAdmittedPayload{Handle: "do:0", Activation: "do:0", Kind: OwnedKindWorkBead, BeadID: ""},
			field: "",
		},
		{
			name:  "absent handle",
			p:     ownedAdmittedPayload{Handle: "missing:0", Activation: "missing:0", Kind: OwnedKindWorkBead, BeadID: "gc-1"},
			field: "",
		},
		{
			name:  "non-pool handle",
			mut:   func(s *lumenState) { s.Nodes["do:0"].DispatchMode = "" },
			p:     ownedAdmittedPayload{Handle: "do:0", Activation: "do:0", Kind: OwnedKindWorkBead, BeadID: "gc-1"},
			field: "",
		},
		{
			name:  "already settled",
			mut:   func(s *lumenState) { s.Nodes["do:0"].Settled = true },
			p:     ownedAdmittedPayload{Handle: "do:0", Activation: "do:0", Kind: OwnedKindWorkBead, BeadID: "gc-1"},
			field: "",
		},
		{
			name:  "already dispatched (idempotent)",
			mut:   func(s *lumenState) { s.Nodes["do:0"].BeadID = "gc-first" },
			p:     ownedAdmittedPayload{Handle: "do:0", Activation: "do:0", Kind: OwnedKindWorkBead, BeadID: "gc-second"},
			field: "gc-first", // the first-recorded id stands
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := poolNodeState()
			if tc.mut != nil {
				tc.mut(s)
			}
			next, delta := foldWorkBeadAdmit(t, s, tc.p)
			if got := next.Nodes["do:0"].BeadID; got != tc.field {
				t.Errorf("BeadID = %q, want %q", got, tc.field)
			}
			if len(delta.NodeUpserts) != 0 || len(delta.FrontierDelete) != 0 || len(delta.FrontierInsert) != 0 {
				t.Errorf("no-op fold emitted a delta: %+v", delta)
			}
		})
	}
}

// TestReducerVersionBumpStrandsOldSnapshot pins that a stale-version snapshot is
// stranded LOUDLY through the fold version gate (never a silent best-effort fold) —
// the property that makes each bump (2→3 BeadID, 3→4 Detail) free on this branch.
func TestReducerVersionBumpStrandsOldSnapshot(t *testing.T) {
	stale := &fold.Snapshot{
		StreamID:              "gcg-run-wb",
		CoveredSeq:            1,
		Engine:                Engine,
		ReducerVersion:        3, // pre-recover (Detail-fold) reducer
		SnapshotFormatVersion: snapshotFormatVersion,
		State:                 []byte("{}"),
	}
	_, _, err := fold.Fold(Reducer(), stale, nil)
	if !errors.Is(err, fold.ErrReducerVersionSkew) {
		t.Fatalf("Fold of a v3 snapshot under the v4 reducer = %v, want ErrReducerVersionSkew", err)
	}
	if got := Reducer().ReducerVersion(); got != 4 {
		t.Fatalf("ReducerVersion() = %d, want 4", got)
	}
}
