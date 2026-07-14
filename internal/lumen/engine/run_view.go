package engine

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// RunView is a read-only projection of a Lumen run's journal stream, folded
// through the v4 reducer. It is the per-run, collision-free source of run
// topology and settled status for observability projections (the dashboard run
// view, P5-OBS.4).
//
// It exists because the Tier-A nodes/edges tables cannot serve this: a node
// row's id is the bare, IR-local node id (reducer_state.go's nodeRowFor), so two
// runs of the same formula collide on the global nodes.id primary key and the
// fold upsert rewrites stream_id last-writer-wins (graphstore/projection.go) —
// the L4-BLOCKER "cosmetic observability clobber" documented in reducer.go. A
// journal stream, by contrast, is per-run (its id is a nonce) and append-only,
// so a RunView is stable for the life of the run.
type RunView struct {
	// RootID is the run's stream id (== the root node id).
	RootID string
	// Name is the run.started name (the run title).
	Name string
	// FormulaRef is the run.started formula reference (may be empty).
	FormulaRef string
	// CreatedAt is the run.started timestamp.
	CreatedAt string
	// Driver is "" for a pool/controller run, "self" for a v1 in-session stepper.
	Driver string
	// Closed reports whether the run has sealed.
	Closed bool
	// Outcome is the terminal run outcome once Closed (empty while open).
	Outcome string
	// Activations are the run's activations in deterministic activation-key
	// order (one per node attempt).
	Activations []RunActivationView
}

// RunActivationView is one activation (node:attempt) in a RunView.
type RunActivationView struct {
	// Activation is the full activation key (nodeID + ":" + attempt).
	Activation string
	// NodeID is the bare IR-local node id.
	NodeID string
	// Attempt is the 0-based attempt index parsed from the activation key.
	Attempt int
	// Kind is the node kind (informational; not mapped onto bead gc.kind).
	Kind string
	// ParentActivation is the enclosing activation ("" for a top-level node).
	ParentActivation string
	// After holds the activation keys this node blocks on (dependency edges).
	After []string
	// Members holds the activation keys this node drains (member edges).
	Members []string
	// Settled reports whether this activation has a terminal outcome.
	Settled bool
	// Outcome is the activation's terminal outcome once Settled.
	Outcome string
	// BeadID is the minted work bead id joining to the city work store ("" if
	// the node dispatched no pool bead).
	BeadID string
}

// FoldRunView replays the run's journal stream through the v4 reducer and
// returns a read-only RunView. It writes nothing, opens no lease, and adds no
// event type — reducerVersion is unaffected. It errors when the stream is empty
// or carries no run.started (a stream that never began a run).
func FoldRunView(ctx context.Context, store *graphstore.Store, streamID string) (RunView, error) {
	if store == nil {
		return RunView{}, fmt.Errorf("lumen: fold run view: nil store")
	}
	if streamID == "" {
		return RunView{}, fmt.Errorf("lumen: fold run view: empty stream id")
	}
	stored, err := store.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		return RunView{}, fmt.Errorf("lumen: fold run view %q: %w", streamID, err)
	}
	if len(stored) == 0 {
		return RunView{}, fmt.Errorf("lumen: fold run view %q: empty stream", streamID)
	}

	tail := make([]fold.Event, len(stored))
	for i, e := range stored {
		tail[i] = storedToFoldEvent(e)
	}
	state, _, err := fold.Fold(Reducer(), nil, tail)
	if err != nil {
		return RunView{}, fmt.Errorf("lumen: fold run view %q: fold: %w", streamID, err)
	}
	st, ok := state.(*lumenState)
	if !ok {
		return RunView{}, fmt.Errorf("lumen: fold run view %q: unexpected state %T", streamID, state)
	}
	if st.RootID == "" {
		return RunView{}, fmt.Errorf("lumen: fold run view %q: no run.started in journal", streamID)
	}

	view := RunView{
		RootID:    st.RootID,
		Name:      st.Name,
		CreatedAt: st.CreatedAt,
		Closed:    st.Closed,
		Outcome:   st.Outcome,
	}
	// FormulaRef and Driver live on the run.started payload (seq 1), not on the
	// folded lumenState — decode them from the event we already read.
	var started runStartedPayload
	if err := json.Unmarshal(stored[0].Payload, &started); err != nil {
		return RunView{}, fmt.Errorf("lumen: fold run view %q: run.started payload: %w", streamID, err)
	}
	view.FormulaRef = started.FormulaRef
	view.Driver = started.Driver

	for _, act := range st.activationKeys() {
		n := st.Nodes[act]
		if n == nil {
			continue
		}
		view.Activations = append(view.Activations, RunActivationView{
			Activation:       act,
			NodeID:           n.NodeID,
			Attempt:          activationAttempt(act),
			Kind:             n.Kind,
			ParentActivation: n.ParentActivation,
			After:            append([]string(nil), n.After...),
			Members:          append([]string(nil), n.Members...),
			Settled:          n.Settled,
			Outcome:          n.Outcome,
			BeadID:           n.BeadID,
		})
	}
	return view, nil
}
