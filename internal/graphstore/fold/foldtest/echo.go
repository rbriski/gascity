// Package foldtest provides a minimal, deterministic echo Reducer used by the
// fold-driver tests and the Tier-A projection tests. It lives in its own package
// (not a _test.go file) so both the fold package and the graphstore package can
// share one reducer definition — one source of truth for the test fixture. It is
// pure: it imports only the fold contract and the canonical encoder, reads no
// clock, and performs no I/O, so it is a faithful stand-in for a real engine
// reducer under R-PURE.
package foldtest

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// Event types the echo reducer understands. Everything else folds to a no-op
// (R-TOTAL: total over the closed vocabulary, transitions may be no-ops).
const (
	EventNode = "echo.node"
	EventEdge = "echo.edge"
	// EventCursor is a cursor plant/advance event. It projects a channel_cursors
	// row and (when it carries a node + wake_at) a defer_wakeups row, so the
	// DROP+refold byte-identity test (DET-T-17) exercises those two otherwise
	// vacuous Tier-A tables non-trivially.
	EventCursor = "echo.cursor"

	// Engine tags the streams; "lumen" satisfies the journal engine CHECK.
	Engine = "lumen"

	// SnapshotFormatVersion pins the echoState serialization.
	SnapshotFormatVersion = 1

	// ReducerVersion is the echo reducer's stamped version.
	ReducerVersion = 1

	// DefaultCreatedAt is used when a node event omits created_at, keeping the
	// reducer clock-free (R-CLOCK): timestamps come from the payload, never a
	// live clock.
	DefaultCreatedAt = "2020-01-01T00:00:00Z"
)

// EchoReducer maps echo.node / echo.edge events to node / edge / frontier
// deltas, accumulating a running count and frontier set so that snapshot+tail
// resume genuinely reconstructs carried-forward state (R-RESUME is not vacuous).
type EchoReducer struct{}

var _ fold.Reducer = EchoReducer{}

// Engine reports the engine tag.
func (EchoReducer) Engine() string { return Engine }

// ReducerVersion reports the stamped reducer version.
func (EchoReducer) ReducerVersion() int { return ReducerVersion }

// Zero returns the empty state. The streamID argument is part of the Reducer
// contract but is not accumulated fold state for the echo reducer: transitions
// read the owning stream from each event (e.StreamID), so keeping it out of the
// serialized state keeps the resume law (R-RESUME) clean at every split point,
// including the degenerate covered_seq=0 boundary where an empty tail cannot
// supply a stream id.
func (EchoReducer) Zero(string) fold.State { return &echoState{} }

// UnmarshalSnapshot deserializes an echoState blob.
func (EchoReducer) UnmarshalSnapshot(formatVersion int, b []byte) (fold.State, error) {
	if formatVersion != SnapshotFormatVersion {
		return nil, fmt.Errorf("foldtest: snapshot format %d, want %d", formatVersion, SnapshotFormatVersion)
	}
	var s echoState
	if err := json.Unmarshal(b, &s); err != nil {
		return nil, fmt.Errorf("foldtest: unmarshal snapshot: %w", err)
	}
	return &s, nil
}

// Apply is the pure transition. It never mutates the input state: it clones,
// mutates the clone, and returns it alongside the projection delta.
func (EchoReducer) Apply(s fold.State, e fold.Event) (fold.State, fold.Delta, error) {
	prev, ok := s.(*echoState)
	if !ok {
		return nil, fold.Delta{}, fmt.Errorf("foldtest: state is %T, want *echoState", s)
	}
	next := prev.clone()
	var delta fold.Delta

	switch e.Type {
	case EventNode:
		var p struct {
			ID        string `json:"id"`
			Title     string `json:"title"`
			CreatedAt string `json:"created_at"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fold.Delta{}, fmt.Errorf("foldtest: node payload: %w", err)
		}
		if p.ID == "" {
			return nil, fold.Delta{}, fmt.Errorf("foldtest: node event at seq %d missing id", e.Seq)
		}
		created := p.CreatedAt
		if created == "" {
			created = DefaultCreatedAt
		}
		next.Count++
		next.Nodes = append(next.Nodes, p.ID)
		next.addFrontier(p.ID)
		delta.NodeUpserts = []fold.NodeRow{{
			ID:          p.ID,
			Title:       p.Title,
			Status:      "open",
			BeadType:    "task",
			CreatedAt:   created,
			StorageTier: "history",
			StreamID:    e.StreamID,
			// Emit label and metadata child rows so node_labels and node_metadata
			// are projected non-vacuously (DET-T-17). Derived purely from the
			// event, so genesis and rebuild produce identical child sets.
			Labels:   []string{"echo", "node:" + p.ID},
			Metadata: map[string]string{"id": p.ID},
		}}
		delta.FrontierInsert = []fold.FrontierRow{{
			NodeID:        p.ID,
			RootID:        e.StreamID,
			ReadyPriority: 2,
			CreatedAt:     created,
			ID:            p.ID,
		}}

	case EventEdge:
		var p struct {
			From string `json:"from"`
			To   string `json:"to"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fold.Delta{}, fmt.Errorf("foldtest: edge payload: %w", err)
		}
		if p.From == "" || p.To == "" {
			return nil, fold.Delta{}, fmt.Errorf("foldtest: edge event at seq %d missing from/to", e.Seq)
		}
		delta.EdgeUpserts = []fold.EdgeRow{{FromID: p.From, ToID: p.To, DepType: "blocks"}}
		// The target is now blocked by a dependency, so it leaves the frontier.
		if next.removeFrontier(p.To) {
			delta.FrontierDelete = []string{p.To}
		}

	case EventCursor:
		var p struct {
			Reader   string `json:"reader"`
			Node     string `json:"node"`
			Position int64  `json:"position"`
			WakeAt   string `json:"wake_at"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return nil, fold.Delta{}, fmt.Errorf("foldtest: cursor payload: %w", err)
		}
		if p.Reader == "" {
			return nil, fold.Delta{}, fmt.Errorf("foldtest: cursor event at seq %d missing reader", e.Seq)
		}
		delta.CursorUpserts = []fold.CursorRow{{
			StreamID:    e.StreamID,
			Substream:   "",
			ReaderKey:   p.Reader,
			Position:    p.Position,
			PlantedSeq:  p.Position,
			AdvancedSeq: p.Position,
		}}
		// A cursor event may also arm a defer wakeup for a node in this stream, so
		// defer_wakeups is projected non-vacuously (DET-T-17).
		if p.Node != "" && p.WakeAt != "" {
			delta.WakeupUpserts = []fold.WakeupRow{{NodeID: p.Node, WakeAt: p.WakeAt}}
		}

	default:
		// Registered-but-uninteresting event: a no-op transition (R-TOTAL).
	}

	return next, delta, nil
}

// echoState accumulates the reducer's carried-forward state. Nodes preserves
// insertion order; Frontier is kept sorted so the serialization is canonical
// regardless of add/remove interleaving.
type echoState struct {
	Count    int      `json:"count"`
	Nodes    []string `json:"nodes"`
	Frontier []string `json:"frontier"`
}

func (s *echoState) clone() *echoState {
	return &echoState{
		Count:    s.Count,
		Nodes:    append([]string(nil), s.Nodes...),
		Frontier: append([]string(nil), s.Frontier...),
	}
}

func (s *echoState) addFrontier(id string) {
	i := sort.SearchStrings(s.Frontier, id)
	if i < len(s.Frontier) && s.Frontier[i] == id {
		return
	}
	s.Frontier = append(s.Frontier, "")
	copy(s.Frontier[i+1:], s.Frontier[i:])
	s.Frontier[i] = id
}

func (s *echoState) removeFrontier(id string) bool {
	i := sort.SearchStrings(s.Frontier, id)
	if i < len(s.Frontier) && s.Frontier[i] == id {
		s.Frontier = append(s.Frontier[:i], s.Frontier[i+1:]...)
		return true
	}
	return false
}

// MarshalSnapshot returns the R-CANON serialization of the state.
func (s *echoState) MarshalSnapshot() ([]byte, error) {
	raw, err := json.Marshal(s)
	if err != nil {
		return nil, fmt.Errorf("foldtest: marshal state: %w", err)
	}
	return canon.Canonicalize(raw)
}

// StateHash is the SHA-256 over the canonical serialization.
func (s *echoState) StateHash() [32]byte {
	b, err := s.MarshalSnapshot()
	if err != nil {
		// A test reducer with an unserializable state is a programming error.
		panic(fmt.Sprintf("foldtest: StateHash: %v", err))
	}
	return canon.Hash(b)
}
