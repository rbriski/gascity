package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// goldenDoc loads and decodes a compiled IR golden from the ir testdata.
func goldenDoc(t *testing.T, name string) *ir.IR {
	t.Helper()
	path := filepath.Join("..", "ir", "testdata", "goldens", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	return decodeIR(t, string(data))
}

// --- DAG goldens ------------------------------------------------------------

// TestDiamondAllPass folds a diamond A->{B,C}->D of exec steps. All pass, D runs
// only after both B and C settle, and the run passes. It proves the reducer
// builds the DAG from node.activated deps and gates D on its two parents.
func TestDiamondAllPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("diamond",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"A"}),
		execNode("D", `echo d`, []string{"B", "C"}),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Errorf("outcome = %q, want pass", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	for _, id := range []string{"A", "B", "C", "D"} {
		if settled[id] != engine.OutcomePass {
			t.Errorf("node %q settled %q, want pass", id, settled[id])
		}
		if nodeStatus(t, store, id) != "done" {
			t.Errorf("node %q status = %q, want done", id, nodeStatus(t, store, id))
		}
	}
	// D's activated event must carry both B and C as deps (the DAG in the log).
	deps := activatedDeps(t, res.Events, "D:0")
	sort.Strings(deps)
	if len(deps) != 2 || deps[0] != "B:0" || deps[1] != "C:0" {
		t.Errorf("D deps = %v, want [B:0 C:0]", deps)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestSkipCascade is the load-bearing correctness fix: A fails, so B (which
// depends on A) is SKIPPED — a settled `skipped` outcome, never run — and the
// run outcome reflects the failure. Transitivity: C depends on B, and a skipped
// dependency is itself blocking, so C skip-cascades too.
func TestSkipCascade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("cascade",
		execNode("A", `exit 1`, nil),
		execNode("B", `echo ran`, []string{"A"}),
		execNode("C", `echo ran`, []string{"B"}),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("run outcome = %q, want failed", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["A"] != engine.OutcomeFailed {
		t.Errorf("A settled %q, want failed", settled["A"])
	}
	if settled["B"] != engine.OutcomeSkipped {
		t.Errorf("B settled %q, want skipped", settled["B"])
	}
	if settled["C"] != engine.OutcomeSkipped {
		t.Errorf("C settled %q, want skipped (transitive cascade)", settled["C"])
	}
	// A skipped node is never run: its output is empty and its status is skipped.
	if got := res.NodeOutputs["B"]; got != "" {
		t.Errorf("B output = %q, want empty (skipped, not run — it never echoed)", got)
	}
	if nodeStatus(t, store, "B") != "skipped" {
		t.Errorf("B status = %q, want skipped", nodeStatus(t, store, "B"))
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestScatterMembersFanOut folds the scatter-members golden: two `do` members
// fan out under a scatter, both pass, and the scatter aggregate settles pass.
func TestScatterMembersFanOut(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := goldenDoc(t, "scatter-members.ir.json")
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"gpt":    {Outcome: enginehost.OutcomePass, Output: "gpt review"},
		"claude": {Outcome: enginehost.OutcomePass, Output: "claude review"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Errorf("outcome = %q, want pass", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	for _, id := range []string{"gpt", "claude", "reviews"} {
		if settled[id] != engine.OutcomePass {
			t.Errorf("node %q settled %q, want pass", id, settled[id])
		}
	}
	// The members are parented to the scatter activation.
	if p := activatedParent(t, res.Events, "gpt:0"); p != "reviews:0" {
		t.Errorf("gpt parent = %q, want reviews:0", p)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// TestGatherAuthoredDegrade folds the gather-authored-degrade golden: one member
// passes, one settles failed, and the authored combine settles degraded — so
// the gather (and the run) settle degraded. A member failure drains into the
// gather rather than failing the run, and the gather emits a head-of-line
// node.decision checkpoint per member.
func TestGatherAuthoredDegrade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := goldenDoc(t, "gather-authored-degrade.ir.json")
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"ok": {Outcome: enginehost.OutcomePass, Output: "looks good"},
	}}

	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomeDegraded {
		t.Errorf("run outcome = %q, want degraded", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["ok"] != engine.OutcomePass {
		t.Errorf("ok settled %q, want pass", settled["ok"])
	}
	if settled["flaky"] != engine.OutcomeFailed {
		t.Errorf("flaky settled %q, want failed", settled["flaky"])
	}
	if settled["reviews_gather"] != engine.OutcomeDegraded {
		t.Errorf("gather settled %q, want degraded", settled["reviews_gather"])
	}
	// Head-of-line: one fold checkpoint per drained member, in member order.
	var members []string
	for _, e := range res.Events {
		if e.Type != engine.EventNodeDecision {
			continue
		}
		var p struct {
			Decision   string `json:"decision"`
			NextMember string `json:"next_member"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode decision: %v", err)
		}
		if p.Decision == "fold_ckpt" {
			members = append(members, p.NextMember)
		}
	}
	if len(members) != 2 || members[0] != "ok:0" || members[1] != "flaky:0" {
		t.Errorf("gather checkpoints = %v, want [ok:0 flaky:0] (member order)", members)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify = %v", err)
	}
}

// --- Determinism proofs -----------------------------------------------------

// TestDETT17_DAGRebuildByteIdentity proves the live incremental projection of a
// DAG run is byte-identical to a DROP+refold from genesis (DET-T-17), over a
// stream that populates nodes, edges, and frontier non-vacuously.
func TestDETT17_DAGRebuildByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("diamond",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"A"}),
		execNode("D", `echo d`, []string{"B", "C"}),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// The stream genuinely populated edges (the DAG) so the comparison is not
	// vacuous.
	var edgeCount int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id WHERE n.stream_id = ?`,
		res.StreamID).Scan(&edgeCount); err != nil {
		t.Fatalf("count edges: %v", err)
	}
	if edgeCount == 0 {
		t.Fatal("no edges projected — DET-T-17 comparison would be vacuous")
	}

	live := dumpTierA(t, store, res.StreamID)
	if err := store.RebuildTierA(ctx, engine.Reducer(), res.StreamID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt := dumpTierA(t, store, res.StreamID)
	if live != rebuilt {
		t.Fatalf("live projection not byte-identical to rebuild:\n--- live ---\n%s\n--- rebuild ---\n%s", live, rebuilt)
	}
}

// TestSplitPointEquivalence_DETT20 proves the fold is associative over the log:
// for every split point k, folding a snapshot at k plus the tail equals folding
// the whole stream from genesis, in both final StateHash and concatenated
// deltas.
func TestSplitPointEquivalence_DETT20(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("diamond",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"A"}),
		execNode("D", `echo d`, []string{"B", "C"}),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	all := foldEvents(res.Events)
	r := engine.Reducer()
	genesisState, genesisDeltas, err := fold.Fold(r, nil, all)
	if err != nil {
		t.Fatalf("genesis fold: %v", err)
	}
	genesisHash := genesisState.StateHash()

	for k := 0; k <= len(all); k++ {
		prefixState, prefixDeltas, err := fold.Fold(r, nil, all[:k])
		if err != nil {
			t.Fatalf("k=%d prefix fold: %v", k, err)
		}
		var snap *fold.Snapshot
		if k > 0 {
			blob, err := prefixState.MarshalSnapshot()
			if err != nil {
				t.Fatalf("k=%d marshal snapshot: %v", k, err)
			}
			snap = &fold.Snapshot{
				StreamID:              res.StreamID,
				CoveredSeq:            all[k-1].Seq,
				Engine:                "lumen",
				ReducerVersion:        r.ReducerVersion(),
				SnapshotFormatVersion: 4,
				StateHash:             prefixState.StateHash(),
				State:                 blob,
			}
		}
		tailState, tailDeltas, err := fold.Fold(r, snap, all[k:])
		if err != nil {
			t.Fatalf("k=%d tail fold: %v", k, err)
		}
		if tailState.StateHash() != genesisHash {
			t.Fatalf("k=%d final state hash diverges from genesis", k)
		}
		joined := append(append([]fold.Delta{}, prefixDeltas...), tailDeltas...)
		if deltaJSON(t, joined) != deltaJSON(t, genesisDeltas) {
			t.Fatalf("k=%d split deltas diverge from genesis deltas", k)
		}
	}
}

// scatterGatherDoc builds a host-free scatter/gather stream that populates
// MEMBER edges (scatter members + a gather draining a scatter) alongside an
// after gate (a seed exec the scatter is `after`), so the member-edge
// determinism proofs (N-2) are non-vacuous over both edge kinds.
func scatterGatherDoc(t *testing.T) *ir.IR {
	t.Helper()
	return decodeIR(t, blockDoc("sgdet",
		execNode("seed", `echo seed`, nil),
		scatterNode("reviews", []string{"seed"}, "continue",
			execNode("m1", `echo r1`, nil),
			execNode("m2", `echo r2`, nil)),
		gatherNode("greview", "reviews", []string{"reviews"},
			execNode("c1", `echo combined`, nil)),
	))
}

// TestDETT17_ScatterGatherRebuildByteIdentity pins DET-T-17 over MEMBER edges,
// not just the after-edge diamond: the live incremental projection of a
// scatter/gather run is byte-identical to a DROP+refold from genesis, over a
// stream whose edge set genuinely includes member edges.
func TestDETT17_ScatterGatherRebuildByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := scatterGatherDoc(t)
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass", res.Outcome)
	}

	// The stream genuinely populated MEMBER edges (not only after edges), so the
	// determinism proof is pinned over member edges rather than being vacuous.
	var memberEdges int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM edges e JOIN nodes n ON n.id = e.from_id
		 WHERE n.stream_id = ? AND e.dep_type = 'member'`, res.StreamID).Scan(&memberEdges); err != nil {
		t.Fatalf("count member edges: %v", err)
	}
	if memberEdges == 0 {
		t.Fatal("no member edges projected — the member-edge DET-T-17 proof would be vacuous")
	}

	live := dumpTierA(t, store, res.StreamID)
	if err := store.RebuildTierA(ctx, engine.Reducer(), res.StreamID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	rebuilt := dumpTierA(t, store, res.StreamID)
	if live != rebuilt {
		t.Fatalf("live projection not byte-identical to rebuild over member edges:\n--- live ---\n%s\n--- rebuild ---\n%s", live, rebuilt)
	}
}

// TestDETT20_ScatterGatherSplitPointEquivalence pins DET-T-20 (fold
// associativity: snapshot-at-k plus tail equals genesis, in both final StateHash
// and concatenated deltas) over a scatter/gather stream carrying member edges —
// the split-point variant of the after-edge proof.
func TestDETT20_ScatterGatherSplitPointEquivalence(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := scatterGatherDoc(t)
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	all := foldEvents(res.Events)
	r := engine.Reducer()
	genesisState, genesisDeltas, err := fold.Fold(r, nil, all)
	if err != nil {
		t.Fatalf("genesis fold: %v", err)
	}
	genesisHash := genesisState.StateHash()

	for k := 0; k <= len(all); k++ {
		prefixState, prefixDeltas, err := fold.Fold(r, nil, all[:k])
		if err != nil {
			t.Fatalf("k=%d prefix fold: %v", k, err)
		}
		var snap *fold.Snapshot
		if k > 0 {
			blob, err := prefixState.MarshalSnapshot()
			if err != nil {
				t.Fatalf("k=%d marshal snapshot: %v", k, err)
			}
			snap = &fold.Snapshot{
				StreamID:              res.StreamID,
				CoveredSeq:            all[k-1].Seq,
				Engine:                "lumen",
				ReducerVersion:        r.ReducerVersion(),
				SnapshotFormatVersion: 4,
				StateHash:             prefixState.StateHash(),
				State:                 blob,
			}
		}
		tailState, tailDeltas, err := fold.Fold(r, snap, all[k:])
		if err != nil {
			t.Fatalf("k=%d tail fold: %v", k, err)
		}
		if tailState.StateHash() != genesisHash {
			t.Fatalf("k=%d final state hash diverges from genesis (member-edge stream)", k)
		}
		joined := append(append([]fold.Delta{}, prefixDeltas...), tailDeltas...)
		if deltaJSON(t, joined) != deltaJSON(t, genesisDeltas) {
			t.Fatalf("k=%d split deltas diverge from genesis deltas (member-edge stream)", k)
		}
	}
}

// --- Upcast identity --------------------------------------------------------

// TestP1JournalUpcastFoldsIdentically writes a journal in P1's provisional
// vocabulary (run.started / node.settled / run.closed, no node.activated) and
// folds it under the v2 reducer via RebuildTierA. The node.settled→outcome.settled
// upcaster fires transparently, so the P1 journal projects the SAME Tier-A rows
// P1 produced for the P1-emitted outcomes (pass/failed): a done root, per-step
// nodes parented to the root, and an empty frontier at close.
//
// The identity claim is NARROWED to pass/failed on purpose: v2's statusForOutcome
// diverges from P1 for an authored `skipped` ("skipped" vs P1's "done") and
// `canceled`. That divergence is intentional-and-safe under the reducerVersion
// bump to 2 — the P1 walking skeleton was exec-only and never emitted an authored
// skip/cancel, so no live P1 journal contains one (see
// TestP1JournalUpcastSkippedSettleAdoptsV2Status).
func TestP1JournalUpcastFoldsIdentically(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	const stream = "gcg-run-p1legacy00"

	// P1 registered these three coarse types; node.settled is the legacy type.
	store.RegisterEventType("lumen", "lumen.run.started")
	store.RegisterEventType("lumen", "lumen.node.settled")
	store.RegisterEventType("lumen", "lumen.run.closed")

	lease, err := store.AcquireWriterLease(ctx, stream, "test", 30_000_000_000)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	appendP1 := func(head uint64, typ, idem string, payload any) {
		body, _ := canonJSON(t, payload)
		if _, err := store.Append(ctx, stream, "lumen", head, lease.Epoch, []graphstore.JournalEvent{{
			Type: typ, IRContractVersion: "0.2.5", IdemToken: idem, Payload: body,
		}}); err != nil {
			t.Fatalf("append %s: %v", typ, err)
		}
	}
	appendP1(0, "lumen.run.started", stream+":run:started", map[string]any{
		"root_id": stream, "name": "legacy", "created_at": "2020-01-01T00:00:00Z",
	})
	appendP1(1, "lumen.node.settled", stream+":n1:0", map[string]any{
		"id": "n1", "outcome": "pass", "output": "one",
	})
	appendP1(2, "lumen.node.settled", stream+":n2:0", map[string]any{
		"id": "n2", "outcome": "failed", "output": "",
	})
	appendP1(3, "lumen.run.closed", stream+":run:closed", map[string]any{"outcome": "failed"})
	_ = store.ReleaseWriterLease(ctx, lease)

	// Fold the P1 journal under the v2 reducer. A missing upcaster would fail
	// here with "unknown event type lumen.node.settled".
	if err := store.RebuildTierA(ctx, engine.Reducer(), stream); err != nil {
		t.Fatalf("rebuild (upcast) failed: %v", err)
	}

	// Concrete P1 projection: bare node ids, root done, n1 done, n2 failed.
	if got := nodeStatus(t, store, stream); got != "failed" {
		t.Errorf("root status = %q, want failed", got)
	}
	if got := nodeStatus(t, store, "n1"); got != "done" {
		t.Errorf("n1 status = %q, want done (upcast to outcome.settled)", got)
	}
	if got := nodeStatus(t, store, "n2"); got != "failed" {
		t.Errorf("n2 status = %q, want failed", got)
	}
	// Parented to the root, exactly as P1 projected.
	if got := nodeParent(t, store, "n1"); got != stream {
		t.Errorf("n1 parent = %q, want the root %q", got, stream)
	}
	// The frontier is empty at close (root cleared, no leaf rows for a P1 journal).
	var frontierRows int
	if err := store.DB().QueryRowContext(ctx,
		`SELECT COUNT(*) FROM frontier WHERE root_id = ?`, stream).Scan(&frontierRows); err != nil {
		t.Fatalf("count frontier: %v", err)
	}
	if frontierRows != 0 {
		t.Errorf("frontier rows = %d, want 0", frontierRows)
	}
}

// --- Vocabulary freeze ------------------------------------------------------

// TestVocabularyFrozen18Types asserts the frozen event vocabulary is exactly 18
// unique types and that every one is registered with the store (Append accepts
// it — its payload is a typed struct, enforced at compile time).
func TestVocabularyFrozen18Types(t *testing.T) {
	if len(engine.EventTypes) != 18 {
		t.Fatalf("EventTypes has %d entries, want 18", len(engine.EventTypes))
	}
	seen := map[string]bool{}
	for _, ty := range engine.EventTypes {
		if seen[ty] {
			t.Errorf("duplicate event type %q", ty)
		}
		seen[ty] = true
	}

	ctx := context.Background()
	store := newStore(t)
	engine.RegisterVocabulary(store)
	const stream = "gcg-run-vocabcheck0"
	lease, err := store.AcquireWriterLease(ctx, stream, "test", 30_000_000_000)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()
	// Append one dummy event of each registered type; an unregistered type would
	// fail with ErrUnknownEventType.
	for i, ty := range engine.EventTypes {
		body, _ := canonJSON(t, map[string]any{})
		if _, err := store.Append(ctx, stream, "lumen", uint64(i), lease.Epoch, []graphstore.JournalEvent{{
			Type: ty, IRContractVersion: "0.2.5", IdemToken: stream + ":" + ty, Payload: body,
		}}); err != nil {
			t.Errorf("append %q rejected (not registered?): %v", ty, err)
		}
	}
}

// TestEveryEventTypeHasTypedPayload proves every frozen event type has a typed
// payload sample (no map[string]any on the wire) and that the sample catalog
// covers exactly the 18 registered types.
func TestEveryEventTypeHasTypedPayload(t *testing.T) {
	samples := engine.EventPayloadSamples()
	if len(samples) != len(engine.EventTypes) {
		t.Fatalf("payload samples = %d, want %d (one per event type)", len(samples), len(engine.EventTypes))
	}
	for _, ty := range engine.EventTypes {
		sample, ok := samples[ty]
		if !ok {
			t.Errorf("event type %q has no typed payload sample", ty)
			continue
		}
		if _, err := json.Marshal(sample); err != nil {
			t.Errorf("event type %q payload does not marshal: %v", ty, err)
		}
	}
}

// --- Deferred kinds ---------------------------------------------------------

// TestDeferredKindsRefusedCleanly proves the P4.2 pressure valve: node kinds
// whose executor arm is deferred are refused with ErrUnsupportedNode before any
// effect runs, and write no journal.
func TestDeferredKindsRefusedCleanly(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name string
		node string
	}{
		{"dispatch", `{"kind":"dispatch","id":"d","after":[],"origin":{"uri":"t","line":1,"col":0},"subject":{"kind":"ref","name":"x"},"discriminant":"kind","exhaustive":true,"arms":[]}`},
		// retry/repeat are NO LONGER deferred — they land as the L5 attempt-loop arm
		// (see loop_test.go); timeout now lands as the TNK arm (see timeout_test.go /
		// timeout_plan_test.go — a bodyless timeout refuses on its MISSING BODY there).
		// channel/async stay refused.
		{"channel", `{"kind":"channel","id":"ch","after":[],"origin":{"uri":"t","line":1,"col":0}}`},
		{"async", `{"kind":"async","id":"as","after":[],"origin":{"uri":"t","line":1,"col":0}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			doc := decodeIR(t, blockDoc("deferred", tc.node))
			_, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: &enginehost.StubHost{}})
			if !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("err = %v, want ErrUnsupportedNode", err)
			}
			var journalRows int
			if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM journal`).Scan(&journalRows); err != nil {
				t.Fatalf("count journal: %v", err)
			}
			if journalRows != 0 {
				t.Errorf("a refused %s wrote %d journal rows, want 0", tc.name, journalRows)
			}
		})
	}
}

// TestSingleAttemptStreamUnchanged (T-K3) is the L5a byte-shape spot-check: the
// activation-key generalization (:0 → :N) and the attempt-derived effect token
// must leave the single-attempt (attempt-0) path bit-for-bit identical to pre-L5.
// It pins the exact idem-token string a do run produces — the do effect token stays
// `…:greet:do:1` (attempt 0 ⇒ suffix 1) — AND the absence of an attempt-loop field
// on the attempt-0 outcome.settled, so a newly-populated attempt-0 field (a
// `retryable`, an `attempt`, …) genuinely FAILS here rather than silently diverging
// every persisted stream. (The full engine suite + the DET drop+refold tests are the
// broader byte-identity gate.)
func TestSingleAttemptStreamUnchanged(t *testing.T) {
	ctx := context.Background()

	// A single do run: the engine-inline effect token is attempt-derived and must
	// render the historical `:do:1` for attempt 0, and the attempt-0 outcome.settled
	// must carry NO `retryable` field (the L5 attempt-loop field).
	store := newStore(t)
	doc := decodeIR(t, blockDoc("greeter", doNode("greet", "Say hi.", nil)))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"greet": {Outcome: enginehost.OutcomePass, Output: "hi"},
	}}
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: host})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	sched := findEventOf(t, res.Events, engine.EventEffectScheduled)
	var sp struct {
		IdemToken string `json:"idem_token"`
	}
	if err := json.Unmarshal(sched.Payload, &sp); err != nil {
		t.Fatalf("decode effect.scheduled: %v", err)
	}
	if want := res.StreamID + ":greet:do:1"; sp.IdemToken != want {
		t.Fatalf("effect idem token = %q, want %q (attempt-0 must derive :do:1)", sp.IdemToken, want)
	}
	settled := findEventOf(t, res.Events, engine.EventOutcomeSettled)
	if got := payloadKeys(t, settled.Payload); got["retryable"] {
		t.Fatalf("attempt-0 outcome.settled carries a `retryable` field; want it omitted (pre-L5 shape)")
	}
}

// payloadKeys decodes a canonical event payload to the set of keys it carries.
func payloadKeys(t *testing.T, payload []byte) map[string]bool {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	keys := make(map[string]bool, len(m))
	for k := range m {
		keys[k] = true
	}
	return keys
}

// findEventOf returns the first event of the given type.
func findEventOf(t *testing.T, events []graphstore.StoredEvent, typ string) graphstore.StoredEvent {
	t.Helper()
	for _, e := range events {
		if e.Type == typ {
			return e
		}
	}
	t.Fatalf("no %s event in stream", typ)
	return graphstore.StoredEvent{}
}

// --- helpers ----------------------------------------------------------------

func settledOutcomeByID(t *testing.T, events []graphstore.StoredEvent) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Outcome    string `json:"outcome"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		out[engine.ActivationNodeID(p.Activation)] = p.Outcome
	}
	return out
}

func activatedDeps(t *testing.T, events []graphstore.StoredEvent, activation string) []string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation string   `json:"activation"`
			After      []string `json:"after"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		if p.Activation == activation {
			return p.After
		}
	}
	t.Fatalf("no node.activated for %q", activation)
	return nil
}

func activatedParent(t *testing.T, events []graphstore.StoredEvent, activation string) string {
	t.Helper()
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			Activation       string `json:"activation"`
			ParentActivation string `json:"parent_activation"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated: %v", err)
		}
		if p.Activation == activation {
			return p.ParentActivation
		}
	}
	t.Fatalf("no node.activated for %q", activation)
	return ""
}

func nodeParent(t *testing.T, store *graphstore.Store, id string) string {
	t.Helper()
	var parent string
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT parent_id FROM nodes WHERE id = ?`, id).Scan(&parent); err != nil {
		t.Fatalf("read parent of %q: %v", id, err)
	}
	return parent
}

func foldEvents(stored []graphstore.StoredEvent) []fold.Event {
	out := make([]fold.Event, len(stored))
	for i, e := range stored {
		out[i] = fold.Event{
			StreamID:          e.StreamID,
			Seq:               e.Seq,
			Engine:            e.Engine,
			Substream:         e.Substream,
			Type:              e.Type,
			IRContractVersion: e.IRContractVersion,
			IdemToken:         e.IdemToken,
			Payload:           e.Payload,
		}
	}
	return out
}

func deltaJSON(t *testing.T, deltas []fold.Delta) string {
	t.Helper()
	b, err := json.Marshal(deltas)
	if err != nil {
		t.Fatalf("marshal deltas: %v", err)
	}
	return string(b)
}

// dumpTierA renders a stream's nodes / node_metadata / edges / frontier rows
// into a stable string for byte-identity comparison. It includes node_metadata
// (where outcome/output live) and created_at (M2) so a rebuild-side metadata or
// timestamp bug fails DET-T-17 rather than slipping past a column subset.
func dumpTierA(t *testing.T, store *graphstore.Store, stream string) string {
	t.Helper()
	ctx := context.Background()
	var sb strings.Builder
	rows, err := store.DB().QueryContext(ctx,
		`SELECT id, status, bead_type, parent_id, created_at FROM nodes WHERE stream_id = ? ORDER BY id`, stream)
	if err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	for rows.Next() {
		var id, status, bt, parent, createdAt string
		if err := rows.Scan(&id, &status, &bt, &parent, &createdAt); err != nil {
			t.Fatalf("scan node: %v", err)
		}
		sb.WriteString("node " + id + " " + status + " " + bt + " " + parent + " " + createdAt + "\n")
	}
	_ = rows.Close()
	mrows, err := store.DB().QueryContext(ctx,
		`SELECT nm.node_id, nm.key, nm.value FROM node_metadata nm JOIN nodes n ON n.id = nm.node_id
		 WHERE n.stream_id = ? ORDER BY nm.node_id, nm.key`, stream)
	if err != nil {
		t.Fatalf("query node_metadata: %v", err)
	}
	for mrows.Next() {
		var nid, key, value string
		if err := mrows.Scan(&nid, &key, &value); err != nil {
			t.Fatalf("scan node_metadata: %v", err)
		}
		sb.WriteString("meta " + nid + " " + key + "=" + value + "\n")
	}
	_ = mrows.Close()
	erows, err := store.DB().QueryContext(ctx,
		`SELECT from_id, to_id, dep_type FROM edges e JOIN nodes n ON n.id = e.from_id
		 WHERE n.stream_id = ? ORDER BY from_id, to_id, dep_type`, stream)
	if err != nil {
		t.Fatalf("query edges: %v", err)
	}
	for erows.Next() {
		var from, to, dt string
		if err := erows.Scan(&from, &to, &dt); err != nil {
			t.Fatalf("scan edge: %v", err)
		}
		sb.WriteString("edge " + from + "->" + to + " " + dt + "\n")
	}
	_ = erows.Close()
	frows, err := store.DB().QueryContext(ctx,
		`SELECT node_id, ready_priority, id FROM frontier WHERE root_id = ? ORDER BY id`, stream)
	if err != nil {
		t.Fatalf("query frontier: %v", err)
	}
	for frows.Next() {
		var nid, id string
		var rp int
		if err := frows.Scan(&nid, &rp, &id); err != nil {
			t.Fatalf("scan frontier: %v", err)
		}
		sb.WriteString("frontier " + nid + "\n")
	}
	_ = frows.Close()
	return sb.String()
}

func canonJSON(t *testing.T, v any) ([]byte, error) {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	return canon.Canonicalize(raw)
}
