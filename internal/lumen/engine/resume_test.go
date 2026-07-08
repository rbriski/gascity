package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// snapshotRow is one persisted snapshot read straight from the table.
type snapshotRow struct {
	covered   uint64
	rv, sfv   int
	stateHash [32]byte
	state     []byte
}

// allSnapshots reads every snapshot row for a stream in covered order.
func allSnapshots(t *testing.T, store *graphstore.Store, stream string) []snapshotRow {
	t.Helper()
	rows, err := store.DB().QueryContext(context.Background(),
		`SELECT covered_seq, reducer_version, snapshot_format_version, state_hash, state
		   FROM snapshots WHERE stream_id = ? ORDER BY covered_seq`, stream)
	if err != nil {
		t.Fatalf("query snapshots: %v", err)
	}
	defer func() { _ = rows.Close() }()
	var out []snapshotRow
	for rows.Next() {
		var r snapshotRow
		var h []byte
		if err := rows.Scan(&r.covered, &r.rv, &r.sfv, &h, &r.state); err != nil {
			t.Fatalf("scan snapshot: %v", err)
		}
		copy(r.stateHash[:], h)
		out = append(out, r)
	}
	return out
}

// TestResumeLawStoreBacked_RRESUME is the store-backed R-RESUME proof: a run that
// snapshots at every unit boundary persists several anchors; loading EACH one from
// the snapshots table and folding only the tail after it reproduces the genesis
// fold — identical final state hash AND identical concatenated deltas (the Tier-A
// projection is a pure function of the deltas, so this is byte-identical
// projection at every snapshot point). It reuses the DET-T-20 split discipline
// but over STORE-PERSISTED snapshots, not in-memory ones.
func TestResumeLawStoreBacked_RRESUME(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("diamond",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"A"}),
		execNode("D", `echo d`, []string{"B", "C"}),
	))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}

	snaps := allSnapshots(t, store, res.StreamID)
	if len(snaps) < 2 {
		t.Fatalf("persisted snapshots = %d, want several (cadence 1) — proof would be weak", len(snaps))
	}

	all := foldEvents(res.Events)
	r := engine.Reducer()
	genesisState, genesisDeltas, err := fold.Fold(r, nil, all)
	if err != nil {
		t.Fatalf("genesis fold: %v", err)
	}
	genesisHash := genesisState.StateHash()

	seqIndex := func(seq uint64) int {
		for i, e := range all {
			if e.Seq == seq {
				return i + 1 // number of events with seq <= covered
			}
		}
		t.Fatalf("covered seq %d not in journal", seq)
		return 0
	}

	for _, sr := range snaps {
		snap := &fold.Snapshot{
			StreamID: res.StreamID, CoveredSeq: sr.covered, Engine: "lumen",
			ReducerVersion: sr.rv, SnapshotFormatVersion: sr.sfv, StateHash: sr.stateHash, State: sr.state,
		}
		k := seqIndex(sr.covered)
		prefixState, prefixDeltas, err := fold.Fold(r, nil, all[:k])
		if err != nil {
			t.Fatalf("covered=%d prefix fold: %v", sr.covered, err)
		}
		// The persisted snapshot's state hash matches an independent fold of the
		// same prefix — the stored blob is faithful (state-hash match).
		if prefixState.StateHash() != sr.stateHash {
			t.Fatalf("covered=%d persisted state hash != prefix fold hash", sr.covered)
		}
		tailState, tailDeltas, err := fold.Fold(r, snap, all[k:])
		if err != nil {
			t.Fatalf("covered=%d resume fold: %v", sr.covered, err)
		}
		if tailState.StateHash() != genesisHash {
			t.Fatalf("covered=%d resumed state hash != genesis", sr.covered)
		}
		joined := append(append([]fold.Delta{}, prefixDeltas...), tailDeltas...)
		if deltaJSON(t, joined) != deltaJSON(t, genesisDeltas) {
			t.Fatalf("covered=%d snapshot+tail deltas diverge from genesis", sr.covered)
		}
	}

	// Projection byte-identity via the dump discipline: a genesis rebuild of the
	// (snapshot-bearing) journal reproduces the live projection exactly.
	live := dumpTierA(t, store, res.StreamID)
	if err := store.RebuildTierA(ctx, r, res.StreamID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rebuilt := dumpTierA(t, store, res.StreamID); live != rebuilt {
		t.Fatalf("snapshot-run projection not byte-identical to genesis rebuild:\n--- live ---\n%s\n--- rebuilt ---\n%s", live, rebuilt)
	}
}

// TestTruncateThenVerifyAndResumeConverges proves a truncated journal still
// Verifies AND still folds the surviving tail (from the retained snapshot) to the
// genesis state — the snapshot has become the new anchor for both the hash chain
// and the fold.
func TestTruncateThenVerifyAndResumeConverges(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("chain",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"B"}),
	))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	all := foldEvents(res.Events)
	r := engine.Reducer()
	genesisState, _, err := fold.Fold(r, nil, all)
	if err != nil {
		t.Fatalf("genesis fold: %v", err)
	}

	snaps := allSnapshots(t, store, res.StreamID)
	if len(snaps) < 2 {
		t.Fatalf("snapshots = %d, want >= 2", len(snaps))
	}
	// Keep the last snapshot; truncate below an older one (the retention policy:
	// never below the latest durable snapshot).
	cut := snaps[len(snaps)-2].covered
	deleted, err := store.TruncateBelowAnchor(ctx, res.StreamID, cut)
	if err != nil {
		t.Fatalf("truncate below %d: %v", cut, err)
	}
	if deleted == 0 {
		t.Fatalf("truncate deleted 0 rows, want > 0")
	}

	// Chain still verifies across the cut.
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Fatalf("Verify after truncate = %v", err)
	}

	// Resume-fold from the LATEST surviving snapshot converges on the genesis state.
	snap, ok, err := store.LatestSnapshot(ctx, res.StreamID)
	if err != nil || !ok {
		t.Fatalf("latest snapshot: ok=%v err=%v", ok, err)
	}
	tail, err := store.ReadStream(ctx, res.StreamID, snap.CoveredSeq+1, 0)
	if err != nil {
		t.Fatalf("read tail: %v", err)
	}
	tailState, _, err := fold.Fold(r, &snap, foldEvents(tail))
	if err != nil {
		t.Fatalf("resume fold after truncate: %v", err)
	}
	if tailState.StateHash() != genesisState.StateHash() {
		t.Fatalf("resumed state after truncate != genesis")
	}
}

// TestResumeContinuesCrashedRun proves engine.Resume drives a crashed run (an
// open stream with settled prefix work but no run.closed) to completion: the
// already-settled step is reloaded (never re-run), the unstarted step runs, and
// the run seals with the aggregated outcome.
func TestResumeContinuesCrashedRun(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	engine.RegisterVocabulary(store)
	const stream = "gcg-run-resume000000"

	doc := decodeIR(t, blockDoc("chain",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
	))

	// Simulate a crash after A settled but before B ran and before run.closed.
	m := newManualRun(t, store, stream)
	m.append(engine.EventRunStarted, stream+":run:started", map[string]any{
		"root_id": stream, "name": "chain", "created_at": "2020-01-01T00:00:00Z",
	})
	m.append(engine.EventNodeActivated, stream+":A:0:act", map[string]any{
		"node_id": "A", "activation": "A:0", "kind": "exec",
	})
	m.append(engine.EventOutcomeSettled, stream+":A:0:settled", map[string]any{
		"activation": "A:0", "outcome": "pass", "output": "a",
	})
	m.close()

	res, err := engine.Resume(ctx, store, doc, stream, nil, engine.Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["B"] != engine.OutcomePass {
		t.Errorf("B settled %q, want pass (B ran on resume)", settled["B"])
	}
	// A was NOT re-run: exactly one A settlement exists across the whole journal.
	full, err := store.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := countSettlements(t, full, "A:0"); n != 1 {
		t.Errorf("A settlements = %d, want 1 (A reloaded, not re-run)", n)
	}
	if got := closedOutcome(t, full); got != engine.OutcomePass {
		t.Errorf("run.closed outcome = %q, want pass", got)
	}
	if err := store.Verify(ctx, stream); err != nil {
		t.Errorf("Verify after resume: %v", err)
	}
}

// TestResumeAtMostOnceSettlesInterruptedEffect proves the P4.1 crash contract on
// resume: a do step whose effect.scheduled committed but whose effect.settled did
// NOT (a crash mid-effect) is settled FAILED on resume and the agent host is
// NEVER re-invoked for it — at-most-once, proven by the host call count.
func TestResumeAtMostOnceSettlesInterruptedEffect(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	engine.RegisterVocabulary(store)
	const stream = "gcg-run-atmostonce00"

	doc := decodeIR(t, blockDoc("agentrun", doNode("work", "Do the work.", nil)))

	// Crash between effect.scheduled and effect.settled.
	idem := stream + ":work:0:do:1"
	m := newManualRun(t, store, stream)
	m.append(engine.EventRunStarted, stream+":run:started", map[string]any{
		"root_id": stream, "name": "agentrun", "created_at": "2020-01-01T00:00:00Z",
	})
	m.append(engine.EventNodeActivated, stream+":work:0:act", map[string]any{
		"node_id": "work", "activation": "work:0", "kind": "do",
	})
	m.append(engine.EventEffectScheduled, idem+":sched", map[string]any{
		"activation": "work:0", "effect": "do", "idem_token": idem,
		"policy": "at_most_once", "spec_hash": "deadbeef",
		"spec": map[string]any{"prompt": "Do the work."},
	})
	m.close()

	// The host is scripted to PASS — if resume re-ran the effect, the node would
	// pass. At-most-once forbids that: it must settle failed without calling the host.
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"work": {Outcome: enginehost.OutcomePass, Output: "should never run"},
	}}
	res, err := engine.Resume(ctx, store, doc, stream, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(stub.Calls()) != 0 {
		t.Fatalf("host called %d times on resume, want 0 (at-most-once: a scheduled effect is never re-run)", len(stub.Calls()))
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("resumed outcome = %q, want failed (interrupted effect settles failed)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["work"] != engine.OutcomeFailed {
		t.Errorf("work settled %q, want failed", settled["work"])
	}
	// The interrupted effect got exactly one settlement, marked interrupted.
	full, err := store.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := effectSettledResults(t, full); len(got) != 1 || got[0] != engine.EffectResultInterrupted {
		t.Errorf("effect settled results = %v, want [interrupted]", got)
	}
	if err := store.Verify(ctx, stream); err != nil {
		t.Errorf("Verify after resume: %v", err)
	}
}

// TestResumeSealedStreamIsNoOp proves resuming an already-completed (sealed) run
// is an idempotent no-op that returns the finished outcome and writes nothing new.
func TestResumeSealedStreamIsNoOp(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("hello", execNode("greet", `echo hi`, nil)))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	headBefore, _ := store.Head(ctx, res.StreamID)

	resumed, err := engine.Resume(ctx, store, doc, res.StreamID, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("resume sealed: %v", err)
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed sealed outcome = %q, want pass", resumed.Outcome)
	}
	if headAfter, _ := store.Head(ctx, res.StreamID); headAfter != headBefore {
		t.Errorf("head grew from %d to %d resuming a sealed stream, want no-op", headBefore, headAfter)
	}
}

// TestResumeRefusesForeignIRHash proves resume refuses a doc whose ir_hash does
// not match the one run.started pinned — you cannot resume a run with a different
// formula.
func TestResumeRefusesForeignIRHash(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	docA := decodeIR(t, blockDoc("hello", execNode("greet", `echo hi`, nil)))
	res, err := engine.Run(ctx, store, docA, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	docB := decodeIR(t, blockDoc("hello", execNode("greet", `echo DIFFERENT`, nil)))
	if _, err := engine.Resume(ctx, store, docB, res.StreamID, nil, engine.Options{}); !errors.Is(err, engine.ErrIRHashMismatch) {
		t.Fatalf("resume with a foreign ir_hash = %v, want ErrIRHashMismatch", err)
	}
}

// TestResumeDetectsCorruptedSnapshot proves a snapshot whose stored state hash
// does not match its blob is rejected on resume — a corrupted anchor is detected,
// never silently folded.
func TestResumeDetectsCorruptedSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("chain",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
	))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	// Corrupt the latest snapshot's state_hash directly (bypassing the gate via a
	// gate-open window is not needed — we open it ourselves to plant the tamper).
	snap, ok, err := store.LatestSnapshot(ctx, res.StreamID)
	if err != nil || !ok {
		t.Fatalf("latest snapshot: ok=%v err=%v", ok, err)
	}
	tampered := append([]byte(nil), snap.State...)
	tampered[0] ^= 0xff
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET state = ? WHERE stream_id = ? AND covered_seq = ?`,
		tampered, res.StreamID, snap.CoveredSeq); err != nil {
		t.Fatalf("tamper: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit tamper: %v", err)
	}

	if _, err := engine.Resume(ctx, store, doc, res.StreamID, nil, engine.Options{}); !errors.Is(err, graphstore.ErrSnapshotHashMismatch) {
		t.Fatalf("resume of a corrupted snapshot = %v, want ErrSnapshotHashMismatch", err)
	}
}

// TestSnapshotDisabledIsInertAndFoldCompatible proves snapshots are opt-in and
// that the disabled path is behaviorally INERT: a run with SnapshotEvery 0 writes
// NO snapshot.anchored events and NO snapshots rows, so its event-TYPE sequence
// matches a P4.2 run. This is the honest invariant — NOT chain-byte-identity: an
// input-bearing run.started (input_hash) or a do effect.settled (node_outcome)
// carries P4.3 payload fields that shift chain hashes off a P4.2 binary (L1). The
// disabled journal still folds cleanly, which the Verify below proves.
func TestSnapshotDisabledIsInertAndFoldCompatible(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("diamond",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"A"}),
		execNode("D", `echo d`, []string{"B", "C"}),
	))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 0})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	for _, e := range res.Events {
		if e.Type == engine.EventSnapshotAnchored {
			t.Fatalf("a snapshot-disabled run emitted a snapshot.anchored event")
		}
	}
	var snapCount int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM snapshots`).Scan(&snapCount); err != nil {
		t.Fatalf("count snapshots: %v", err)
	}
	if snapCount != 0 {
		t.Fatalf("snapshot-disabled run wrote %d snapshots, want 0 (opt-in)", snapCount)
	}
	// Fold-compatible: the disabled journal still Verifies and rebuilds cleanly.
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Fatalf("Verify of a snapshot-disabled journal = %v", err)
	}
}

// TestResumeSettlesDoNodeFromRecordedEffectNoHostCall is the B1 proof: a crash
// in the window AFTER effect.settled committed but BEFORE outcome.settled did
// leaves the node unsettled while its effect is NOT interrupted. Resume must
// settle the node FROM the recorded effect result — never re-invoke the host —
// so the memoized effect discipline holds and no ErrIdemTokenReuse treadmill
// starts. Proven by the host call count (0) and the single settlement.
func TestResumeSettlesDoNodeFromRecordedEffectNoHostCall(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	engine.RegisterVocabulary(store)
	const stream = "gcg-run-b1settled00"

	doc := decodeIR(t, blockDoc("agentrun", doNode("work", "Do the work.", nil)))

	// Crash AFTER effect.settled but BEFORE outcome.settled: the effect result is
	// on disk (result ok, node_outcome pass, output recorded), the node is not.
	idem := stream + ":work:0:do:1"
	m := newManualRun(t, store, stream)
	m.append(engine.EventRunStarted, stream+":run:started", map[string]any{
		"root_id": stream, "name": "agentrun", "created_at": "2020-01-01T00:00:00Z",
	})
	m.append(engine.EventNodeActivated, stream+":work:0:act", map[string]any{
		"node_id": "work", "activation": "work:0", "kind": "do",
	})
	m.append(engine.EventEffectScheduled, idem+":sched", map[string]any{
		"activation": "work:0", "effect": "do", "idem_token": idem,
		"policy": "at_most_once", "spec_hash": "deadbeef",
		"spec": map[string]any{"prompt": "Do the work."},
	})
	m.append(engine.EventEffectSettled, idem+":done", map[string]any{
		"activation": "work:0", "idem_token": idem, "result": "ok",
		"node_outcome": "pass", "output": "recorded output",
	})
	m.close()

	// The host is scripted to PASS with a DIFFERENT output. If resume re-ran the
	// effect, the node would carry "should never run"; the memoized settlement
	// forbids that — it must reuse the recorded "recorded output".
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"work": {Outcome: enginehost.OutcomePass, Output: "should never run"},
	}}
	res, err := engine.Resume(ctx, store, doc, stream, nil, engine.Options{Host: stub})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(stub.Calls()) != 0 {
		t.Fatalf("host called %d times on resume, want 0 (settled-window effect is memoized, not re-run)", len(stub.Calls()))
	}
	if res.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass (settled from recorded effect)", res.Outcome)
	}
	if got := res.NodeOutputs["work"]; got != "recorded output" {
		t.Errorf("work output = %q, want the recorded effect output (never the host's)", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["work"] != engine.OutcomePass {
		t.Errorf("work settled %q, want pass", settled["work"])
	}
	full, err := store.ReadStream(ctx, stream, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := countSettlements(t, full, "work:0"); n != 1 {
		t.Errorf("work settlements = %d, want 1 (settled once, from the recorded effect)", n)
	}
	// Exactly one effect.settled survives — the memoized one, not a re-acted dup.
	if got := effectSettledResults(t, full); len(got) != 1 {
		t.Errorf("effect settled records = %v, want exactly 1 (no re-acted duplicate)", got)
	}
	if err := store.Verify(ctx, stream); err != nil {
		t.Errorf("Verify after resume: %v (a re-acted effect would have reused an idem token)", err)
	}
}

// TestResumeDoesNotReExecuteSettledCombineMember is the B2 proof: a crash
// mid-gather (after a combine member settled but before the gather itself
// settled) must NOT re-run the already-settled combine member on resume. The
// settled-reload guard lives inside runUnit, so it fires at combine nesting
// depth, not just at the top level. Proven by a zero host-call count for the
// combine `do` member and a single settlement for it.
func TestResumeDoesNotReExecuteSettledCombineMember(t *testing.T) {
	ctx := context.Background()
	store1 := newStore(t)

	doc := decodeIR(t, blockDoc("gatherrun",
		scatterNode("S", nil, "continue", settleNode("m1", "pass")),
		gatherNode("G", "S", []string{"S"}, doNode("combineDo", "Combine.", nil)),
	))
	host1 := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"combineDo": {Outcome: enginehost.OutcomePass, Output: "combined once"},
	}}
	res, err := engine.RunWithOptions(ctx, store1, doc, nil, engine.Options{Host: host1})
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}
	if len(host1.Calls()) != 1 {
		t.Fatalf("baseline host calls = %d, want 1", len(host1.Calls()))
	}

	// Cut the journal just before the gather G settled: the combine `do` has fully
	// settled (effect + outcome), the gather has not, run.closed is absent — the
	// mid-gather crash B2 targets.
	cut := indexOfSettlement(t, res.Events, "G:0")
	if cut <= 0 {
		t.Fatalf("no G:0 settlement found in baseline journal")
	}

	// Replay that prefix into a fresh store: a journal-only crash image (no Tier-A
	// projection), identical bytes so the chain and ir_hash match.
	store2 := newStore(t)
	replayPrefix(t, store2, res.StreamID, res.Events, cut)

	// Resume with a host that would re-run combineDo if reached. B2: it must not be.
	host2 := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"combineDo": {Outcome: enginehost.OutcomePass, Output: "SHOULD NOT RUN AGAIN"},
	}}
	resumed, err := engine.Resume(ctx, store2, doc, res.StreamID, nil, engine.Options{Host: host2})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	if len(host2.Calls()) != 0 {
		t.Fatalf("host called %d times on resume, want 0 (settled combine member must not re-execute)", len(host2.Calls()))
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	full, err := store2.ReadStream(ctx, res.StreamID, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if n := countSettlements(t, full, "combineDo:0"); n != 1 {
		t.Errorf("combineDo settlements = %d, want 1 (reloaded, not re-run)", n)
	}
	if got := effectSettledResults(t, full); len(got) != 1 {
		t.Errorf("effect settled records = %v, want exactly 1 (no re-acted duplicate)", got)
	}
	if got := settledOutcomeByID(t, full)["G"]; got != engine.OutcomePass {
		t.Errorf("gather G settled %q, want pass (completed from recorded member outcomes)", got)
	}
	if err := store2.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify after resume: %v", err)
	}
}

// TestResumeReconcilesProjectionForSealedRun is the H1 proof (and the L1/L3
// convergence assertion): a crash after run.closed committed to the journal but
// before its Tier-A projection commit leaves the root `open` and the frontier
// uncleared. Resume is the repair path — it must reconcile the projection even
// though the stream is sealed, not no-op. Convergence is asserted by a
// byte-identical dump against a clean genesis rebuild.
func TestResumeReconcilesProjectionForSealedRun(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	engine.RegisterVocabulary(store)
	const stream = "gcg-run-h1seal0000"

	doc := decodeIR(t, blockDoc("chain", execNode("A", `echo a`, nil)))

	// A fully sealed journal with NO projection applied (manualRun writes only the
	// log) — the exact state a crash between run.closed's append and its projection
	// leaves behind.
	m := newManualRun(t, store, stream)
	m.append(engine.EventRunStarted, stream+":run:started", map[string]any{
		"root_id": stream, "name": "chain", "created_at": "2020-01-01T00:00:00Z",
	})
	m.append(engine.EventNodeActivated, stream+":A:0:act", map[string]any{
		"node_id": "A", "activation": "A:0", "kind": "exec",
	})
	m.append(engine.EventOutcomeSettled, stream+":A:0:settled", map[string]any{
		"activation": "A:0", "outcome": "pass", "output": "a",
	})
	m.append(engine.EventRunClosed, stream+":run:closed", map[string]any{"outcome": "pass"})
	m.close()

	// Precondition: the projection really is unapplied (no node rows yet).
	var nodeRows int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM nodes WHERE stream_id = ?`, stream).Scan(&nodeRows); err != nil {
		t.Fatalf("count nodes: %v", err)
	}
	if nodeRows != 0 {
		t.Fatalf("precondition: %d node rows, want 0 (an unprojected sealed journal)", nodeRows)
	}

	resumed, err := engine.Resume(ctx, store, doc, stream, nil, engine.Options{})
	if err != nil {
		t.Fatalf("resume sealed: %v", err)
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	// Tier-A converged: the root is closed and the frontier is empty.
	if st := nodeStatus(t, store, stream); st != "done" {
		t.Errorf("root status after resume = %q, want done (projection reconciled)", st)
	}
	var frontierRows int
	if err := store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM frontier WHERE root_id = ?`, stream).Scan(&frontierRows); err != nil {
		t.Fatalf("count frontier: %v", err)
	}
	if frontierRows != 0 {
		t.Errorf("frontier rows after resume = %d, want 0 (frontier cleared at seal)", frontierRows)
	}
	// Convergence proof: the reconciled projection is byte-identical to a clean
	// genesis rebuild of the same journal.
	live := dumpTierA(t, store, stream)
	if err := store.RebuildTierA(ctx, engine.Reducer(), stream); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	if rebuilt := dumpTierA(t, store, stream); live != rebuilt {
		t.Fatalf("resumed projection not byte-identical to genesis rebuild:\n--- live ---\n%s\n--- rebuilt ---\n%s", live, rebuilt)
	}
}

// TestTruncateRefusesRottedCoveringSnapshot is the H2 proof: TruncateBelowAnchor
// must verify the covering snapshot's blob still hashes to its state_hash BEFORE
// deleting the prefix — a rotted blob is the only rebuild source once the prefix
// is gone, so a mismatch aborts the truncation. The prefix stays intact and
// resume still works.
func TestTruncateRefusesRottedCoveringSnapshot(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("chain",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"B"}),
	))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	snaps := allSnapshots(t, store, res.StreamID)
	if len(snaps) < 2 {
		t.Fatalf("snapshots = %d, want >= 2", len(snaps))
	}
	// Rot the blob of the OLDER snapshot we would truncate below (the latest stays
	// intact so resume can still fold from it).
	cut := snaps[len(snaps)-2].covered
	corruptSnapshotBlob(t, store, res.StreamID, cut)

	headBefore, _ := store.Head(ctx, res.StreamID)
	allBefore, _ := store.ReadStream(ctx, res.StreamID, 1, 0)

	if _, err := store.TruncateBelowAnchor(ctx, res.StreamID, cut); !errors.Is(err, graphstore.ErrSnapshotHashMismatch) {
		t.Fatalf("truncate over a rotted covering snapshot = %v, want ErrSnapshotHashMismatch", err)
	}

	// The prefix is untouched: nothing was deleted.
	if h, _ := store.Head(ctx, res.StreamID); h != headBefore {
		t.Errorf("head = %d after a refused truncate, want %d (prefix preserved)", h, headBefore)
	}
	if allAfter, _ := store.ReadStream(ctx, res.StreamID, 1, 0); len(allAfter) != len(allBefore) {
		t.Errorf("events = %d after a refused truncate, want %d (nothing deleted)", len(allAfter), len(allBefore))
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify after refused truncate = %v, want nil (chain intact)", err)
	}
	// Resume still works from the intact journal (using the latest, intact snapshot).
	resumed, err := engine.Resume(ctx, store, doc, res.StreamID, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("resume after refused truncate: %v", err)
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
}

// TestResumeCrossChecksSnapshotAgainstAnchor is the M1 proof: a self-consistent
// snapshot forgery (state', hash(state')) planted through the write gate passes
// the blob self-check, but its hash disagrees with the chain-anchored
// snapshot.anchored payload. Resume cross-checks the two and refuses the
// forgery with ErrSnapshotHashMismatch.
func TestResumeCrossChecksSnapshotAgainstAnchor(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("chain",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
	))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	snap, ok, err := store.LatestSnapshot(ctx, res.StreamID)
	if err != nil || !ok {
		t.Fatalf("latest snapshot: ok=%v err=%v", ok, err)
	}

	// Plant a SELF-CONSISTENT forgery: tamper the blob AND rewrite state_hash to
	// match it, so the self-check (Hash(blob) == state_hash) passes. Only the
	// chain-anchored snapshot.anchored payload still holds the true hash.
	tampered := append([]byte(nil), snap.State...)
	tampered[0] ^= 0xff
	forgedHash := canon.Hash(tampered)
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET state = ?, state_hash = ? WHERE stream_id = ? AND covered_seq = ?`,
		tampered, forgedHash[:], res.StreamID, snap.CoveredSeq); err != nil {
		t.Fatalf("plant forgery: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit forgery: %v", err)
	}

	if _, err := engine.Resume(ctx, store, doc, res.StreamID, nil, engine.Options{}); !errors.Is(err, graphstore.ErrSnapshotHashMismatch) {
		t.Fatalf("resume of a self-consistent forgery = %v, want ErrSnapshotHashMismatch (chain-anchor cross-check)", err)
	}
}

// TestResumeRefusesDifferentInput is the M2 proof: run.started pins an
// input_hash, and resuming with a different input silently changes every
// interpolation scope. Resume refuses a mismatch and admits the identical input.
func TestResumeRefusesDifferentInput(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("hello", execNode("greet", `echo hi`, nil)))
	res, err := engine.RunWithOptions(ctx, store, doc, map[string]any{"x": "a"}, engine.Options{})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if _, err := engine.Resume(ctx, store, doc, res.StreamID, map[string]any{"x": "b"}, engine.Options{}); !errors.Is(err, engine.ErrInputHashMismatch) {
		t.Fatalf("resume with a different input = %v, want ErrInputHashMismatch", err)
	}
	// The identical input resumes cleanly (a sealed no-op returning the outcome).
	resumed, err := engine.Resume(ctx, store, doc, res.StreamID, map[string]any{"x": "a"}, engine.Options{})
	if err != nil {
		t.Fatalf("resume with the same input: %v", err)
	}
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
}

// TestResumeMatchesGenesisRecordRuleForSkippedRef is the B1 proof: resume must
// reproduce the genesis record() rule EXACTLY, so a not-yet-run node that
// interpolates a SKIPPED upstream renders the same unresolved {{ref}} it did in
// the original run. X fails → Y (after X) skip-cascades → Z (no dep on Y)
// interpolates {{Y}}. Genesis leaves {{Y}} verbatim (a skipped node is never
// recorded into scope); a resume that seeds scope from EVERY settled node would
// instead render {{Y}} as "" and commit a DIVERGENT output. This pins byte
// identity of Z's rendered output and of NodeOutputs (which must omit skipped Y).
func TestResumeMatchesGenesisRecordRuleForSkippedRef(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("skipref",
		execNode("X", `exit 1`, nil),
		execNode("Y", `echo yran`, []string{"X"}),
		execNode("Z", `echo "Z sees {{Y}}"`, nil),
	))

	// Genesis run: capture Z's rendered output and NodeOutputs.
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("genesis run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["X"] != engine.OutcomeFailed || settled["Y"] != engine.OutcomeSkipped {
		t.Fatalf("genesis outcomes = %v, want X failed / Y skipped", settled)
	}
	const wantZ = "Z sees {{Y}}"
	if res.NodeOutputs["Z"] != wantZ {
		t.Fatalf("genesis Z output = %q, want %q ({{Y}} unresolved — Y was skipped)", res.NodeOutputs["Z"], wantZ)
	}
	if _, ok := res.NodeOutputs["Y"]; ok {
		t.Fatalf("genesis NodeOutputs unexpectedly records skipped Y = %q", res.NodeOutputs["Y"])
	}

	// Crash just before Z settled: X failed, Y skip-cascaded, Z activated but not
	// settled, run.closed absent. Replay that prefix into a fresh store.
	cut := indexOfSettlement(t, res.Events, "Z:0")
	if cut <= 0 {
		t.Fatalf("no Z:0 settlement in genesis journal")
	}
	store2 := newStore(t)
	replayPrefix(t, store2, res.StreamID, res.Events, cut)

	resumed, err := engine.Resume(ctx, store2, doc, res.StreamID, nil, engine.Options{})
	if err != nil {
		t.Fatalf("resume: %v", err)
	}
	// Z's rendered output is BYTE-IDENTICAL to genesis — {{Y}} stays unresolved.
	if got := resumed.NodeOutputs["Z"]; got != wantZ {
		t.Errorf("resumed Z output = %q, want byte-identical to genesis %q", got, wantZ)
	}
	// The resumed Z settlement on disk carries the identical output.
	full, err := store2.ReadStream(ctx, res.StreamID, 1, 0)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if got := settledOutputByID(t, full)["Z"]; got != wantZ {
		t.Errorf("resumed Z on-disk output = %q, want %q", got, wantZ)
	}
	// NodeOutputs match genesis exactly: skipped Y is absent from both.
	if _, ok := resumed.NodeOutputs["Y"]; ok {
		t.Errorf("resumed NodeOutputs records skipped Y = %q, want absent (genesis omits it)", resumed.NodeOutputs["Y"])
	}
	if !reflect.DeepEqual(res.NodeOutputs, resumed.NodeOutputs) {
		t.Errorf("resumed NodeOutputs = %v, want equal to genesis %v", resumed.NodeOutputs, res.NodeOutputs)
	}
	if err := store2.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify after resume: %v", err)
	}
}

// TestRebuildTierAFromSnapshotAfterTruncate is the H1 proof: RebuildTierA must
// reconstruct a retention-truncated stream's projection from its covering
// snapshot — the genesis prefix is gone, so a from-genesis fold would hit
// ErrNonContiguousTail. It seeds a snapshotting run, captures the live projection,
// rebuilds it from genesis (still intact) to confirm the untruncated path, then
// truncates below an anchor and drops+rebuilds from the snapshot. Both rebuilds
// must be byte-identical to the pre-truncation projection.
func TestRebuildTierAFromSnapshotAfterTruncate(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("chain",
		execNode("A", `echo a`, nil),
		execNode("B", `echo b`, []string{"A"}),
		execNode("C", `echo c`, []string{"B"}),
		execNode("D", `echo d`, []string{"C"}),
	))
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	r := engine.Reducer()

	// The complete, pre-truncation projection: truncation must not lose any of it.
	pre := dumpTierA(t, store, res.StreamID)

	// The from-genesis rebuild of the untruncated stream reproduces it (DET-T-17)
	// AND exercises the no-snapshot path still working with a snapshot present.
	if err := store.RebuildTierA(ctx, r, res.StreamID); err != nil {
		t.Fatalf("genesis rebuild (untruncated): %v", err)
	}
	if got := dumpTierA(t, store, res.StreamID); got != pre {
		t.Fatalf("genesis rebuild != live projection:\n--- live ---\n%s\n--- rebuilt ---\n%s", pre, got)
	}

	snaps := allSnapshots(t, store, res.StreamID)
	if len(snaps) < 2 {
		t.Fatalf("snapshots = %d, want >= 2 (cadence 1)", len(snaps))
	}
	// Keep the latest snapshot; cut below an older one (the retention policy).
	cut := snaps[len(snaps)-2].covered
	deleted, err := store.TruncateBelowAnchor(ctx, res.StreamID, cut)
	if err != nil {
		t.Fatalf("truncate below %d: %v", cut, err)
	}
	if deleted == 0 {
		t.Fatalf("truncate deleted 0 rows")
	}
	// Genesis is now gone: the surviving journal starts above seq 1.
	surviving, err := store.ReadStream(ctx, res.StreamID, 1, 0)
	if err != nil {
		t.Fatalf("read surviving: %v", err)
	}
	if surviving[0].Seq == 1 {
		t.Fatalf("stream not truncated (first surviving seq still 1)")
	}

	// Drop + snapshot-anchored refold reproduces the pre-truncation projection.
	if err := store.RebuildTierA(ctx, r, res.StreamID); err != nil {
		t.Fatalf("rebuild truncated stream: %v", err)
	}
	if got := dumpTierA(t, store, res.StreamID); got != pre {
		t.Fatalf("snapshot-anchored rebuild != pre-truncation projection:\n--- pre ---\n%s\n--- rebuilt ---\n%s", pre, got)
	}
}

// TestSnapshotNeverCoversInFlightEffect is the N2 assert: a snapshot is anchored
// only at a unit boundary, never inside runDo (between an effect's scheduled and
// settled). With aggressive snapshotting (cadence 1) over a do-formula, NO
// snapshot.anchored event may appear while an effect is open — pinning the
// "snapshot never covers an in-flight effect" invariant so future concurrency
// cannot break it silently.
func TestSnapshotNeverCoversInFlightEffect(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, blockDoc("agentwork",
		doNode("one", "Do one.", nil),
		doNode("two", "Do two.", []string{"one"}),
	))
	stub := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"one": {Outcome: enginehost.OutcomePass, Output: "1"},
		"two": {Outcome: enginehost.OutcomePass, Output: "2"},
	}}
	res, err := engine.RunWithOptions(ctx, store, doc, nil, engine.Options{Host: stub, SnapshotEvery: 1})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// Non-vacuous: snapshots actually happened and effects actually scheduled.
	if len(allSnapshots(t, store, res.StreamID)) == 0 {
		t.Fatalf("no snapshots written — the invariant would be vacuous")
	}
	inFlight := 0
	sawSchedule := false
	for _, e := range res.Events {
		switch e.Type {
		case engine.EventEffectScheduled:
			inFlight++
			sawSchedule = true
		case engine.EventEffectSettled:
			inFlight--
		case engine.EventSnapshotAnchored:
			if inFlight != 0 {
				t.Fatalf("snapshot.anchored at seq %d with %d effect(s) in flight — a snapshot must never cover a scheduled-but-unsettled effect", e.Seq, inFlight)
			}
		}
	}
	if !sawSchedule {
		t.Fatalf("no effect.scheduled events — the invariant would be vacuous")
	}
	if inFlight != 0 {
		t.Fatalf("journal ended with %d unpaired effect(s)", inFlight)
	}
	if err := store.Verify(ctx, res.StreamID); err != nil {
		t.Errorf("Verify: %v", err)
	}
}

// settledOutputByID returns, per bare node id, the Output of its outcome.settled.
func settledOutputByID(t *testing.T, events []graphstore.StoredEvent) map[string]string {
	t.Helper()
	out := map[string]string{}
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Output     string `json:"output"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		out[engine.ActivationNodeID(p.Activation)] = p.Output
	}
	return out
}

// --- manual-run helper (simulate a crashed partial journal) -----------------

type manualRun struct {
	t      *testing.T
	store  *graphstore.Store
	stream string
	lease  graphstore.WriterLease
	head   uint64
}

func newManualRun(t *testing.T, store *graphstore.Store, stream string) *manualRun {
	t.Helper()
	lease, err := store.AcquireWriterLease(context.Background(), stream, "manual", 30_000_000_000)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	return &manualRun{t: t, store: store, stream: stream, lease: lease}
}

func (m *manualRun) append(typ, idem string, payload map[string]any) {
	m.t.Helper()
	body, err := canonJSON(m.t, payload)
	if err != nil {
		m.t.Fatalf("canon %s: %v", typ, err)
	}
	res, err := m.store.Append(context.Background(), m.stream, "lumen", m.head, m.lease.Epoch, []graphstore.JournalEvent{{
		Type: typ, IRContractVersion: "0.2.5", IdemToken: idem, Payload: body,
	}})
	if err != nil {
		m.t.Fatalf("append %s: %v", typ, err)
	}
	m.head = res.FirstSeq
}

func (m *manualRun) close() {
	_ = m.store.ReleaseWriterLease(context.Background(), m.lease)
}

// indexOfSettlement returns the index of the outcome.settled event for the given
// activation, or -1 if absent. Used to cut a journal just before a settlement.
func indexOfSettlement(t *testing.T, events []graphstore.StoredEvent, activation string) int {
	t.Helper()
	for i, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			return i
		}
	}
	return -1
}

// replayPrefix writes events[:upto] verbatim into store under stream, producing
// a journal-only crash image (no Tier-A projection). The bytes are the committed
// payloads, so the recomputed chain matches the original and ir_hash is pinned.
func replayPrefix(t *testing.T, store *graphstore.Store, stream string, events []graphstore.StoredEvent, upto int) {
	t.Helper()
	ctx := context.Background()
	engine.RegisterVocabulary(store)
	lease, err := store.AcquireWriterLease(ctx, stream, "replay", 30_000_000_000)
	if err != nil {
		t.Fatalf("replay lease: %v", err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()
	var head uint64
	for i := 0; i < upto; i++ {
		e := events[i]
		res, err := store.Append(ctx, stream, "lumen", head, lease.Epoch, []graphstore.JournalEvent{{
			Substream: e.Substream, Type: e.Type, IRContractVersion: e.IRContractVersion,
			IdemToken: e.IdemToken, Payload: e.Payload,
		}})
		if err != nil {
			t.Fatalf("replay append %d (%s): %v", i, e.Type, err)
		}
		head = res.FirstSeq
	}
}

// corruptSnapshotBlob rots the state blob of the snapshot covering coveredSeq,
// through the snapshot write gate, so canon.Hash(state) no longer equals the
// stored state_hash.
func corruptSnapshotBlob(t *testing.T, store *graphstore.Store, stream string, coveredSeq uint64) {
	t.Helper()
	ctx := context.Background()
	snaps := allSnapshots(t, store, stream)
	var blob []byte
	for _, s := range snaps {
		if s.covered == coveredSeq {
			blob = append([]byte(nil), s.state...)
		}
	}
	if blob == nil {
		t.Fatalf("no snapshot covering %d", coveredSeq)
	}
	blob[0] ^= 0xff
	tx, err := store.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 1 WHERE singleton = 0`); err != nil {
		t.Fatalf("open gate: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshots SET state = ? WHERE stream_id = ? AND covered_seq = ?`,
		blob, stream, coveredSeq); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}
	if _, err := tx.ExecContext(ctx, `UPDATE snapshot_write_gate SET open = 0 WHERE singleton = 0`); err != nil {
		t.Fatalf("close gate: %v", err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatalf("commit corruption: %v", err)
	}
}

func countSettlements(t *testing.T, events []graphstore.StoredEvent, activation string) int {
	t.Helper()
	n := 0
	for _, e := range events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == activation {
			n++
		}
	}
	return n
}
