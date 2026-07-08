// Package settlementfold folds ONE journal stream into a seq-ordered list of
// coarse settlement facts (P5.4). It is engine-agnostic: it folds whatever a
// single stream carries, tagging each fact with its own event's engine. A root's
// settlement/<root> stream genuinely mixes v1 and v2 facts (plus the v2
// control-epoch fence) — a root can settle via a v2 finalize and a v1 autoclose —
// while a lumen root's run stream carries the lumen fine-grained terminal facts.
// The UNIFIED provenance "across all three engines" is assembled one layer up, by
// beads.ProvenanceTimeline folding BOTH of a root's streams (this package folds
// each; ProvenanceTimeline groups them per-stream). This is the read-side
// complement of the coarse settlement events the dispatcher and the cmd-side v1
// closers emit (internal/beads/settlement_events.go, internal/dispatch/settlement.go).
//
// PROVENANCE ONLY — ZERO Tier-A. The v1/v2 engines stay mutation-primary: their
// projection of record is the existing gc.outcome column write, never a fold of
// these events. So every transition here returns an EMPTY fold.Delta; a fold that
// upserted a nodes/edges/frontier row would create exactly the dual-primary the
// ADR forbids. FoldEvents tripwires that invariant at runtime, and the package
// tests pin it. Nothing on the write path folds settlement streams into Tier-A
// (RebuildTierA/Resume are only ever driven with the lumen reducer over lumen
// run streams); this reducer exists for the read surface and as a well-defined,
// zero-delta safety net if a settlement stream is ever handed to RebuildTierA.
//
// TOTAL + PURE. The fold is total (R-TOTAL, the P4.5 reducer-totality lesson): it
// never errors on any event the append layer admits — a recognized type whose
// payload this binary cannot decode still records a minimal fact rather than
// poisoning the stream forever, and an unrecognized type folds to a defined
// no-op. It is pure (R-PURE): no clock, no rand, no map-order dependence, so a
// DROP+refold reproduces the timeline byte-identically. Time provenance is the
// journal's appended_at framing column (read separately if ever needed), never a
// payload or a live clock.
package settlementfold

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// Recognized settlement-relevant event types across the three engines. They are
// kept as local string constants — matching the wire contract strings minted by
// beads.Settlement*Type, beads.controlFenceEventType, and the lumen terminal
// vocabulary — so this pure package imports neither internal/beads nor
// internal/lumen (both of which pull database/sql via graphstore). The strings
// are the stable on-wire contract; a divergence would simply stop mapping an
// event to a fact (a no-op), never a panic.
const (
	typeSettlementRoot      = "settlement.root"
	typeSettlementAttempt   = "settlement.attempt"
	typeSettlementWorkflow  = "settlement.workflow.finalized"
	typeControlEpochFenced  = "control.epoch.fenced"
	typeLumenOutcomeSettled = "lumen.outcome.settled"
	typeLumenRunClosed      = "lumen.run.closed"
)

const (
	// reducerVersion is the provenance reducer's stamped version, bumped on any
	// semantic change to the fold. It is independent of the v1/v2/lumen execution
	// reducers.
	reducerVersion = 1
	// snapshotFormatVersion pins the Timeline serialization layout.
	snapshotFormatVersion = 1
)

// ErrTierADelta is the loud tripwire signal: a settlement event folded to a
// non-empty Tier-A delta. It can only fire on a programming error (a future edit
// that made a provenance transition emit nodes/edges/frontier), and FoldEvents
// fails on it rather than silently creating a dual-primary projection.
var ErrTierADelta = errors.New("settlementfold: settlement fold produced a Tier-A delta")

// SettlementFact is one entry in a root's provenance timeline: a single coarse
// settlement/lifecycle transition, tagged with the engine that emitted it and the
// dense stream seq that orders it. It is derived purely from a committed journal
// event; no field is a live clock.
type SettlementFact struct {
	Seq     uint64 `json:"seq"`
	Engine  string `json:"engine"`
	Type    string `json:"type"`
	Root    string `json:"root,omitempty"`
	Bead    string `json:"bead,omitempty"`
	Kind    string `json:"kind,omitempty"`
	Outcome string `json:"outcome,omitempty"`
	Attempt int    `json:"attempt,omitempty"`
}

// Timeline is the fold state: the ordered settlement facts of one root's stream.
// It is a fold.State — its serialization is canonical (facts carried in seq
// order, no maps) so StateHash gates a snapshot and two folds over the same
// events produce a byte-identical Timeline.
type Timeline struct {
	Facts []SettlementFact `json:"facts"`
}

func (t *Timeline) clone() *Timeline {
	return &Timeline{Facts: append([]SettlementFact(nil), t.Facts...)}
}

// MarshalSnapshot returns the R-CANON serialization of the timeline.
func (t *Timeline) MarshalSnapshot() ([]byte, error) {
	raw, err := json.Marshal(t)
	if err != nil {
		return nil, fmt.Errorf("settlementfold: marshal timeline: %w", err)
	}
	return canon.Canonicalize(raw)
}

// StateHash is the SHA-256 over the canonical serialization.
func (t *Timeline) StateHash() [32]byte {
	b, err := t.MarshalSnapshot()
	if err != nil {
		// A timeline of JSON-marshalable facts cannot fail to serialize; a failure
		// is a programming error, not a runtime condition.
		panic(fmt.Sprintf("settlementfold: StateHash: %v", err))
	}
	return canon.Hash(b)
}

// --- typed decode structs (mirror the on-wire payloads) ----------------------

// settlementPayload mirrors beads.SettlementPayload's wire shape.
type settlementPayload struct {
	Root     string `json:"root"`
	Bead     string `json:"bead"`
	Kind     string `json:"kind,omitempty"`
	Outcome  string `json:"outcome"`
	Attempt  int    `json:"attempt,omitempty"`
	StoreRef string `json:"store_ref,omitempty"`
}

// fencePayload mirrors beads.controlFencePayload's wire shape.
type fencePayload struct {
	Bead string `json:"bead"`
}

// lumenOutcomePayload mirrors the fields of the lumen outcome.settled payload the
// timeline surfaces (activation + outcome).
type lumenOutcomePayload struct {
	Activation string `json:"activation"`
	Outcome    string `json:"outcome"`
}

// lumenRunClosedPayload mirrors the lumen run.closed outcome field.
type lumenRunClosedPayload struct {
	Outcome string `json:"outcome"`
}

// factForEvent maps one journal event to a provenance fact. It returns
// recognized=false for any type outside the settlement-relevant vocabulary (a
// defined no-op, keeping the fold total). For a recognized type it NEVER errors:
// a payload this binary cannot decode still yields a minimal fact (Seq/Engine/
// Type set, decoded fields zero) rather than poisoning the fold — the P4.5
// totality discipline.
func factForEvent(e fold.Event) (SettlementFact, bool) {
	base := SettlementFact{Seq: e.Seq, Engine: e.Engine, Type: e.Type}
	switch e.Type {
	case typeSettlementRoot, typeSettlementAttempt, typeSettlementWorkflow:
		var p settlementPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			base.Root, base.Bead, base.Kind, base.Outcome, base.Attempt = p.Root, p.Bead, p.Kind, p.Outcome, p.Attempt
		}
		return base, true
	case typeControlEpochFenced:
		var p fencePayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			base.Root, base.Bead = p.Bead, p.Bead
		}
		return base, true
	case typeLumenOutcomeSettled:
		var p lumenOutcomePayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			base.Bead, base.Outcome = p.Activation, p.Outcome
		}
		// A lumen run stream is keyed by its run/root id (engine.go: RootID ==
		// StreamID), so the fact's root is the stream itself.
		base.Root = e.StreamID
		return base, true
	case typeLumenRunClosed:
		var p lumenRunClosedPayload
		if err := json.Unmarshal(e.Payload, &p); err == nil {
			base.Outcome = p.Outcome
		}
		base.Root, base.Bead = e.StreamID, e.StreamID
		return base, true
	default:
		return SettlementFact{}, false
	}
}

// apply is the shared pure transition behind both the fold.Reducer (single-engine,
// for RebuildTierA-safety and the tripwire) and FoldEvents (mixed-engine, for the
// read surface). It appends a fact for a recognized event and always returns an
// EMPTY Tier-A delta.
func apply(s fold.State, e fold.Event) (fold.State, fold.Delta, error) {
	prev, ok := s.(*Timeline)
	if !ok {
		return nil, fold.Delta{}, fmt.Errorf("settlementfold: state is %T, want *Timeline", s)
	}
	next := prev.clone()
	if fact, recognized := factForEvent(e); recognized {
		next.Facts = append(next.Facts, fact)
	}
	return next, fold.Delta{}, nil
}

// FoldEvents folds a settlement stream's events into a provenance Timeline.
//
// Unlike fold.Fold it does NOT enforce a single engine per stream: a unified
// settlement stream deliberately carries interleaved lumen / v2 / v1 (and v2
// control-epoch-fence) rows, each fact tagged with its own event's engine. This
// is the read-side driver behind beads.ProvenanceTimeline.
//
// It tripwires the provenance-only invariant: every transition must fold to an
// EMPTY Tier-A delta; a non-empty delta fails loudly with ErrTierADelta rather
// than letting a settlement fold silently touch nodes/edges/frontier. Events are
// expected in seq order (as ReadStream returns them); the timeline preserves that
// order.
func FoldEvents(events []fold.Event) (*Timeline, error) {
	var state fold.State = &Timeline{}
	for _, e := range events {
		next, delta, err := apply(state, e)
		if err != nil {
			return nil, err
		}
		if !deltaEmpty(delta) {
			return nil, fmt.Errorf("settlementfold: (%s, %s) at seq %d: %w", e.Engine, e.Type, e.Seq, ErrTierADelta)
		}
		state = next
	}
	return state.(*Timeline), nil
}

// deltaEmpty reports whether d carries no Tier-A rows at all — the invariant every
// provenance transition must satisfy. It enumerates EVERY field of fold.Delta and
// must be kept in lockstep with that struct: a field added to fold.Delta but not
// listed here would let a real Tier-A row slip past the tripwire. TestDeltaEmptyGuard
// exercises one non-empty delta per field to pin the current field set.
func deltaEmpty(d fold.Delta) bool {
	return len(d.NodeUpserts) == 0 && len(d.EdgeUpserts) == 0 &&
		len(d.FrontierInsert) == 0 && len(d.FrontierDelete) == 0 &&
		len(d.CursorUpserts) == 0 && len(d.WakeupUpserts) == 0 && len(d.WakeupDeletes) == 0
}

// --- fold.Reducer (engine-tagged, for RebuildTierA-safety + the tripwire) -----

// provenanceReducer is the engine-tagged fold.Reducer over a settlement stream.
// Its Engine() lets RebuildTierA/Resume never confuse it with the lumen/graph
// folds, and its Apply always returns an empty delta — so if a settlement stream
// were ever handed to RebuildTierA with this reducer, the rebuild would be a
// well-defined no-op rather than corrupting Tier-A. Nothing on the write path
// does that (settlement streams are never folded); the mixed-engine read uses
// FoldEvents, which is why the standard fold.Fold single-engine driver is not the
// timeline entry point.
type provenanceReducer struct{ engine string }

var _ fold.Reducer = provenanceReducer{}

// Reducer returns the engine-tagged provenance reducer (engine is "v1" or "v2",
// or "lumen" for a run stream). It is total, pure, and emits zero Tier-A deltas.
func Reducer(engine string) fold.Reducer { return provenanceReducer{engine: engine} }

// Engine reports the reducer's engine tag.
func (r provenanceReducer) Engine() string { return r.engine }

// ReducerVersion reports the stamped provenance reducer version.
func (provenanceReducer) ReducerVersion() int { return reducerVersion }

// Zero returns the empty timeline.
func (provenanceReducer) Zero(string) fold.State { return &Timeline{} }

// UnmarshalSnapshot deserializes a Timeline blob.
func (provenanceReducer) UnmarshalSnapshot(formatVersion int, b []byte) (fold.State, error) {
	if formatVersion != snapshotFormatVersion {
		return nil, fmt.Errorf("settlementfold: snapshot format %d, want %d", formatVersion, snapshotFormatVersion)
	}
	var t Timeline
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, fmt.Errorf("settlementfold: unmarshal snapshot: %w", err)
	}
	return &t, nil
}

// Apply is the pure, total, zero-delta transition.
func (provenanceReducer) Apply(s fold.State, e fold.Event) (fold.State, fold.Delta, error) {
	return apply(s, e)
}
