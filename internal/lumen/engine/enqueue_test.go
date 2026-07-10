package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestEnqueueRefusesUnlowerableIR pins the §6 L6 enqueue-wedge fix: an IR that will
// not lower (here a `run` nested under a scatter — run is top-level only this slice)
// is refused LOUD at EnqueueRun, BEFORE run.started is appended, so no wedged,
// unsealable run is ever discoverable. Head stays 0. (retry-in-scatter now lowers
// after the RN slice, so it is no longer a valid un-lowerable example.)
func TestEnqueueRefusesUnlowerableIR(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// A run nested under a scatter is un-lowerable (run is top-level only this slice).
	doc := decodeIR(t, blockDoc("bad",
		scatterNode("s", nil, "continue", runNodeJSON("r", nil, "sometarget", "", "")),
	))

	streamID, err := engine.EnqueueRun(ctx, store, doc, nil, "packs/x@v1", "workers")
	if err == nil {
		t.Fatalf("enqueue accepted an un-lowerable IR (stream %q); want a loud refusal", streamID)
	}
	if !errors.Is(err, engine.ErrUnsupportedNode) {
		t.Fatalf("enqueue error = %v, want wrapped ErrUnsupportedNode", err)
	}
	if !strings.Contains(err.Error(), "does not lower") {
		t.Errorf("enqueue error = %q, want it to name the lowering failure", err.Error())
	}
	// No run was seeded (the check precedes every side effect): nothing discoverable.
	runs, lerr := engine.ListOpenRuns(ctx, store)
	if lerr != nil {
		t.Fatalf("list open runs: %v", lerr)
	}
	if len(runs) != 0 {
		t.Fatalf("open runs = %d, want 0 (the refusal left no run behind)", len(runs))
	}
}

// containsRun reports whether the discovered open-run set includes streamID.
func containsRun(runs []engine.OpenRun, streamID string) bool {
	for _, r := range runs {
		if r.StreamID == streamID {
			return true
		}
	}
	return false
}

// TestEnqueueRunSeedsManifest (T-A1) proves EnqueueRun seeds a fresh nonce stream
// with exactly run.started (Head==1), stamps the provenance a controller loop
// reloads by (ir/input hashes, formula_ref, default_route), is discoverable via
// ListOpenRuns, and leaves the stream in the state a later Advance drives from the
// REBUILD path (its ir/input hash guards passing proves the stamped hashes match
// the doc + input).
func TestEnqueueRunSeedsManifest(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, _ := doOnlyDoc()
	doc := decodeIR(t, docJSON)
	input := map[string]any{"topic": "widgets"}

	streamID, err := engine.EnqueueRun(ctx, store, doc, input, "packs/review@v1", "workers")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	if streamID == "" {
		t.Fatal("enqueue returned an empty stream id")
	}
	if strings.ContainsRune(streamID, ':') {
		t.Fatalf("stream id %q contains ':' (it is the run root node id)", streamID)
	}

	// Head == 1: only run.started was appended.
	head, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	if head != 1 {
		t.Fatalf("head after enqueue = %d, want 1 (run.started only)", head)
	}

	// The manifest carries the provenance the loop reloads by.
	m, err := engine.ReadRunManifest(ctx, store, streamID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	if m.Name != doc.Name {
		t.Fatalf("manifest name = %q, want %q", m.Name, doc.Name)
	}
	if m.IRHash == "" {
		t.Fatal("manifest ir_hash empty — provenance not stamped")
	}
	if m.InputHash == "" {
		t.Fatal("manifest input_hash empty — a non-empty input must be pinned")
	}
	if m.FormulaRef != "packs/review@v1" {
		t.Fatalf("manifest formula_ref = %q, want packs/review@v1", m.FormulaRef)
	}
	if m.DefaultRoute != "workers" {
		t.Fatalf("manifest default_route = %q, want workers", m.DefaultRoute)
	}

	// Discoverable.
	runs, err := engine.ListOpenRuns(ctx, store)
	if err != nil {
		t.Fatalf("list open runs: %v", err)
	}
	if !containsRun(runs, streamID) {
		t.Fatalf("open runs %+v does not include the enqueued run %q", runs, streamID)
	}

	// A follow-up Advance takes the REBUILD path (head != 0) and its ir/input hash
	// guards pass — proving the stamped hashes match the doc + input.
	res, err := engine.Advance(ctx, store, doc, streamID, input, newFakeWorkStore().opts())
	if err != nil {
		t.Fatalf("advance rebuild: %v (ir/input hash guard regression?)", err)
	}
	if !res.Parked {
		t.Fatalf("advance = %+v, want Parked (a do-only run awaits the pool)", res)
	}
	if n := countJournalType(t, store, streamID, engine.EventRunStarted); n != 1 {
		t.Fatalf("run.started count = %d, want 1 (enqueue seeded it; Advance rebuilt, did not re-seed)", n)
	}
}

// TestEnqueueThenAdvanceSealsEngineOnlyRun (T-A2) is the SDK-self-sufficiency pin:
// an engine-only (exec) formula enqueued and advanced with NO Host, NO PoolRouter,
// and NO configured agent role seals in a single pass. The engine drives itself;
// only `do` execution would need a pool agent.
func TestEnqueueThenAdvanceSealsEngineOnlyRun(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("pipe",
		execNode("A", "echo a", nil),
		execNode("B", "echo b", []string{"A"}),
	))

	streamID, err := engine.EnqueueRun(ctx, store, doc, nil, "packs/pipe@v1", "")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}

	res, err := engine.Advance(ctx, store, doc, streamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("advance engine-only: %v", err)
	}
	if !res.Sealed || res.Parked {
		t.Fatalf("advance = %+v, want Sealed in one pass (no pool, no host)", res)
	}
	if res.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Run.Outcome)
	}

	// Sealed → the root leaves 'open' → ListOpenRuns drops it.
	runs, err := engine.ListOpenRuns(ctx, store)
	if err != nil {
		t.Fatalf("list open runs: %v", err)
	}
	if containsRun(runs, streamID) {
		t.Fatalf("sealed run %q still listed as open: %+v", streamID, runs)
	}
	if err := store.Verify(ctx, streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestEnqueueRunNonceStreams (T-A3) proves two enqueues of one doc open two
// distinct nonce streams, both discoverable, neither carrying the activation-key
// delimiter ':'.
func TestEnqueueRunNonceStreams(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, _ := doOnlyDoc()
	doc := decodeIR(t, docJSON)

	s1, err := engine.EnqueueRun(ctx, store, doc, nil, "ref", "workers")
	if err != nil {
		t.Fatalf("enqueue 1: %v", err)
	}
	s2, err := engine.EnqueueRun(ctx, store, doc, nil, "ref", "workers")
	if err != nil {
		t.Fatalf("enqueue 2: %v", err)
	}
	if s1 == s2 {
		t.Fatalf("two enqueues produced the same stream id %q (no per-run nonce)", s1)
	}
	for _, s := range []string{s1, s2} {
		if strings.ContainsRune(s, ':') {
			t.Fatalf("stream id %q contains ':'", s)
		}
	}

	runs, err := engine.ListOpenRuns(ctx, store)
	if err != nil {
		t.Fatalf("list open runs: %v", err)
	}
	if !containsRun(runs, s1) || !containsRun(runs, s2) {
		t.Fatalf("both nonce runs must be discoverable; got %+v", runs)
	}
}

// TestDefaultRouteFieldDropRefoldIdentity (T-A5) is the DET-T-17 extension for the
// additive default_route field: a stream whose run.started carries default_route
// survives a drop+refold byte-identically (the reducer folds no new state from it).
func TestDefaultRouteFieldDropRefoldIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	docJSON, _ := doOnlyDoc()
	doc := decodeIR(t, docJSON)

	streamID, err := engine.EnqueueRun(ctx, store, doc, nil, "packs/x@v1", "workers")
	if err != nil {
		t.Fatalf("enqueue: %v", err)
	}
	// Advance once so the projection carries a dispatched pool row too, not just the
	// root — a richer surface for the byte-identity comparison.
	if _, err := engine.Advance(ctx, store, doc, streamID, nil, newFakeWorkStore().opts()); err != nil {
		t.Fatalf("advance: %v", err)
	}

	if m, err := engine.ReadRunManifest(ctx, store, streamID); err != nil || m.DefaultRoute != "workers" {
		t.Fatalf("default_route not persisted on run.started: manifest=%+v err=%v", m, err)
	}

	// Drop+refold byte-identity: the additive field carries no hidden state.
	assertProjectionEqualsRefold(t, store, streamID)

	if v := engine.Reducer().ReducerVersion(); v != 3 {
		t.Fatalf("reducerVersion = %d, want 3 (real-bead do-node redesign took the honest bump)", v)
	}
}
