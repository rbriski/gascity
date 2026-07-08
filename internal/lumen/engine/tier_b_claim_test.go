package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

const (
	tierBStream    = "gcg-run-tierb0"
	tierBCreatedAt = "2026-07-08T00:00:00Z"
	tierBNodeID    = "summarize"
)

// materializeTierB seeds a fresh run and one pool-mode work bead, returning its
// activation key. Every test starts here.
func materializeTierB(t *testing.T, store *graphstore.Store) string {
	t.Helper()
	act, err := engine.MaterializeTierBWork(context.Background(), store, tierBStream, engine.TierBWorkSpec{
		RunName:   "review",
		CreatedAt: tierBCreatedAt,
		NodeID:    tierBNodeID,
		Kind:      "do",
	})
	if err != nil {
		t.Fatalf("materialize: %v", err)
	}
	return act
}

// projectedNode reads the fold-owned projection row a `bd ready`/`bd show` would
// see for the work bead: status, assignee, and the dispatch_mode metadata marker.
func projectedNode(t *testing.T, store *graphstore.Store) (status, assignee, dispatchMode string, present bool) {
	t.Helper()
	ctx := context.Background()
	id := tierBNodeID
	err := store.DB().QueryRowContext(ctx,
		`SELECT status, COALESCE(assignee,'') FROM nodes WHERE id = ? AND fold_owned = 1`, id).
		Scan(&status, &assignee)
	if err != nil {
		return "", "", "", false
	}
	_ = store.DB().QueryRowContext(ctx,
		`SELECT value FROM node_metadata WHERE node_id = ? AND key = 'dispatch_mode'`, id).Scan(&dispatchMode)
	return status, assignee, dispatchMode, true
}

// lumenEventTypes reads the stream through the beads.Store journal capability
// (AppendLogStore) — proving the claim/close reflect on the bead-compatible
// surface — and returns the ordered event types plus a by-type payload index.
func lumenEventTypes(t *testing.T, js beads.Store, streamID string) ([]string, map[string][]byte) {
	t.Helper()
	logStore, ok := beads.AppendLogStoreFor(js)
	if !ok {
		t.Fatalf("journal store does not expose AppendLogStore")
	}
	events, err := logStore.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("ReadStream: %v", err)
	}
	var types []string
	byType := map[string][]byte{}
	for _, e := range events {
		types = append(types, e.Type)
		byType[e.Type] = e.Payload
	}
	return types, byType
}

// TestTierBMaterializeProjectsClaimableWorkBead proves a pool-mode node
// materializes as an OPEN, claimable, fold-owned (write-closed) work bead on the
// journal root, and that run.started + node.activated are the only journal facts.
func TestTierBMaterializeProjectsClaimableWorkBead(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	if act != "summarize:0" {
		t.Fatalf("activation = %q, want summarize:0", act)
	}

	status, assignee, dm, present := projectedNode(t, store)
	if !present {
		t.Fatal("work bead was not projected")
	}
	if status != "open" || assignee != "" || dm != engine.DispatchModePool {
		t.Fatalf("projected {status:%q assignee:%q dispatch_mode:%q}, want {open, \"\", pool}", status, assignee, dm)
	}

	// It is fold-owned (write-closed), not a façade row.
	var foldOwned int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT fold_owned FROM nodes WHERE id = 'summarize'`).Scan(&foldOwned); err != nil {
		t.Fatalf("read fold_owned: %v", err)
	}
	if foldOwned != 1 {
		t.Fatalf("work bead fold_owned = %d, want 1 (Tier-A write-closed)", foldOwned)
	}

	js := beads.NewJournalStore(store)
	types, _ := lumenEventTypes(t, js, tierBStream)
	if want := []string{engine.EventRunStarted, engine.EventNodeActivated}; !reflect.DeepEqual(types, want) {
		t.Fatalf("journal = %v, want %v", types, want)
	}
	if err := store.Verify(context.Background(), tierBStream); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestTierBClaimIsCasAppendPureFold proves a claim translates into a CAS
// owned.admitted append whose fold (assignee/in_progress) is projected — through
// the beads.Store surface — and that the projection is a pure fold, not a raw
// column write.
func TestTierBClaimIsCasAppendPureFold(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)

	if err := engine.ClaimTierBWork(context.Background(), store, tierBStream, act, "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	status, assignee, _, _ := projectedNode(t, store)
	if status != engine.StatusClaimed || assignee != "worker-a" {
		t.Fatalf("after claim {status:%q assignee:%q}, want {in_progress, worker-a}", status, assignee)
	}

	js := beads.NewJournalStore(store)
	types, byType := lumenEventTypes(t, js, tierBStream)
	if types[len(types)-1] != engine.EventOwnedAdmitted {
		t.Fatalf("last journal event = %q, want owned.admitted", types[len(types)-1])
	}
	var admitted struct {
		Kind     string `json:"kind"`
		Assignee string `json:"assignee"`
	}
	if err := json.Unmarshal(byType[engine.EventOwnedAdmitted], &admitted); err != nil {
		t.Fatalf("decode owned.admitted: %v", err)
	}
	if admitted.Kind != engine.OwnedKindTierB || admitted.Assignee != "worker-a" {
		t.Fatalf("owned.admitted = %+v, want {tier_b, worker-a}", admitted)
	}

	// Pure fold: the projection equals a from-scratch drop+refold of the journal.
	assertProjectionEqualsRefold(t, store, tierBStream)
}

// TestTierBCloseIsSettledAppend proves the worker's close translates into an
// owned.settled append that folds the bead to its terminal outcome status, with
// the claimant retained for provenance.
func TestTierBCloseIsSettledAppend(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()
	if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "3 bullets"); err != nil {
		t.Fatalf("settle: %v", err)
	}

	status, assignee, _, _ := projectedNode(t, store)
	if status != "done" || assignee != "worker-a" {
		t.Fatalf("after settle {status:%q assignee:%q}, want {done, worker-a}", status, assignee)
	}

	js := beads.NewJournalStore(store)
	types, byType := lumenEventTypes(t, js, tierBStream)
	if types[len(types)-1] != engine.EventOwnedSettled {
		t.Fatalf("last journal event = %q, want owned.settled", types[len(types)-1])
	}
	var settled struct {
		Outcome string `json:"outcome"`
		Output  string `json:"output"`
	}
	if err := json.Unmarshal(byType[engine.EventOwnedSettled], &settled); err != nil {
		t.Fatalf("decode owned.settled: %v", err)
	}
	if settled.Outcome != engine.OutcomePass || settled.Output != "3 bullets" {
		t.Fatalf("owned.settled = %+v, want {pass, 3 bullets}", settled)
	}
	assertProjectionEqualsRefold(t, store, tierBStream)
	if err := store.Verify(ctx, tierBStream); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestTierBConcurrentClaimCasLoud is the S0.4 kill for claims: two workers race
// to claim one bead, EXACTLY one wins, and the loser gets a loud, typed error —
// never a silent overwrite of the winner's assignee.
func TestTierBConcurrentClaimCasLoud(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, 2)
	assignees := []string{"worker-a", "worker-b"}
	for i := range assignees {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = engine.ClaimTierBWork(ctx, store, tierBStream, act, assignees[i])
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		if err == nil {
			winners++
			continue
		}
		// A loser must fail loudly: either it lost the append CAS, or it observed
		// the winner's claim first. Both are typed and non-destructive.
		if !errors.Is(err, engine.ErrTierBClaimConflict) && !errors.Is(err, engine.ErrTierBAlreadyClaimed) {
			t.Fatalf("loser %s got %v, want ErrTierBClaimConflict or ErrTierBAlreadyClaimed", assignees[i], err)
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1", winners)
	}

	// The projection reflects a single winner — no silent overwrite.
	status, assignee, _, _ := projectedNode(t, store)
	if status != engine.StatusClaimed || (assignee != "worker-a" && assignee != "worker-b") {
		t.Fatalf("after race {status:%q assignee:%q}, want in_progress + one winner", status, assignee)
	}
	// Exactly one owned.admitted committed (write-once per handle).
	if got := countJournalType(t, store, tierBStream, engine.EventOwnedAdmitted); got != 1 {
		t.Fatalf("owned.admitted rows = %d, want exactly 1 (write-once claim)", got)
	}
	assertProjectionEqualsRefold(t, store, tierBStream)
}

// TestTierBLateClaimAndIdempotentReclaim proves a claim of an already-held bead
// is rejected loudly (no overwrite), while an honest re-claim by the SAME worker
// is idempotent.
func TestTierBLateClaimAndIdempotentReclaim(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()
	if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
		t.Fatalf("claim a: %v", err)
	}
	// A different worker is rejected loudly, and does not overwrite the assignee.
	err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-b")
	if !errors.Is(err, engine.ErrTierBAlreadyClaimed) {
		t.Fatalf("late claim err = %v, want ErrTierBAlreadyClaimed", err)
	}
	if _, assignee, _, _ := projectedNode(t, store); assignee != "worker-a" {
		t.Fatalf("assignee = %q after rejected claim, want worker-a (no overwrite)", assignee)
	}
	// The original worker re-claiming is idempotent success (byte-identical replay).
	if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
		t.Fatalf("idempotent re-claim: %v", err)
	}
	if got := countJournalType(t, store, tierBStream, engine.EventOwnedAdmitted); got != 1 {
		t.Fatalf("owned.admitted rows = %d after re-claim, want 1 (deduped)", got)
	}
}

// TestTierBDoubleClaimIsWriteOnceLoudAtSubstrate isolates the loud CAS at the
// journal substrate: two divergent owned.admitted appends at the same head — the
// exact concurrent-claim collision — and the second is rejected with a typed
// journal conflict, never a silent second claim.
func TestTierBDoubleClaimIsWriteOnceLoudAtSubstrate(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()
	engine.RegisterVocabulary(store)

	head, err := store.Head(ctx, tierBStream)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	mk := func(assignee string) graphstore.JournalEvent {
		body, _ := json.Marshal(map[string]any{"handle": act, "activation": act, "kind": engine.OwnedKindTierB, "assignee": assignee})
		canonBody := mustCanon(t, body)
		return graphstore.JournalEvent{Type: engine.EventOwnedAdmitted, IRContractVersion: "0.2.5", IdemToken: "tier-b-claim:" + act, Payload: canonBody}
	}
	if _, err := store.Append(ctx, tierBStream, engine.Engine, head, 0, []graphstore.JournalEvent{mk("worker-a")}); err != nil {
		t.Fatalf("first claim append: %v", err)
	}
	// Second claim, different worker, SAME head and SAME idem token: the substrate
	// rejects it loudly rather than committing a second, silent claim.
	_, err = store.Append(ctx, tierBStream, engine.Engine, head, 0, []graphstore.JournalEvent{mk("worker-b")})
	if !errors.Is(err, graphstore.ErrIdemTokenReuse) && !errors.Is(err, graphstore.ErrWrongExpectedVersion) {
		t.Fatalf("second claim err = %v, want ErrIdemTokenReuse or ErrWrongExpectedVersion", err)
	}
}

// TestTierBDropRefoldIdentity is the DROP+refold proof for the claim/settle arms:
// the incremental fold and the full-state ProjectDelta produce identical Tier-A
// rows (DET-T-17), so the projection is a pure fold with no hidden state.
func TestTierBDropRefoldIdentity(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()
	if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "done"); err != nil {
		t.Fatalf("settle: %v", err)
	}

	// Store-level: a second drop+refold reproduces the projection byte-identically.
	before := projectionSnapshot(t, store, tierBStream)
	if err := store.RebuildTierA(ctx, engine.Reducer(), tierBStream); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after := projectionSnapshot(t, store, tierBStream)
	if before != after {
		t.Fatalf("drop+refold changed the projection:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// Reducer-level: the sum of the incremental deltas equals the full-state
	// projection, proving the claim/settle arms carry no state ProjectDelta misses.
	events := readFoldEvents(t, store, tierBStream)
	state, deltas, err := fold.Fold(engine.Reducer(), nil, events)
	if err != nil {
		t.Fatalf("fold: %v", err)
	}
	projector, ok := state.(fold.SnapshotProjector)
	if !ok {
		t.Fatal("lumen state is not a SnapshotProjector")
	}
	full := projector.ProjectDelta(tierBStream)
	inc := collapseNodeUpserts(deltas)
	fullNodes := collapseNodeUpserts([]fold.Delta{full})
	if !reflect.DeepEqual(inc, fullNodes) {
		t.Fatalf("incremental node projection != full ProjectDelta:\nincremental=%+v\nfull=%+v", inc, fullNodes)
	}
}

// TestTierBLateClaimAfterSettleDoesNotPoison is the HIGH-1 reducer-totality proof:
// SettleTierBWork is public + unfenced, so a claim can race an unclaimed settle and
// commit its CAS owned.admitted AFTER owned.settled at the substrate. The fold must
// stay TOTAL over that legal sequence — a reducer error here would make RebuildTierA
// (and Resume) fail FOREVER, permanently poisoning the stream. Instead the late
// claim loses: the settle stands, the claim folds to a defined no-op, and the
// projection stays consistent.
func TestTierBLateClaimAfterSettleDoesNotPoison(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()

	// Settle the (unclaimed) pool bead through the public, unfenced API.
	if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "done"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// The racing claim: it read head AFTER the settle committed, so its CAS append
	// lands at the current head, AFTER owned.settled — the exact interleaving the
	// unfenced settle admits. (Committed directly at the substrate because a
	// post-settle ClaimTierBWork would now be rejected at the read guard.)
	appendRaw(t, store, tierBStream, engine.EventOwnedAdmitted, engine.OwnedKindTierB+"-claim:"+act, map[string]any{
		"handle": act, "activation": act, "kind": engine.OwnedKindTierB, "assignee": "worker-late",
	})

	// TOTALITY: the fold must not error — RebuildTierA succeeds on the legal sequence.
	if err := store.RebuildTierA(ctx, engine.Reducer(), tierBStream); err != nil {
		t.Fatalf("RebuildTierA poisoned by a late claim after settle: %v", err)
	}
	// Late claim lost: the settle stands, no assignee-overwrite of the settled node.
	status, assignee, _, _ := projectedNode(t, store)
	if status != "done" || assignee != "" {
		t.Fatalf("after late claim {status:%q assignee:%q}, want {done, \"\"} (settle stands, claim lost)", status, assignee)
	}
	// And a further drop+refold is stable.
	assertProjectionEqualsRefold(t, store, tierBStream)
}

// TestTierBDoubleSettleFoldsIdempotent proves a redundant owned.settled reaching
// the fold (e.g. a re-sliced or crafted stream) folds to a defined idempotent
// no-op — the first settle stands and RebuildTierA never errors (HIGH-1 totality
// for the settle arm).
func TestTierBDoubleSettleFoldsIdempotent(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()
	if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "first"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// A second owned.settled for the same handle under a DIFFERENT idem token (the
	// canonical token would dedupe / ErrIdemTokenReuse at the append). It reaches
	// the fold, which must treat the already-settled node as a no-op.
	appendRaw(t, store, tierBStream, engine.EventOwnedSettled, "tier-b-settle:"+act+":dup", map[string]any{
		"handle": act, "kind": engine.OwnedKindTierB, "outcome": engine.OutcomeFailed, "output": "second",
	})
	if err := store.RebuildTierA(ctx, engine.Reducer(), tierBStream); err != nil {
		t.Fatalf("RebuildTierA poisoned by a double settle: %v", err)
	}
	// The FIRST settle stands (not overwritten by the divergent second).
	status, _, _, _ := projectedNode(t, store)
	if status != "done" {
		t.Fatalf("after double settle status=%q, want done (first settle stands)", status)
	}
	if out := nodeMeta(t, store, tierBNodeID, "output"); out != "first" {
		t.Fatalf("output=%q, want first (second settle was an idempotent no-op)", out)
	}
	assertProjectionEqualsRefold(t, store, tierBStream)
}

// TestTierBReSettleIsIdempotentSuccess is the MED-1 proof: a byte-identical
// re-settle dedupes to idempotent success as SettleTierBWork documents. It regresses
// with the false-idempotency bug where a settled node dropped its dispatch_mode
// marker, so readTierBNode saw dispatch_mode="" and the guard rejected the retry
// with ErrTierBNotClaimable before the append dedupe could fire.
func TestTierBReSettleIsIdempotentSuccess(t *testing.T) {
	store := newStore(t)
	act := materializeTierB(t, store)
	ctx := context.Background()
	if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "3 bullets"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	// The documented idempotent re-settle: byte-identical, must return success.
	if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "3 bullets"); err != nil {
		t.Fatalf("byte-identical re-settle = %v, want nil (idempotent success)", err)
	}
	if got := countJournalType(t, store, tierBStream, engine.EventOwnedSettled); got != 1 {
		t.Fatalf("owned.settled rows = %d after re-settle, want 1 (deduped)", got)
	}
	// The settled pool bead retains its dispatch_mode provenance (what makes the
	// re-settle guard reachable).
	if dm := nodeMeta(t, store, tierBNodeID, "dispatch_mode"); dm != engine.DispatchModePool {
		t.Fatalf("settled dispatch_mode = %q, want pool (retained provenance)", dm)
	}
}

// TestTierBClaimCannotCrossStreams is the MED-2 proof: nodes.id is a global PK but
// executor node ids are IR-local, so a claim naming a pool node of ANOTHER stream
// must be rejected at the read guard (stream-scoped), never appended to — and never
// allowed to poison the target stream.
func TestTierBClaimCannotCrossStreams(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	act := materializeTierB(t, store) // stream A: node "summarize", activation "summarize:0"

	// A second, live run whose own node id differs.
	const otherStream = "gcg-run-tierb-other"
	otherAct, err := engine.MaterializeTierBWork(ctx, store, otherStream, engine.TierBWorkSpec{
		RunName: "other", CreatedAt: tierBCreatedAt, NodeID: "othernode", Kind: "do",
	})
	if err != nil {
		t.Fatalf("materialize other stream: %v", err)
	}
	// Claiming stream A's node against otherStream must be rejected: it does not
	// live in otherStream.
	if err := engine.ClaimTierBWork(ctx, store, otherStream, act, "worker-x"); !errors.Is(err, engine.ErrTierBNotClaimable) {
		t.Fatalf("cross-stream claim err = %v, want ErrTierBNotClaimable", err)
	}
	// otherStream is untouched: no owned.admitted appended, its own bead still ready,
	// and its projection is a clean fold of its own journal.
	if got := countJournalType(t, store, otherStream, engine.EventOwnedAdmitted); got != 0 {
		t.Fatalf("owned.admitted appended to target stream = %d, want 0 (rejected before append)", got)
	}
	if !inFrontier(t, store, otherStream, otherAct) {
		t.Fatal("otherStream's own bead left the frontier after a rejected cross-stream claim")
	}
	assertProjectionEqualsRefold(t, store, otherStream)
	// Stream A is untouched: still open and unclaimed.
	status, assignee, _, _ := projectedNode(t, store)
	if status != "open" || assignee != "" {
		t.Fatalf("stream A {status:%q assignee:%q}, want {open, \"\"} (untouched)", status, assignee)
	}
}

// TestTierBSettleDrivesDownstreamDAG is the MED-3(a) proof that a Tier-B
// owned.settled drives the run's DAG exactly like an engine outcome.settled: a
// passing settle makes a gated dependent ready; a failed one skip-cascades it (the
// dependent never becomes ready), consistent with P4.2.
func TestTierBSettleDrivesDownstreamDAG(t *testing.T) {
	t.Run("pass_makes_dependent_ready", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		act := materializeTierB(t, store)
		actB := appendPoolNode(t, store, tierBStream, "publish", []string{act})
		if inFrontier(t, store, tierBStream, actB) {
			t.Fatal("gated dependent is ready before its dependency settled")
		}
		if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomePass, "ok"); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if !inFrontier(t, store, tierBStream, actB) {
			t.Fatal("dependent not ready after its dependency settled pass (settle did not drive the DAG)")
		}
	})
	t.Run("failed_skip_cascades_dependent", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		act := materializeTierB(t, store)
		actB := appendPoolNode(t, store, tierBStream, "publish", []string{act})
		if err := engine.SettleTierBWork(ctx, store, tierBStream, act, engine.OutcomeFailed, "boom"); err != nil {
			t.Fatalf("settle: %v", err)
		}
		if inFrontier(t, store, tierBStream, actB) {
			t.Fatal("dependent is ready after its dependency settled failed (skip-cascade not applied)")
		}
	})
}

// TestTierBAsyncOwnedEventsDoNotFoldAsTierB is the MED-3(b) kind-symmetry proof: a
// non-Tier-B owned.admitted/owned.settled (kind=async) whose handle collides with a
// pool activation must NOT fold as a Tier-B claim/settle. Both arms discriminate on
// kind, so a future async/detached handle can never settle or claim a pool node.
func TestTierBAsyncOwnedEventsDoNotFoldAsTierB(t *testing.T) {
	t.Run("async_settle_does_not_settle_pool_node", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		act := materializeTierB(t, store)
		appendRaw(t, store, tierBStream, engine.EventOwnedSettled, "async-settle:"+act, map[string]any{
			"handle": act, "kind": "async", "outcome": engine.OutcomePass, "output": "x",
		})
		if err := store.RebuildTierA(ctx, engine.Reducer(), tierBStream); err != nil {
			t.Fatalf("RebuildTierA: %v", err)
		}
		status, assignee, dm, _ := projectedNode(t, store)
		if status != "open" || assignee != "" || dm != engine.DispatchModePool {
			t.Fatalf("after async settle {status:%q assignee:%q dm:%q}, want {open, \"\", pool}", status, assignee, dm)
		}
	})
	t.Run("async_admit_does_not_claim_pool_node", func(t *testing.T) {
		store := newStore(t)
		ctx := context.Background()
		act := materializeTierB(t, store)
		appendRaw(t, store, tierBStream, engine.EventOwnedAdmitted, "async-admit:"+act, map[string]any{
			"handle": act, "activation": act, "kind": "async", "assignee": "async-owner",
		})
		if err := store.RebuildTierA(ctx, engine.Reducer(), tierBStream); err != nil {
			t.Fatalf("RebuildTierA: %v", err)
		}
		status, assignee, _, _ := projectedNode(t, store)
		if status != "open" || assignee != "" {
			t.Fatalf("after async admit {status:%q assignee:%q}, want {open, \"\"}", status, assignee)
		}
	})
}

// TestTierBEmptyAssigneeClaimIsNoOp is the LOW-2 proof: a Tier-B owned.admitted
// with an empty assignee holds the bead for nobody, so the fold folds it to a
// defined no-op — the node stays claimable, never orphaned — and a real worker can
// still claim it.
func TestTierBEmptyAssigneeClaimIsNoOp(t *testing.T) {
	store := newStore(t)
	ctx := context.Background()
	act := materializeTierB(t, store)
	// Distinct idem token: this is a crafted anomaly (the API rejects an empty
	// assignee before append), and it must not block a later legitimate claim.
	appendRaw(t, store, tierBStream, engine.EventOwnedAdmitted, "tier-b-claim:"+act+":empty", map[string]any{
		"handle": act, "activation": act, "kind": engine.OwnedKindTierB, "assignee": "",
	})
	if err := store.RebuildTierA(ctx, engine.Reducer(), tierBStream); err != nil {
		t.Fatalf("RebuildTierA: %v", err)
	}
	if status, assignee, _, _ := projectedNode(t, store); status != "open" || assignee != "" {
		t.Fatalf("after empty-assignee claim {status:%q assignee:%q}, want {open, \"\"} (still claimable)", status, assignee)
	}
	// Not orphaned: the bead stays IN the frontier (an ineffective claim must not
	// pull an unowned, open bead off the serve surface).
	if !inFrontier(t, store, tierBStream, act) {
		t.Fatal("empty-assignee claim orphaned the bead (removed from frontier while still open)")
	}
	// Not orphaned: a real worker still claims it.
	if err := engine.ClaimTierBWork(ctx, store, tierBStream, act, "worker-a"); err != nil {
		t.Fatalf("claim after empty-assignee no-op: %v", err)
	}
	if _, assignee, _, _ := projectedNode(t, store); assignee != "worker-a" {
		t.Fatalf("assignee = %q, want worker-a", assignee)
	}
}

// --- helpers ---------------------------------------------------------------

// appendRaw commits one crafted lumen event at the current stream head WITHOUT
// re-projecting, so a test controls exactly when (and over what event sequence)
// RebuildTierA folds. It is how the totality tests construct the legal-but-racing
// sequences the higher-level API guards would otherwise prevent.
func appendRaw(t *testing.T, store *graphstore.Store, streamID, typ, idemToken string, payload map[string]any) {
	t.Helper()
	ctx := context.Background()
	engine.RegisterVocabulary(store)
	head, err := store.Head(ctx, streamID)
	if err != nil {
		t.Fatalf("head: %v", err)
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", typ, err)
	}
	if _, err := store.Append(ctx, streamID, engine.Engine, head, 0, []graphstore.JournalEvent{{
		Type:              typ,
		IRContractVersion: "0.2.5",
		IdemToken:         idemToken,
		Payload:           mustCanon(t, body),
	}}); err != nil {
		t.Fatalf("append %s: %v", typ, err)
	}
}

// appendPoolNode activates a downstream pool-mode node gated on `after`, re-projects,
// and returns its activation key — the DAG scaffolding MaterializeTierBWork's single
// node cannot express.
func appendPoolNode(t *testing.T, store *graphstore.Store, streamID, nodeID string, after []string) string {
	t.Helper()
	activation := nodeID + ":0"
	payload := map[string]any{
		"node_id":       nodeID,
		"activation":    activation,
		"kind":          "do",
		"dispatch_mode": engine.DispatchModePool,
	}
	if len(after) > 0 {
		payload["after"] = after
	}
	appendRaw(t, store, streamID, engine.EventNodeActivated, streamID+":activated:"+activation, payload)
	if err := store.RebuildTierA(context.Background(), engine.Reducer(), streamID); err != nil {
		t.Fatalf("rebuild after activating %s: %v", nodeID, err)
	}
	return activation
}

// nodeMeta reads one projected node_metadata value (empty if absent).
func nodeMeta(t *testing.T, store *graphstore.Store, nodeID, key string) string {
	t.Helper()
	var v string
	_ = store.DB().QueryRowContext(context.Background(),
		`SELECT value FROM node_metadata WHERE node_id = ? AND key = ?`, nodeID, key).Scan(&v)
	return v
}

// inFrontier reports whether an activation sits in the stream's Tier-A frontier.
func inFrontier(t *testing.T, store *graphstore.Store, streamID, activation string) bool {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM frontier WHERE root_id = ? AND node_id = ?`, streamID, activation).Scan(&n); err != nil {
		t.Fatalf("query frontier: %v", err)
	}
	return n > 0
}

func countJournalType(t *testing.T, store *graphstore.Store, streamID, typ string) int {
	t.Helper()
	var n int
	if err := store.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM journal WHERE stream_id = ? AND type = ?`, streamID, typ).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", typ, err)
	}
	return n
}

func mustCanon(t *testing.T, raw []byte) []byte {
	t.Helper()
	out, err := canon.Canonicalize(raw)
	if err != nil {
		t.Fatalf("canonicalize: %v", err)
	}
	return out
}

// readFoldEvents reads the stream through the beads.Store journal capability and
// projects each row onto the fold.Event view.
func readFoldEvents(t *testing.T, store *graphstore.Store, streamID string) []fold.Event {
	t.Helper()
	stored, err := store.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
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

// collapseNodeUpserts folds a delta slice to the final row per node id
// (last-write-wins), the net node projection the deltas produce.
func collapseNodeUpserts(deltas []fold.Delta) map[string]fold.NodeRow {
	out := map[string]fold.NodeRow{}
	for _, d := range deltas {
		for _, n := range d.NodeUpserts {
			out[n.ID] = n
		}
	}
	return out
}

// assertProjectionEqualsRefold proves the live projection equals a fresh
// drop+refold of the journal — the pure-fold property.
func assertProjectionEqualsRefold(t *testing.T, store *graphstore.Store, streamID string) {
	t.Helper()
	before := projectionSnapshot(t, store, streamID)
	if err := store.RebuildTierA(context.Background(), engine.Reducer(), streamID); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	after := projectionSnapshot(t, store, streamID)
	if before != after {
		t.Fatalf("live projection != drop+refold:\n--- live ---\n%s\n--- refold ---\n%s", before, after)
	}
}

// projectionSnapshot renders a canonical, stable dump of a stream's Tier-A rows
// (nodes, node_metadata, frontier) for byte-equality assertions.
func projectionSnapshot(t *testing.T, store *graphstore.Store, streamID string) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	nodeRows, err := store.DB().QueryContext(ctx,
		`SELECT id, status, COALESCE(assignee,''), bead_type, COALESCE(parent_id,'')
		   FROM nodes WHERE stream_id = ? AND fold_owned = 1 ORDER BY id`, streamID)
	if err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	defer func() { _ = nodeRows.Close() }()
	for nodeRows.Next() {
		var id, status, assignee, bt, parent string
		if err := nodeRows.Scan(&id, &status, &assignee, &bt, &parent); err != nil {
			t.Fatalf("scan node: %v", err)
		}
		fmt.Fprintf(&b, "node %s status=%s assignee=%s type=%s parent=%s\n", id, status, assignee, bt, parent)
	}
	metaRows, err := store.DB().QueryContext(ctx,
		`SELECT m.node_id, m.key, m.value FROM node_metadata m
		   JOIN nodes n ON n.id = m.node_id
		  WHERE n.stream_id = ? AND n.fold_owned = 1 ORDER BY m.node_id, m.key`, streamID)
	if err != nil {
		t.Fatalf("query metadata: %v", err)
	}
	defer func() { _ = metaRows.Close() }()
	var metaLines []string
	for metaRows.Next() {
		var id, k, v string
		if err := metaRows.Scan(&id, &k, &v); err != nil {
			t.Fatalf("scan meta: %v", err)
		}
		metaLines = append(metaLines, fmt.Sprintf("meta %s %s=%s", id, k, v))
	}
	sort.Strings(metaLines)
	b.WriteString(strings.Join(metaLines, "\n"))
	b.WriteString("\n")
	frontierRows, err := store.DB().QueryContext(ctx,
		`SELECT node_id FROM frontier WHERE root_id = ? ORDER BY node_id`, streamID)
	if err != nil {
		t.Fatalf("query frontier: %v", err)
	}
	defer func() { _ = frontierRows.Close() }()
	for frontierRows.Next() {
		var id string
		if err := frontierRows.Scan(&id); err != nil {
			t.Fatalf("scan frontier: %v", err)
		}
		fmt.Fprintf(&b, "frontier %s\n", id)
	}
	return b.String()
}
