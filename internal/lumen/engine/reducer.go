package engine

import (
	"encoding/json"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// Engine is the journal engine tag for every Lumen event. It satisfies the
// journal `engine IN ('lumen','v2','v1')` CHECK.
const Engine = "lumen"

// The provisional closed event vocabulary of the linear Lumen executor. It is
// deliberately three coarse control events — enough to drive and observe a
// linear run end-to-end. A richer per-node-kind vocabulary is later work; these
// are the only (engine, type) pairs the executor appends and the reducer folds.
const (
	// EventRunStarted opens the run: it materializes the root node and seeds the
	// frontier with it.
	EventRunStarted = "lumen.run.started"
	// EventNodeSettled records one executed step's terminal outcome (and, for an
	// exec/settle, its output value).
	EventNodeSettled = "lumen.node.settled"
	// EventRunClosed closes the run with the root's aggregated outcome and clears
	// the frontier.
	EventRunClosed = "lumen.run.closed"
)

// EventTypes is the provisional closed vocabulary in a stable order, for
// registration against the journal store.
var EventTypes = []string{EventRunStarted, EventNodeSettled, EventRunClosed}

// Outcome vocabulary. These mirror the emitted IR / execution-model outcome
// names; the executor only ever produces pass, failed, degraded, or skipped.
const (
	OutcomePass     = "pass"
	OutcomeFailed   = "failed"
	OutcomeDegraded = "degraded"
	OutcomeSkipped  = "skipped"
)

const (
	reducerVersion        = 1
	snapshotFormatVersion = 1
)

// runStartedPayload is the body of an EventRunStarted event.
type runStartedPayload struct {
	RootID    string `json:"root_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
}

// nodeSettledPayload is the body of an EventNodeSettled event.
type nodeSettledPayload struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

// runClosedPayload is the body of an EventRunClosed event.
type runClosedPayload struct {
	Outcome string `json:"outcome"`
}

// lumenReducer is the pure, total fold reducer for the linear executor's
// journal vocabulary. It performs no I/O and reads no clock: every timestamp it
// projects comes from an event payload (the run.started event carries the run's
// created_at, which the reducer threads through node rows). It maps the run
// stream onto a minimal Tier-A projection — a root node plus one node per
// settled step, and a frontier that holds the root while the run is open and is
// empty once it closes.
type lumenReducer struct{}

var _ fold.Reducer = lumenReducer{}

// Engine reports the engine tag.
func (lumenReducer) Engine() string { return Engine }

// ReducerVersion reports the stamped reducer version, bumped on any semantic
// change to the fold.
func (lumenReducer) ReducerVersion() int { return reducerVersion }

// Zero returns the empty fold state. The stream id is read from each event
// rather than carried in state, so resume stays clean at the covered_seq=0
// boundary (mirroring the reference reducer).
func (lumenReducer) Zero(string) fold.State { return &lumenState{} }

// UnmarshalSnapshot deserializes a lumenState blob.
func (lumenReducer) UnmarshalSnapshot(formatVersion int, b []byte) (fold.State, error) {
	if formatVersion != snapshotFormatVersion {
		return nil, fmt.Errorf("lumen: snapshot format %d, want %d", formatVersion, snapshotFormatVersion)
	}
	var s lumenState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("lumen: unmarshal snapshot: %w", err)
	}
	return &s, nil
}

// Apply is the pure transition. It never mutates the input state: it clones,
// mutates the clone, and returns it with the projection delta. Structurally
// impossible sequences (a settlement or close before the run has started)
// return a typed error — journal corruption, never ordinary control flow.
func (lumenReducer) Apply(s fold.State, e fold.Event) (fold.State, fold.Delta, error) {
	prev, ok := s.(*lumenState)
	if !ok {
		return nil, fold.Delta{}, fmt.Errorf("lumen: state is %T, want *lumenState", s)
	}
	next := prev.clone()
	var delta fold.Delta

	switch e.Type {
	case EventRunStarted:
		var p runStartedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fold.Delta{}, fmt.Errorf("lumen: run.started payload at seq %d: %w", e.Seq, err)
		}
		if p.RootID == "" {
			return nil, fold.Delta{}, fmt.Errorf("lumen: run.started at seq %d missing root_id", e.Seq)
		}
		next.RootID = p.RootID
		next.Name = p.Name
		next.CreatedAt = p.CreatedAt
		delta.NodeUpserts = []fold.NodeRow{{
			ID:          p.RootID,
			Title:       p.Name,
			Status:      "open",
			BeadType:    "run",
			CreatedAt:   p.CreatedAt,
			StorageTier: "history",
			StreamID:    e.StreamID,
		}}
		delta.FrontierInsert = []fold.FrontierRow{{
			NodeID:        p.RootID,
			RootID:        e.StreamID,
			ReadyPriority: 2,
			CreatedAt:     p.CreatedAt,
			ID:            p.RootID,
		}}

	case EventNodeSettled:
		if next.RootID == "" {
			return nil, fold.Delta{}, fmt.Errorf("lumen: node.settled at seq %d before run.started", e.Seq)
		}
		var p nodeSettledPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fold.Delta{}, fmt.Errorf("lumen: node.settled payload at seq %d: %w", e.Seq, err)
		}
		if p.ID == "" {
			return nil, fold.Delta{}, fmt.Errorf("lumen: node.settled at seq %d missing id", e.Seq)
		}
		delta.NodeUpserts = []fold.NodeRow{{
			ID:          p.ID,
			Title:       p.ID,
			Status:      statusForOutcome(p.Outcome),
			BeadType:    "step",
			ParentID:    next.RootID,
			CreatedAt:   next.CreatedAt,
			StorageTier: "history",
			StreamID:    e.StreamID,
			Metadata:    map[string]string{"outcome": p.Outcome, "output": p.Output},
		}}

	case EventRunClosed:
		if next.RootID == "" {
			return nil, fold.Delta{}, fmt.Errorf("lumen: run.closed at seq %d before run.started", e.Seq)
		}
		var p runClosedPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fold.Delta{}, fmt.Errorf("lumen: run.closed payload at seq %d: %w", e.Seq, err)
		}
		next.Closed = true
		delta.NodeUpserts = []fold.NodeRow{{
			ID:          next.RootID,
			Title:       next.Name,
			Status:      statusForOutcome(p.Outcome),
			BeadType:    "run",
			CreatedAt:   next.CreatedAt,
			StorageTier: "history",
			StreamID:    e.StreamID,
		}}
		delta.FrontierDelete = []string{next.RootID}

	default:
		return nil, fold.Delta{}, fmt.Errorf("lumen: unknown event type %q at seq %d", e.Type, e.Seq)
	}

	return next, delta, nil
}

// statusForOutcome maps a step outcome onto a node status: a failed outcome is
// a failed node; every other outcome (pass/degraded/skipped) is done.
func statusForOutcome(outcome string) string {
	if outcome == OutcomeFailed {
		return "failed"
	}
	return "done"
}

// lumenState is the reducer's carried-forward state: the run's root identity and
// created_at (both sourced from the run.started payload, keeping the fold
// clock-free) and whether the run has closed.
type lumenState struct {
	RootID    string `json:"root_id"`
	Name      string `json:"name"`
	CreatedAt string `json:"created_at"`
	Closed    bool   `json:"closed"`
}

func (s *lumenState) clone() *lumenState {
	c := *s
	return &c
}

// MarshalSnapshot returns the R-CANON serialization of the state.
func (s *lumenState) MarshalSnapshot() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("lumen: marshal state: %w", err)
	}
	return canon.Canonicalize(raw)
}

// StateHash is the SHA-256 over the canonical serialization.
func (s *lumenState) StateHash() [32]byte {
	b, err := s.MarshalSnapshot()
	if err != nil {
		panic(fmt.Sprintf("lumen: StateHash: %v", err))
	}
	return canon.Hash(b)
}
