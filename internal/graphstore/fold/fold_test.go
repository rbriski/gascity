package fold_test

import (
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/graphstore/fold/foldtest"
)

const stream = "gcj-root-fold"

// registerNodeV0Upcaster registers the test upcaster exactly once. The upcaster
// registry is process-global and rejects duplicate keys (a production guard), so
// re-running under -count=N must not re-register.
var registerNodeV0Upcaster = sync.OnceFunc(func() {
	fold.RegisterUpcaster(foldtest.Engine, "echo.node.v0", "ir-0",
		func(_, _ string, payload []byte) (string, string, []byte, error) {
			var p struct {
				Node string `json:"node"`
			}
			if err := json.Unmarshal(payload, &p); err != nil {
				return "", "", nil, err
			}
			out, err := canon.Canonicalize([]byte(fmt.Sprintf(`{"id":%q}`, p.Node)))
			if err != nil {
				return "", "", nil, err
			}
			return foldtest.EventNode, "ir-1", out, nil
		})
})

// nodeEvent builds a canonical echo.node event at seq.
func nodeEvent(t *testing.T, seq uint64, id string) fold.Event {
	t.Helper()
	return event(t, seq, foldtest.EventNode, fmt.Sprintf(`{"id":%q,"title":%q}`, id, "T-"+id))
}

func edgeEvent(t *testing.T, seq uint64, from, to string) fold.Event {
	t.Helper()
	return event(t, seq, foldtest.EventEdge, fmt.Sprintf(`{"from":%q,"to":%q}`, from, to))
}

func event(t *testing.T, seq uint64, typ, raw string) fold.Event {
	t.Helper()
	p, err := canon.Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("canonicalize %q: %v", raw, err)
	}
	return fold.Event{StreamID: stream, Seq: seq, Engine: foldtest.Engine, Type: typ, Payload: p}
}

// corpus is a small mixed stream: nodes n1..n4 with two edges that block n2 and
// n4 out of the frontier. It exercises accumulating state (count + frontier set)
// so the resume law is not vacuous.
func corpus(t *testing.T) []fold.Event {
	t.Helper()
	return []fold.Event{
		nodeEvent(t, 1, "n1"),
		nodeEvent(t, 2, "n2"),
		edgeEvent(t, 3, "n1", "n2"),
		nodeEvent(t, 4, "n3"),
		nodeEvent(t, 5, "n4"),
		edgeEvent(t, 6, "n3", "n4"),
	}
}

// TestResumeLaw_R_RESUME proves R-RESUME/DET-T-20: for every split point k,
// folding a snapshot-at-k plus the tail after k yields the same final state
// (StateHash) and the same concatenated Deltas as a single genesis fold. A
// snapshot at any k is semantically invisible.
func TestResumeLaw_R_RESUME(t *testing.T) {
	r := foldtest.EchoReducer{}
	events := corpus(t)

	fullState, fullDeltas, err := fold.Fold(r, nil, events)
	if err != nil {
		t.Fatalf("genesis fold: %v", err)
	}
	fullHash := fullState.StateHash()

	for k := 0; k <= len(events); k++ {
		midState, headDeltas, err := fold.Fold(r, nil, events[:k])
		if err != nil {
			t.Fatalf("k=%d: head fold: %v", k, err)
		}
		blob, err := midState.MarshalSnapshot()
		if err != nil {
			t.Fatalf("k=%d: marshal snapshot: %v", k, err)
		}
		var covered uint64
		if k > 0 {
			covered = events[k-1].Seq
		}
		snap := &fold.Snapshot{
			StreamID:              stream,
			CoveredSeq:            covered,
			Engine:                r.Engine(),
			ReducerVersion:        r.ReducerVersion(),
			SnapshotFormatVersion: foldtest.SnapshotFormatVersion,
			StateHash:             midState.StateHash(),
			State:                 blob,
		}
		resumedState, tailDeltas, err := fold.Fold(r, snap, events[k:])
		if err != nil {
			t.Fatalf("k=%d: resume fold: %v", k, err)
		}
		if resumedState.StateHash() != fullHash {
			t.Fatalf("k=%d: resumed state hash %x != full %x", k, resumedState.StateHash(), fullHash)
		}
		// The concatenation head[:k] + tail[k:] must reproduce the full delta list.
		gotDeltas := append(append([]fold.Delta(nil), headDeltas...), tailDeltas...)
		if a, b := mustJSON(t, gotDeltas), mustJSON(t, fullDeltas); a != b {
			t.Fatalf("k=%d: split deltas diverge from genesis deltas\n got: %s\nwant: %s", k, a, b)
		}
	}
}

// TestVersionGate_R_VERSION_GATE proves a snapshot whose reducer_version differs
// from the running reducer fails loudly with ErrReducerVersionSkew — never a
// silent best-effort fold.
func TestVersionGate_R_VERSION_GATE(t *testing.T) {
	r := foldtest.EchoReducer{}
	events := corpus(t)
	midState, _, err := fold.Fold(r, nil, events[:2])
	if err != nil {
		t.Fatalf("head fold: %v", err)
	}
	blob, err := midState.MarshalSnapshot()
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	snap := &fold.Snapshot{
		StreamID:              stream,
		CoveredSeq:            events[1].Seq,
		Engine:                r.Engine(),
		ReducerVersion:        r.ReducerVersion() + 1, // skew
		SnapshotFormatVersion: foldtest.SnapshotFormatVersion,
		StateHash:             midState.StateHash(),
		State:                 blob,
	}
	_, _, err = fold.Fold(r, snap, events[2:])
	if !errors.Is(err, fold.ErrReducerVersionSkew) {
		t.Fatalf("resume across reducer-version skew = %v, want ErrReducerVersionSkew", err)
	}
}

// TestEngineMismatch proves a snapshot tagged with a different engine is rejected.
func TestEngineMismatch(t *testing.T) {
	r := foldtest.EchoReducer{}
	events := corpus(t)
	midState, _, _ := fold.Fold(r, nil, events[:1])
	blob, _ := midState.MarshalSnapshot()
	snap := &fold.Snapshot{
		StreamID: stream, CoveredSeq: events[0].Seq, Engine: "v2",
		ReducerVersion: r.ReducerVersion(), SnapshotFormatVersion: foldtest.SnapshotFormatVersion,
		StateHash: midState.StateHash(), State: blob,
	}
	if _, _, err := fold.Fold(r, snap, events[1:]); !errors.Is(err, fold.ErrEngineMismatch) {
		t.Fatalf("resume across engine mismatch = %v, want ErrEngineMismatch", err)
	}
}

// TestNonContiguousTail proves a mis-sliced tail (gap or overlap versus the
// snapshot cover) is a loud error, not silent corruption.
func TestNonContiguousTail(t *testing.T) {
	r := foldtest.EchoReducer{}
	events := corpus(t)
	// Genesis fold expects seq 1 first; hand it the tail starting at seq 3.
	if _, _, err := fold.Fold(r, nil, events[2:]); !errors.Is(err, fold.ErrNonContiguousTail) {
		t.Fatalf("genesis fold of a seq-3 tail = %v, want ErrNonContiguousTail", err)
	}
}

// TestForeignEventInTail proves a tail carrying an event from a different stream
// or a different engine is rejected with ErrForeignEvent — seq contiguity alone
// cannot catch a cross-stream/cross-engine splice, since streams share the dense
// seq space.
func TestForeignEventInTail(t *testing.T) {
	r := foldtest.EchoReducer{}

	// Cross-stream: a seq-contiguous tail whose second event belongs to another
	// stream. Seq stays dense (1,2), so only the stream check catches it.
	crossStream := []fold.Event{
		nodeEvent(t, 1, "n1"),
		{
			StreamID: "gcj-root-OTHER", Seq: 2, Engine: foldtest.Engine, Type: foldtest.EventNode,
			Payload: mustCanon(t, `{"id":"n2"}`),
		},
	}
	if _, _, err := fold.Fold(r, nil, crossStream); !errors.Is(err, fold.ErrForeignEvent) {
		t.Fatalf("fold of a cross-stream tail = %v, want ErrForeignEvent", err)
	}

	// Cross-engine: same stream, dense seq, but the second event carries a foreign
	// engine tag that does not match the reducer.
	crossEngine := []fold.Event{
		nodeEvent(t, 1, "n1"),
		{
			StreamID: stream, Seq: 2, Engine: "v2", Type: foldtest.EventNode,
			Payload: mustCanon(t, `{"id":"n2"}`),
		},
	}
	if _, _, err := fold.Fold(r, nil, crossEngine); !errors.Is(err, fold.ErrForeignEvent) {
		t.Fatalf("fold of a cross-engine tail = %v, want ErrForeignEvent", err)
	}
}

func mustCanon(t *testing.T, raw string) []byte {
	t.Helper()
	p, err := canon.Canonicalize([]byte(raw))
	if err != nil {
		t.Fatalf("canonicalize %q: %v", raw, err)
	}
	return p
}

// TestUpcasterAppliedBeforeFold proves RegisterUpcaster rewrites are composed
// before the reducer sees an event: an event on an old contract folds to the
// same state as the equivalent current-contract event.
func TestUpcasterAppliedBeforeFold(t *testing.T) {
	r := foldtest.EchoReducer{}

	// A v0 node event carries {"node":"nX"} under contract "ir-0"; the registered
	// upcaster rewrites it to the current echo.node payload {"id":"nX"} under
	// "ir-1" before the reducer sees it.
	registerNodeV0Upcaster()

	oldPayload, err := canon.Canonicalize([]byte(`{"node":"nX"}`))
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	oldEvent := fold.Event{
		StreamID: stream, Seq: 1, Engine: foldtest.Engine,
		Type: "echo.node.v0", IRContractVersion: "ir-0", Payload: oldPayload,
	}
	upcastState, upcastDeltas, err := fold.Fold(r, nil, []fold.Event{oldEvent})
	if err != nil {
		t.Fatalf("fold old-contract event: %v", err)
	}

	// The equivalent current-contract event, folded directly.
	directState, directDeltas, err := fold.Fold(r, nil, []fold.Event{nodeEventNoTitle(t, 1, "nX")})
	if err != nil {
		t.Fatalf("fold current event: %v", err)
	}

	if upcastState.StateHash() != directState.StateHash() {
		t.Fatalf("upcast state %x != direct state %x", upcastState.StateHash(), directState.StateHash())
	}
	if a, b := mustJSON(t, upcastDeltas), mustJSON(t, directDeltas); a != b {
		t.Fatalf("upcast deltas != direct deltas\n up: %s\ndir: %s", a, b)
	}
}

func nodeEventNoTitle(t *testing.T, seq uint64, id string) fold.Event {
	t.Helper()
	return event(t, seq, foldtest.EventNode, fmt.Sprintf(`{"id":%q}`, id))
}

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("json marshal: %v", err)
	}
	return string(b)
}
