package settlementfold

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// mustPayload marshals p to JSON for a test event payload.
func mustPayload(t *testing.T, p any) []byte {
	t.Helper()
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// mixedEngineStream builds one stream interleaving lumen, v2, and v1 facts plus a
// v2 control-epoch fence, dense seq 1..6. It exercises FoldEvents' engine-agnostic
// tolerance — the fold special-cases no engine and never chokes on a foreign
// event type. It is NOT the production topology: production folds a root's
// settlement/<root> stream (v1/v2 + fence) and its lumen run stream <root>
// SEPARATELY, then beads.ProvenanceTimeline groups the two. Putting all three
// engines on one stream here is a fold-mechanics fixture, not a claim that the
// settlement stream carries lumen facts.
func mixedEngineStream(t *testing.T) []fold.Event {
	t.Helper()
	stream := "mixed-test-stream"
	return []fold.Event{
		{
			StreamID: stream, Seq: 1, Engine: "lumen", Type: typeLumenOutcomeSettled,
			Payload: mustPayload(t, map[string]any{"activation": "impl:0", "outcome": "pass"}),
		},
		{
			StreamID: stream, Seq: 2, Engine: "v2", Type: typeSettlementAttempt,
			Payload: mustPayload(t, map[string]any{"root": "gcg-root", "bead": "gcg-log", "outcome": "fail", "attempt": 2}),
		},
		{
			StreamID: stream, Seq: 3, Engine: "v2", Type: typeControlEpochFenced,
			Payload: mustPayload(t, map[string]any{"bead": "gcg-ctl"}),
		},
		{
			StreamID: stream, Seq: 4, Engine: "v1", Type: typeSettlementRoot,
			Payload: mustPayload(t, map[string]any{"root": "gcg-root", "bead": "gcg-root", "outcome": "fail"}),
		},
		{
			StreamID: stream, Seq: 5, Engine: "lumen", Type: typeLumenRunClosed,
			Payload: mustPayload(t, map[string]any{"outcome": "pass"}),
		},
		{
			StreamID: stream, Seq: 6, Engine: "v2", Type: typeSettlementWorkflow,
			Payload: mustPayload(t, map[string]any{"root": "gcg-root", "bead": "gcg-fin", "outcome": "fail"}),
		},
	}
}

// TestFoldEventsMixedEngineTimeline proves the mixed-engine fold: interleaved
// lumen/v2/v1 (+fence) events fold to one seq-ordered, correctly engine-tagged
// timeline, total over every type.
func TestFoldEventsMixedEngineTimeline(t *testing.T) {
	t.Parallel()
	tl, err := FoldEvents(mixedEngineStream(t))
	if err != nil {
		t.Fatalf("FoldEvents: %v", err)
	}
	if len(tl.Facts) != 6 {
		t.Fatalf("timeline has %d facts, want 6: %+v", len(tl.Facts), tl.Facts)
	}
	wantEngine := []string{"lumen", "v2", "v2", "v1", "lumen", "v2"}
	wantType := []string{typeLumenOutcomeSettled, typeSettlementAttempt, typeControlEpochFenced, typeSettlementRoot, typeLumenRunClosed, typeSettlementWorkflow}
	for i, f := range tl.Facts {
		if f.Seq != uint64(i+1) {
			t.Fatalf("fact %d seq = %d, want %d (must be seq-ordered)", i, f.Seq, i+1)
		}
		if f.Engine != wantEngine[i] {
			t.Fatalf("fact %d engine = %q, want %q", i, f.Engine, wantEngine[i])
		}
		if f.Type != wantType[i] {
			t.Fatalf("fact %d type = %q, want %q", i, f.Type, wantType[i])
		}
	}
	// Spot-check payload decode: the attempt fact carries its number, the v1 root
	// its outcome.
	if tl.Facts[1].Attempt != 2 || tl.Facts[1].Bead != "gcg-log" {
		t.Fatalf("attempt fact = %+v, want bead=gcg-log attempt=2", tl.Facts[1])
	}
	if tl.Facts[3].Outcome != "fail" || tl.Facts[3].Root != "gcg-root" {
		t.Fatalf("v1 root fact = %+v, want root=gcg-root outcome=fail", tl.Facts[3])
	}
	// Lumen facts derive their root from the stream id (RootID == StreamID).
	if tl.Facts[4].Root != "mixed-test-stream" {
		t.Fatalf("lumen run.closed fact root = %q, want the stream id", tl.Facts[4].Root)
	}
}

// TestFoldZeroTierADelta is the provenance-only tripwire, structural half: every
// recognized transition folds to an EMPTY Tier-A delta, so no settlement fold can
// create the dual-primary the ADR forbids.
func TestFoldZeroTierADelta(t *testing.T) {
	t.Parallel()
	r := Reducer("v2")
	state := r.Zero("settlement/gcg-root")
	for _, e := range mixedEngineStream(t) {
		// Fold each event through the reducer directly (engine agnostic here) and
		// assert the delta is empty.
		next, delta, err := r.Apply(state, fold.Event{
			StreamID: e.StreamID, Seq: e.Seq, Engine: r.Engine(), Type: e.Type, Payload: e.Payload,
		})
		if err != nil {
			t.Fatalf("Apply(%s): %v", e.Type, err)
		}
		if !deltaEmpty(delta) {
			t.Fatalf("Apply(%s) produced a non-empty Tier-A delta: %+v", e.Type, delta)
		}
		state = next
	}
}

// TestDeltaEmptyGuard proves the tripwire predicate itself detects a Tier-A row,
// so if a future edit made a provenance transition emit one, FoldEvents would
// fail loudly with ErrTierADelta rather than silently projecting it.
func TestDeltaEmptyGuard(t *testing.T) {
	t.Parallel()
	if !deltaEmpty(fold.Delta{}) {
		t.Fatal("deltaEmpty(zero) = false, want true")
	}
	nonEmpty := []fold.Delta{
		{NodeUpserts: []fold.NodeRow{{ID: "x"}}},
		{EdgeUpserts: []fold.EdgeRow{{FromID: "a", ToID: "b"}}},
		{FrontierInsert: []fold.FrontierRow{{NodeID: "x"}}},
		{FrontierDelete: []string{"x"}},
		{CursorUpserts: []fold.CursorRow{{StreamID: "s"}}},
		{WakeupUpserts: []fold.WakeupRow{{NodeID: "x"}}},
		{WakeupDeletes: []string{"x"}},
	}
	for i, d := range nonEmpty {
		if deltaEmpty(d) {
			t.Fatalf("deltaEmpty(non-empty %d) = true, want false", i)
		}
	}
}

// TestFoldTotalOverUnknownAndCorrupt proves totality (R-TOTAL, the P4.5 lesson):
// an unrecognized-but-admitted type folds to a no-op, and a recognized type with
// an undecodable payload still records a minimal fact — never an error that would
// poison the stream forever.
func TestFoldTotalOverUnknownAndCorrupt(t *testing.T) {
	t.Parallel()
	stream := "settlement/gcg-root"
	events := []fold.Event{
		// Unrecognized type: defined no-op, no fact.
		{StreamID: stream, Seq: 1, Engine: "v2", Type: "lumen.node.activated", Payload: []byte(`{"x":1}`)},
		// Recognized type, corrupt payload: minimal fact, no error.
		{StreamID: stream, Seq: 2, Engine: "v2", Type: typeSettlementRoot, Payload: []byte(`{not json`)},
		// Recognized, well-formed.
		{
			StreamID: stream, Seq: 3, Engine: "v1", Type: typeSettlementRoot,
			Payload: mustPayload(t, map[string]any{"root": "gcg-root", "bead": "gcg-root", "outcome": "pass"}),
		},
	}
	tl, err := FoldEvents(events)
	if err != nil {
		t.Fatalf("FoldEvents must be total (never error): %v", err)
	}
	if len(tl.Facts) != 2 {
		t.Fatalf("timeline has %d facts, want 2 (unknown skipped, corrupt+valid recorded)", len(tl.Facts))
	}
	// The corrupt one is a minimal fact: type/engine/seq set, payload fields empty.
	if tl.Facts[0].Seq != 2 || tl.Facts[0].Type != typeSettlementRoot || tl.Facts[0].Root != "" {
		t.Fatalf("corrupt fact = %+v, want minimal {seq=2 type=settlement.root root=empty}", tl.Facts[0])
	}
	if tl.Facts[1].Root != "gcg-root" || tl.Facts[1].Outcome != "pass" {
		t.Fatalf("valid fact = %+v, want root=gcg-root outcome=pass", tl.Facts[1])
	}
}

// TestFoldPureDropRefoldIdentity proves purity + DROP+refold identity: folding
// the same events twice yields byte-identical timelines (StateHash + serialized
// bytes), so a rebuild reproduces provenance deterministically.
func TestFoldPureDropRefoldIdentity(t *testing.T) {
	t.Parallel()
	events := mixedEngineStream(t)

	tl1, err := FoldEvents(events)
	if err != nil {
		t.Fatalf("fold 1: %v", err)
	}
	tl2, err := FoldEvents(events)
	if err != nil {
		t.Fatalf("fold 2 (refold): %v", err)
	}

	if tl1.StateHash() != tl2.StateHash() {
		t.Fatal("refold StateHash differs — fold is not pure/deterministic")
	}
	b1, err := tl1.MarshalSnapshot()
	if err != nil {
		t.Fatalf("marshal 1: %v", err)
	}
	b2, err := tl2.MarshalSnapshot()
	if err != nil {
		t.Fatalf("marshal 2: %v", err)
	}
	if string(b1) != string(b2) {
		t.Fatalf("refold serialization differs:\n %s\n %s", b1, b2)
	}
}

// TestSnapshotRoundTrip proves the Timeline serializes and unmarshals back to the
// same state hash through the reducer's snapshot path.
func TestSnapshotRoundTrip(t *testing.T) {
	t.Parallel()
	r := Reducer("v1")
	tl, err := FoldEvents(mixedEngineStream(t))
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	b, err := tl.MarshalSnapshot()
	if err != nil {
		t.Fatalf("marshal snapshot: %v", err)
	}
	loaded, err := r.UnmarshalSnapshot(snapshotFormatVersion, b)
	if err != nil {
		t.Fatalf("unmarshal snapshot: %v", err)
	}
	if loaded.StateHash() != tl.StateHash() {
		t.Fatal("snapshot round-trip changed the state hash")
	}
	if _, err := r.UnmarshalSnapshot(999, b); err == nil {
		t.Fatal("UnmarshalSnapshot accepted a wrong format version, want error")
	}
}

// TestReducerContract pins the engine tag and reducer version surface used to
// keep RebuildTierA/Resume from confusing the provenance fold with the
// lumen/graph folds.
func TestReducerContract(t *testing.T) {
	t.Parallel()
	for _, engine := range []string{"v1", "v2", "lumen"} {
		r := Reducer(engine)
		if r.Engine() != engine {
			t.Fatalf("Engine() = %q, want %q", r.Engine(), engine)
		}
		if r.ReducerVersion() != reducerVersion {
			t.Fatalf("ReducerVersion() = %d, want %d", r.ReducerVersion(), reducerVersion)
		}
	}
}

// TestErrTierADeltaIsTyped guards that the tripwire sentinel is a distinct,
// matchable error.
func TestErrTierADeltaIsTyped(t *testing.T) {
	t.Parallel()
	if !errors.Is(ErrTierADelta, ErrTierADelta) {
		t.Fatal("ErrTierADelta must be its own sentinel")
	}
}
