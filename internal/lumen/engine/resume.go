package engine

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// ErrIRHashMismatch is returned by Resume when the doc handed to it is not the
// formula that started the run: run.started pinned an ir_hash, and resuming a
// stream with a different formula would fold new events against a foreign DAG.
// It is a loud refusal before any append.
var ErrIRHashMismatch = errors.New("lumen: resume ir hash mismatch")

// ErrInputHashMismatch is returned by Resume when the input handed to it does
// not match the input run.started pinned (input_hash). Interpolation scope is
// seeded from the input, so resuming with a different input would silently
// change the meaning of every unresolved {{ref}}; it is a loud refusal before
// any append. A run started with an empty (unpinned) input imposes no
// constraint.
var ErrInputHashMismatch = errors.New("lumen: resume input hash mismatch")

// maybeSnapshot anchors a fold snapshot when the cadence says so (or force is
// set at the run seal), then folds the resulting snapshot.anchored event forward
// so the driver's state/head stay in lockstep with the journal. It is a no-op
// when snapshotting is disabled (snapshotEvery == 0), which keeps a non-opted run
// byte-identical to P4.2. Snapshots are taken only at unit boundaries — where the
// committed state is consistent and no effect is in flight — so a snapshot never
// covers a scheduled-but-unsettled effect.
func (d *driver) maybeSnapshot(force bool) error {
	if d.snapshotEvery <= 0 {
		return nil
	}
	if !force && d.sinceSnapshot < d.snapshotEvery {
		return nil
	}
	if d.head == 0 || d.st().Closed {
		return nil
	}

	// Crash boundary: a snapshot is due but not yet written (no snapshots row, no
	// snapshot.anchored event). Resume folds from the prior anchor, or genesis.
	if err := d.crashAt(crashBeforeSnapshot, d.streamID); err != nil {
		return err
	}

	st := d.st()
	blob, err := st.MarshalSnapshot()
	if err != nil {
		return fmt.Errorf("lumen: snapshot marshal at seq %d: %w", d.head, err)
	}
	stateHash := st.StateHash()
	snap := fold.Snapshot{
		StreamID:              d.streamID,
		CoveredSeq:            d.head,
		Engine:                Engine,
		ReducerVersion:        reducerVersion,
		SnapshotFormatVersion: snapshotFormatVersion,
		StateHash:             stateHash,
		State:                 blob,
	}
	body, err := canonPayload(snapshotAnchoredPayload{
		CoveredSeq: d.head,
		StateHash:  hex.EncodeToString(stateHash[:]),
	})
	if err != nil {
		return fmt.Errorf("lumen: snapshot anchor payload at seq %d: %w", d.head, err)
	}
	anchor := graphstore.JournalEvent{
		Type:              EventSnapshotAnchored,
		IRContractVersion: d.irVer,
		IdemToken:         fmt.Sprintf("%s:snap:%d", d.streamID, d.head),
		Payload:           body,
	}
	anchorSeq, err := d.store.WriteSnapshot(d.ctx, Engine, d.epoch, snap, anchor)
	if err != nil {
		return fmt.Errorf("lumen: write snapshot at seq %d: %w", d.head, err)
	}

	// Fold the anchored event forward (a no-op transition) so state and head track
	// the journal. Projecting the empty delta keeps the gate discipline identical
	// to the append loop.
	next, delta, err := d.reducer.Apply(d.state, fold.Event{
		StreamID:          d.streamID,
		Seq:               anchorSeq,
		Engine:            Engine,
		Type:              EventSnapshotAnchored,
		IRContractVersion: d.irVer,
		IdemToken:         anchor.IdemToken,
		Payload:           body,
	})
	if err != nil {
		return fmt.Errorf("lumen: fold snapshot anchor at seq %d: %w", anchorSeq, err)
	}
	d.state = next

	tx, err := d.store.DB().BeginTx(d.ctx, nil)
	if err != nil {
		return fmt.Errorf("lumen: begin snapshot projection tx: %w", err)
	}
	if err := d.store.ApplyDelta(d.ctx, tx, delta); err != nil {
		_ = tx.Rollback()
		return fmt.Errorf("lumen: project snapshot anchor: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lumen: commit snapshot projection: %w", err)
	}
	d.head = anchorSeq
	d.sinceSnapshot = 0
	// Crash boundary: the snapshot.anchored event and its state blob are durable —
	// resume loads this anchor and folds only the tail after it.
	return d.crashAt(crashAfterSnapshot, d.streamID)
}

// Resume restarts a crashed run of doc on streamID and drives it to completion
// (13-p4 §4). It loads the latest durable snapshot, folds ONLY the surviving tail
// after it to rebuild the run state (R-RESUME: the rebuilt projection is
// byte-identical to a genesis fold of the untruncated journal), reconstructs the
// interpolation scope from settled node outputs, settles any crash-interrupted
// effect as FAILED without re-invoking the agent (at-most-once, the P4.1 crash
// contract), then runs the remaining frontier and seals the run.
//
// EFFECT DISCIPLINE (M1): only `do` steps carry the effect.scheduled/settled
// record pair, so only `do` is at-MOST-once across a crash — a do whose effect
// was scheduled but not settled is settled FAILED without re-invoking the agent.
// `exec` steps have NO effect record: a crash after the shell ran but before
// outcome.settled committed leaves the exec unsettled, and resume re-runs it — so
// exec is at-LEAST-once across resume. That is safe for idempotent shells and is
// the current contract; extending the effect.scheduled/settled discipline to exec
// (making it at-most-once too) is a deferred follow-up.
//
// input re-supplies the run input (the journal pins the formula via ir_hash but
// does not carry the input). A doc whose ir_hash differs from the one run.started
// pinned is refused with ErrIRHashMismatch. A snapshot whose stored state hash
// does not match its blob is refused with ErrSnapshotHashMismatch — a corrupted
// anchor is detected, never silently trusted. Resuming an already-sealed stream
// is an idempotent no-op that returns the completed result.
func Resume(ctx context.Context, store *graphstore.Store, doc *ir.IR, streamID string, input map[string]any, opts Options) (RunResult, error) {
	if store == nil {
		return RunResult{}, fmt.Errorf("lumen: resume: nil store")
	}
	if doc == nil {
		return RunResult{}, fmt.Errorf("lumen: resume: nil IR document")
	}
	if streamID == "" {
		return RunResult{}, fmt.Errorf("lumen: resume: empty stream id")
	}

	units, err := buildUnits(doc.Nodes, opts.Host != nil)
	if err != nil {
		return RunResult{}, err
	}
	RegisterVocabulary(store)

	lease, err := store.AcquireWriterLease(ctx, streamID, leaseHolder, leaseTTL)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: resume: acquire writer lease %q: %w", streamID, err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()

	reducer := lumenReducer{}

	snap, hasSnap, err := store.LatestSnapshot(ctx, streamID)
	if err != nil {
		return RunResult{}, err
	}
	var (
		snapPtr *fold.Snapshot
		fromSeq uint64 = 1
	)
	if hasSnap {
		// A rotted snapshot blob is a loud refusal with no fallback: once the prefix
		// is truncated the snapshot is the only rebuild source, so the mitigation is
		// upstream — TruncateBelowAnchor re-hashes the covering blob BEFORE deleting
		// the prefix (H2), so a resume never faces a truncated stream whose only
		// anchor has rotted (M2).
		if canon.Hash(snap.State) != snap.StateHash {
			return RunResult{}, fmt.Errorf("lumen: resume %q: snapshot@%d: %w", streamID, snap.CoveredSeq, graphstore.ErrSnapshotHashMismatch)
		}
		snapPtr = &snap
		fromSeq = snap.CoveredSeq + 1
	}

	stored, err := store.ReadStream(ctx, streamID, fromSeq, 0)
	if err != nil {
		return RunResult{}, err
	}
	// Defense-in-depth (M1): the snapshot's blob is self-consistent above
	// (Hash(blob) == stored_hash), but a forgery (state', hash(state')) planted
	// through the snapshot write gate is self-consistent too. Cross-check the blob
	// hash against the snapshot.anchored event's state_hash at covered_seq+1 — that
	// event is in the hash chain (Verify protects it), so a blob that disagrees
	// with it is corruption, never a valid resume anchor.
	if hasSnap {
		if err := crossCheckAnchor(stored, snap); err != nil {
			return RunResult{}, fmt.Errorf("lumen: resume %q: %w", streamID, err)
		}
	}
	tail := make([]fold.Event, len(stored))
	for i, e := range stored {
		tail[i] = storedToFoldEvent(e)
	}
	state, tailDeltas, err := fold.Fold(reducer, snapPtr, tail)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: resume %q: fold: %w", streamID, err)
	}
	ls := state.(*lumenState)
	if ls.RootID == "" {
		return RunResult{}, fmt.Errorf("lumen: resume %q: no run.started in journal — nothing to resume", streamID)
	}
	if ls.IRHash != "" && ls.IRHash != irHash(doc) {
		return RunResult{}, fmt.Errorf("lumen: resume %q: journal ir_hash %s != doc %s: %w", streamID, ls.IRHash, irHash(doc), ErrIRHashMismatch)
	}
	// Pin the input (M2): a run.started that pinned an input_hash may only resume
	// with the same input, so interpolation scope is identical. An unpinned run
	// (empty input at start) imposes no constraint.
	if ls.InputHash != "" && ls.InputHash != inputHash(input) {
		return RunResult{}, fmt.Errorf("lumen: resume %q: journal input_hash %s != input %s: %w", streamID, ls.InputHash, inputHash(input), ErrInputHashMismatch)
	}

	head, err := store.Head(ctx, streamID)
	if err != nil {
		return RunResult{}, err
	}

	// NOTE (deferred P4.3 red-team follow-ups, lower severity):
	//   - L4: the reducer-version gate on a LOADED snapshot IS enforced — fold.Fold
	//     rejects a snapshot whose stamped reducer_version differs from the running
	//     reducer with ErrReducerVersionSkew (fold.go), so a reducer bump strands an
	//     old snapshot loudly rather than folding it best-effort.
	//   - L5: snapshot retention is caller-driven (TruncateBelowAnchor); no
	//     automatic cadence prunes superseded snapshots.
	//   - N1: the folded Tier-A frontier remains observer-only — the single-writer
	//     resume drives its own topo loop and never reads it for execution control.

	// A committed effect/outcome event with a malformed payload is journal
	// corruption, not a droppable row: surface it loudly rather than silently
	// skipping a settlement (which would re-act a memoized effect or re-run a
	// settled node) (L2).
	crashInterrupted, err := interruptedEffects(stored)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: resume %q: %w", streamID, err)
	}
	recordedEffects, err := settledEffects(stored)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: resume %q: %w", streamID, err)
	}

	d := &driver{
		ctx:              ctx,
		store:            store,
		streamID:         streamID,
		irVer:            doc.Contract.Version,
		epoch:            lease.Epoch,
		reducer:          reducer,
		state:            state,
		head:             head,
		host:             opts.Host,
		snapshotEvery:    opts.SnapshotEvery,
		crashInterrupted: crashInterrupted,
		settledEffects:   recordedEffects,
	}

	// Reconstruct the resume seeds from the settled nodes carried in the rebuilt
	// state, replaying the genesis record() rule so a resumed step's {{ref}}
	// interpolation sees exactly the upstream outputs genesis did — no more (a
	// skipped node / aggregate is NOT seeded), no less (B1). NodeOutputs and the
	// scope are seeded separately: aggregates land in NodeOutputs but never scope.
	nodeOutputs, scopeSeed := reconstructOutputs(ls)
	scope := baseScope(input)
	for id, out := range scopeSeed {
		scope[id] = out
	}

	// Sync the Tier-A projection to the journal. The driver appends an event and
	// projects it in two transactions, so a crash can land between them, leaving a
	// committed event unprojected; re-applying the folded tail deltas (idempotent
	// upserts) closes that window and projects any prefix a crash left entirely
	// unprojected. Snapshot-covered events were projected at their unit boundary,
	// so re-applying only the tail suffices.
	//
	// This runs BEFORE the sealed early-return (H1): a crash after run.closed's
	// journal append but before its projection commit leaves Tier-A with the root
	// still `open` and the frontier uncleared. Resume IS the repair path for that
	// window, so a sealed stream must still reconcile its projection — returning a
	// no-op here would strand the projection forever.
	if err := d.reapplyDeltas(tailDeltas); err != nil {
		return RunResult{}, err
	}

	// Already sealed before the crash: resume is a no-op read of the finished run
	// (its projection is now reconciled above). Events is the full surviving journal
	// (from seq 1), not just the post-snapshot tail (L3).
	if ls.Closed {
		full, err := store.ReadStream(ctx, streamID, 1, 0)
		if err != nil {
			return RunResult{}, fmt.Errorf("lumen: resume %q: read stream: %w", streamID, err)
		}
		return RunResult{StreamID: streamID, Outcome: ls.Outcome, NodeOutputs: nodeOutputs, Events: full}, nil
	}

	// Drive every unit through the SAME path as a fresh run. The reload / settle /
	// at-most-once memoization lives inside runUnit (keyed on the crashInterrupted
	// and settledEffects maps wired above), so an already-settled unit is reloaded
	// and a crash-interrupted effect is settled without re-acting — at any nesting
	// level, combine members included (B1/B2).
	for i := range units {
		if err := d.runUnit(units[i], scope, nodeOutputs); err != nil {
			return RunResult{}, err
		}
		if err := d.maybeSnapshot(false); err != nil {
			return RunResult{}, err
		}
	}

	runOutcome := d.st().runOutcome()
	if err := d.maybeSnapshot(true); err != nil {
		return RunResult{}, err
	}
	if err := d.append(EventRunClosed, streamID+":run:closed", runClosedPayload{Outcome: runOutcome}); err != nil {
		return RunResult{}, err
	}

	// Events is the full surviving journal (from seq 1), not just the post-snapshot
	// tail read from fromSeq (L3).
	events, err := store.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		return RunResult{}, fmt.Errorf("lumen: resume %q: read stream: %w", streamID, err)
	}
	return RunResult{StreamID: streamID, Outcome: runOutcome, NodeOutputs: nodeOutputs, Events: events}, nil
}

// reapplyDeltas re-projects a run's folded tail deltas to Tier-A in one
// transaction, syncing the projection to the journal on resume. The upserts are
// idempotent, so re-applying already-projected events is a no-op and an
// unprojected prefix is filled in.
func (d *driver) reapplyDeltas(deltas []fold.Delta) error {
	if len(deltas) == 0 {
		return nil
	}
	tx, err := d.store.DB().BeginTx(d.ctx, nil)
	if err != nil {
		return fmt.Errorf("lumen: resume: begin reprojection tx: %w", err)
	}
	for i := range deltas {
		if err := d.store.ApplyDelta(d.ctx, tx, deltas[i]); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("lumen: resume: reproject delta %d: %w", i, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("lumen: resume: commit reprojection: %w", err)
	}
	return nil
}

// resumeMemoized settles u from the journal without re-acting, when resume can
// prove the work already happened. It returns handled=true when it consumed the
// unit (the caller must not run it). Three cases, in order:
//
//   - the unit already settled (outcome.settled committed): reload its output;
//   - a do effect settled but its outcome.settled never committed (a crash in
//     the effect.settled -> outcome.settled window, B1): settle the node from
//     the recorded effect result — the host is NOT re-invoked;
//   - an effect was scheduled but never settled (a crash mid-effect): settle it
//     FAILED under at-most-once (settleInterruptedEffect).
//
// On a fresh run crashInterrupted and settledEffects are nil and no node is
// pre-settled, so this always returns handled=false and is a pure no-op.
func (d *driver) resumeMemoized(u planUnit, scope, nodeOutputs map[string]string) (bool, error) {
	if n, ok := d.st().Nodes[u.activation]; ok && n.Settled {
		// Reproduce the genesis record() rule for an already-settled unit WITHOUT
		// re-running it (B1): a skip-cascaded node recorded nothing, so seed neither
		// map; a leaf that ran seeded scope and nodeOutputs; a scatter/gather
		// aggregate that drained seeded nodeOutputs only, never the scope.
		if ranOutcome(n.Outcome) {
			nodeOutputs[u.nodeID] = n.Output
			if u.kind == unitLeaf {
				scope[u.nodeID] = n.Output
			}
		}
		return true, nil
	}
	if es, ok := d.settledEffects[u.activation]; ok {
		if err := d.appendSettled(u.activation, nodeOutcomeFor(es), es.Output, es.Detail); err != nil {
			return true, err
		}
		d.record(u.nodeID, es.Output, scope, nodeOutputs)
		return true, nil
	}
	if tok, ok := d.crashInterrupted[u.activation]; ok {
		if err := d.settleInterruptedEffect(u, tok); err != nil {
			return true, err
		}
		scope[u.nodeID] = ""
		nodeOutputs[u.nodeID] = ""
		return true, nil
	}
	return false, nil
}

// nodeOutcomeFor resolves the node outcome to settle from a recorded
// effect.settled. It prefers the explicitly memoized NodeOutcome (B1); it falls
// back to deriving one from the effect Result for a legacy record that predates
// that field. The fallback collapses ok to pass — it cannot recover a degraded
// distinction the record never carried — but never silently passes a failure.
func nodeOutcomeFor(es effectSettledPayload) string {
	if es.NodeOutcome != "" {
		return es.NodeOutcome
	}
	switch es.Result {
	case EffectResultOK:
		return OutcomePass
	default:
		return OutcomeFailed
	}
}

// settleInterruptedEffect closes a crash-interrupted effect for u's activation:
// it appends effect.settled{interrupted} and outcome.settled{failed} WITHOUT
// calling the host, honoring the at-most-once contract (a scheduled effect is
// never re-acted on resume). The idem tokens match the normal-path tokens, so a
// double resume dedupes rather than double-settling.
func (d *driver) settleInterruptedEffect(u planUnit, idemToken string) error {
	if err := d.append(EventEffectSettled, idemToken+":done", effectSettledPayload{
		Activation:  u.activation,
		IdemToken:   idemToken,
		Result:      EffectResultInterrupted,
		NodeOutcome: OutcomeFailed,
		Detail:      "effect_interrupted: crash before settlement; not re-run under at_most_once",
	}); err != nil {
		return err
	}
	return d.appendSettled(u.activation, OutcomeFailed, "", "effect_interrupted: not re-run under at_most_once")
}

// settledEffects returns, per activation, the effect.settled record of an effect
// that settled but whose paired outcome.settled never committed — the crash
// window B1 targets. It scans the raw tail (effect events fold to no-ops, so
// they are not in the reducer state) and drops any effect whose activation also
// carries an outcome.settled, leaving only the settlements a resume must apply.
func settledEffects(stored []graphstore.StoredEvent) (map[string]effectSettledPayload, error) {
	settled := map[string]effectSettledPayload{} // activation -> effect.settled
	outcome := map[string]bool{}                 // activation -> outcome.settled present
	for _, e := range stored {
		switch e.Type {
		case EventEffectSettled:
			var p effectSettledPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("decode effect.settled at seq %d: %w", e.Seq, err)
			}
			settled[p.Activation] = p
		case EventOutcomeSettled:
			var p outcomeSettledPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("decode outcome.settled at seq %d: %w", e.Seq, err)
			}
			outcome[p.Activation] = true
		}
	}
	out := map[string]effectSettledPayload{}
	for act, p := range settled {
		if !outcome[act] {
			out[act] = p
		}
	}
	return out, nil
}

// crossCheckAnchor verifies a loaded snapshot's blob hash against the
// chain-anchored snapshot.anchored event at covered_seq+1 (M1). The event's
// state_hash is protected by the journal hash chain, so it is the trustworthy
// witness for the blob a self-consistent forgery could otherwise impersonate. A
// missing anchor, a wrong event type at that seq, or a hash disagreement is
// corruption, reported as ErrSnapshotHashMismatch.
func crossCheckAnchor(tail []graphstore.StoredEvent, snap fold.Snapshot) error {
	anchorSeq := snap.CoveredSeq + 1
	want := hex.EncodeToString(snap.StateHash[:])
	for _, e := range tail {
		if e.Seq != anchorSeq {
			continue
		}
		if e.Type != EventSnapshotAnchored {
			return fmt.Errorf("snapshot@%d: event at anchor seq %d is %q, not %s: %w",
				snap.CoveredSeq, anchorSeq, e.Type, EventSnapshotAnchored, graphstore.ErrSnapshotHashMismatch)
		}
		var p snapshotAnchoredPayload
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			return fmt.Errorf("snapshot@%d: decode anchor payload: %w", snap.CoveredSeq, err)
		}
		if p.StateHash != want {
			return fmt.Errorf("snapshot@%d: blob hash %s != chain-anchored %s: %w",
				snap.CoveredSeq, want, p.StateHash, graphstore.ErrSnapshotHashMismatch)
		}
		return nil
	}
	return fmt.Errorf("snapshot@%d: no snapshot.anchored event at seq %d: %w",
		snap.CoveredSeq, anchorSeq, graphstore.ErrSnapshotHashMismatch)
}

// interruptedEffects returns, per activation, the idem token of an effect that
// was scheduled but whose settlement never committed — the crash-interrupted
// set. Effect events are no-op folds (they do not live in the reducer state), so
// this scans the raw tail; snapshots are taken only at unit boundaries, so an
// in-flight effect is always in the surviving tail.
func interruptedEffects(stored []graphstore.StoredEvent) (map[string]string, error) {
	scheduled := map[string]string{} // activation -> idem token
	settled := map[string]bool{}     // idem token settled
	for _, e := range stored {
		switch e.Type {
		case EventEffectScheduled:
			var p effectScheduledPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("decode effect.scheduled at seq %d: %w", e.Seq, err)
			}
			scheduled[p.Activation] = p.IdemToken
		case EventEffectSettled:
			var p effectSettledPayload
			if err := json.Unmarshal(e.Payload, &p); err != nil {
				return nil, fmt.Errorf("decode effect.settled at seq %d: %w", e.Seq, err)
			}
			settled[p.IdemToken] = true
		}
	}
	out := map[string]string{}
	for act, tok := range scheduled {
		if !settled[tok] {
			out[act] = tok
		}
	}
	return out, nil
}

// reconstructOutputs replays the genesis record() rule over a rebuilt run state,
// returning the resume seeds for RunResult.NodeOutputs and for the interpolation
// scope — which are NOT the same set. Genesis records a unit ONLY when it actually
// RAN: a skip-cascaded node (blocked, or an all-skipped drain aggregate) settles
// `skipped` WITHOUT record(), so it seeds neither map; a leaf that ran seeds both
// scope and nodeOutputs with its output; a scatter/gather aggregate that drained
// writes nodeOutputs (its empty aggregate output) but NEVER the scope — exactly as
// runScatter/runGather do. Reproducing this split keeps a resumed run
// byte-identical to genesis: a not-yet-run node interpolating a skipped node or an
// aggregate renders the same unresolved {{ref}} it would have in the original run,
// and NodeOutputs omits the skipped ids genesis omits (B1).
func reconstructOutputs(s *lumenState) (nodeOutputs, scope map[string]string) {
	nodeOutputs = map[string]string{}
	scope = map[string]string{}
	for _, k := range s.activationKeys() {
		n := s.Nodes[k]
		if !n.Settled || !ranOutcome(n.Outcome) {
			continue // skip-cascaded / canceled: genesis recorded nothing
		}
		nodeOutputs[n.NodeID] = n.Output
		if !isAggregateKind(n.Kind) {
			scope[n.NodeID] = n.Output
		}
	}
	return nodeOutputs, scope
}

// isAggregateKind reports whether an IR node kind is a drain aggregate
// (scatter / gather), whose settled output genesis writes to nodeOutputs (always
// "") but never seeds into the interpolation scope.
func isAggregateKind(kind string) bool {
	return kind == string(ir.NodeScatter) || kind == string(ir.NodeGather)
}

// storedToFoldEvent projects a committed journal row onto the I/O-free fold view.
func storedToFoldEvent(e graphstore.StoredEvent) fold.Event {
	return fold.Event{
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
