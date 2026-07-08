package dispatch

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// --- test doubles -----------------------------------------------------------

// spySettlementEmitter records every Emit* call for anchor-wiring assertions.
type spySettlementEmitter struct {
	mu       sync.Mutex
	root     []Settlement
	attempt  []Settlement
	workflow []Settlement
	engines  []string
}

func (s *spySettlementEmitter) record(bucket *[]Settlement, engine string, v Settlement) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	*bucket = append(*bucket, v)
	s.engines = append(s.engines, engine)
	return nil
}

func (s *spySettlementEmitter) EmitRootSettled(_ context.Context, engine string, v Settlement) error {
	return s.record(&s.root, engine, v)
}

func (s *spySettlementEmitter) EmitAttemptSettled(_ context.Context, engine string, v Settlement) error {
	return s.record(&s.attempt, engine, v)
}

func (s *spySettlementEmitter) EmitWorkflowFinalized(_ context.Context, engine string, v Settlement) error {
	return s.record(&s.workflow, engine, v)
}

// failingSettlementEmitter fails every emit to prove a journal error never
// alters bead state or fails the control action.
type failingSettlementEmitter struct{ err error }

func (f failingSettlementEmitter) EmitRootSettled(context.Context, string, Settlement) error {
	return f.err
}

func (f failingSettlementEmitter) EmitAttemptSettled(context.Context, string, Settlement) error {
	return f.err
}

func (f failingSettlementEmitter) EmitWorkflowFinalized(context.Context, string, Settlement) error {
	return f.err
}

func openDispatchJournal(t *testing.T) (beads.Store, *graphstore.Store) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "settle-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	return beads.NewJournalStore(gs), gs
}

func tierACounts(t *testing.T, gs *graphstore.Store) (nodes, edges, frontier int) {
	t.Helper()
	q := func(table string) int {
		var n int
		if err := gs.ReadDB().QueryRowContext(context.Background(), "SELECT COUNT(*) FROM "+table).Scan(&n); err != nil {
			t.Fatalf("count %s: %v", table, err)
		}
		return n
	}
	return q("nodes"), q("edges"), q("frontier")
}

func readSettlementStream(t *testing.T, gs *graphstore.Store, rootID string) []graphstore.StoredEvent {
	t.Helper()
	evs, err := gs.ReadStream(context.Background(), beads.SettlementStreamID(rootID), 1, 0)
	if err != nil {
		t.Fatalf("read settlement stream: %v", err)
	}
	return evs
}

// --- emitter unit tests (driven directly) -----------------------------------

// TestNewJournalSettlementEmitterNilInert proves the nil-default: a nil journal
// (a non-opted city) yields a nil SettlementEmitter, so the dispatcher's emit
// helpers early-return and nothing happens — byte-identical to pre-P5.3.
func TestNewJournalSettlementEmitterNilInert(t *testing.T) {
	t.Parallel()
	if e := NewJournalSettlementEmitter(nil); e != nil {
		t.Fatalf("NewJournalSettlementEmitter(nil) = %#v, want nil interface", e)
	}
}

// TestSettlementEmitterAppendsCoarseFact proves an emit lands one engine-tagged
// event on the per-root stream with the typed payload, and the hash chain stays
// intact (graphstore.Verify green).
func TestSettlementEmitterAppendsCoarseFact(t *testing.T) {
	t.Parallel()
	store, gs := openDispatchJournal(t)
	emitter := NewJournalSettlementEmitter(store)
	if emitter == nil {
		t.Fatal("emitter is nil for a journal-backed store")
	}
	if err := emitter.EmitRootSettled(context.Background(), beads.SettlementEngineV2,
		Settlement{Root: "gcg-root1", Bead: "gcg-root1", Outcome: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("EmitRootSettled: %v", err)
	}

	evs := readSettlementStream(t, gs, "gcg-root1")
	if len(evs) != 1 {
		t.Fatalf("stream has %d events, want 1", len(evs))
	}
	got := evs[0]
	if got.Engine != beads.SettlementEngineV2 {
		t.Fatalf("engine = %q, want v2", got.Engine)
	}
	if got.Type != beads.SettlementRootType {
		t.Fatalf("type = %q, want %q", got.Type, beads.SettlementRootType)
	}
	var p beads.SettlementPayload
	if err := json.Unmarshal(got.Payload, &p); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if p.Root != "gcg-root1" || p.Bead != "gcg-root1" || p.Outcome != beadmeta.OutcomePass {
		t.Fatalf("payload = %+v, want root/bead=gcg-root1 outcome=pass", p)
	}
	if err := gs.Verify(context.Background(), beads.SettlementStreamID("gcg-root1")); err != nil {
		t.Fatalf("Verify: %v (hash chain must stay intact)", err)
	}
}

// TestSettlementEmitterRedoDedupes is the R-IDEM proof: emitting the SAME
// settlement twice (the idempotent-redo control path re-running) appends exactly
// one event — the second is a byte-identical duplicate under the same outcome-
// scoped idem token and dedupes to a no-op success, never ErrIdemTokenReuse.
func TestSettlementEmitterRedoDedupes(t *testing.T) {
	t.Parallel()
	store, gs := openDispatchJournal(t)
	emitter := NewJournalSettlementEmitter(store)

	s := Settlement{Root: "gcg-r", Bead: "gcg-log", Outcome: beadmeta.OutcomeFail, Attempt: 2}
	for i := 0; i < 3; i++ {
		if err := emitter.EmitAttemptSettled(context.Background(), beads.SettlementEngineV2, s); err != nil {
			t.Fatalf("redo %d: %v", i, err)
		}
	}
	if evs := readSettlementStream(t, gs, "gcg-r"); len(evs) != 1 {
		t.Fatalf("stream has %d events after 3 identical emits, want 1 (R-IDEM dedupe)", len(evs))
	}

	// A genuinely different re-settlement (different outcome → different token) is
	// a second provenance fact, not a dedupe.
	if err := emitter.EmitAttemptSettled(context.Background(), beads.SettlementEngineV2,
		Settlement{Root: "gcg-r", Bead: "gcg-log", Outcome: beadmeta.OutcomePass, Attempt: 2}); err != nil {
		t.Fatalf("distinct-outcome emit: %v", err)
	}
	if evs := readSettlementStream(t, gs, "gcg-r"); len(evs) != 2 {
		t.Fatalf("stream has %d events, want 2 (distinct outcome is a new fact)", len(evs))
	}
}

// TestSettlementEmitterRetriesContendedHead proves the bounded optimistic-CAS
// retry: a competing append steals the head between the emitter's StreamHead read
// and its append, forcing an ErrWrongExpectedVersion that the emitter absorbs by
// re-reading and appending behind the winner — never surfacing an error.
func TestSettlementEmitterRetriesContendedHead(t *testing.T) {
	store, gs := openDispatchJournal(t)
	emitter := NewJournalSettlementEmitter(store)
	streamID := beads.SettlementStreamID("gcg-contended")

	var once sync.Once
	settlementAfterHead = func() {
		once.Do(func() {
			// A cross-process writer commits first, stealing the head the emitter
			// just read so its CAS misses and must retry.
			ev, err := beads.SettlementEvent(beads.SettlementRootType,
				beads.SettlementPayload{Root: "gcg-contended", Bead: "sibling", Outcome: beadmeta.OutcomeFail})
			if err != nil {
				t.Errorf("build competing event: %v", err)
				return
			}
			head, err := gs.Head(context.Background(), streamID)
			if err != nil {
				t.Errorf("competing head: %v", err)
				return
			}
			if _, err := gs.Append(context.Background(), streamID, beads.SettlementEngineV2, head, 0,
				[]graphstore.JournalEvent{ev}); err != nil {
				t.Errorf("competing append: %v", err)
			}
		})
	}
	defer func() { settlementAfterHead = nil }()

	if err := emitter.EmitWorkflowFinalized(context.Background(), beads.SettlementEngineV2,
		Settlement{Root: "gcg-contended", Bead: "gcg-fin", Outcome: beadmeta.OutcomePass}); err != nil {
		t.Fatalf("EmitWorkflowFinalized under contention: %v", err)
	}
	// Both the competitor and the emitter's event landed (2 events), chain intact.
	if evs := readSettlementStream(t, gs, "gcg-contended"); len(evs) != 2 {
		t.Fatalf("stream has %d events, want 2 (competitor + retried emit)", len(evs))
	}
	if err := gs.Verify(context.Background(), streamID); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// TestSettlementEmitZeroTierADelta is the provenance-only tripwire: emitting
// coarse settlements produces ZERO Tier-A (nodes/edges/frontier) rows — nothing
// folds these events, so the v1/v2 projection of record stays the mutation-
// primary columns and no dual-primary is created.
func TestSettlementEmitZeroTierADelta(t *testing.T) {
	t.Parallel()
	store, gs := openDispatchJournal(t)
	emitter := NewJournalSettlementEmitter(store)

	n0, e0, f0 := tierACounts(t, gs)

	ctx := context.Background()
	_ = emitter.EmitRootSettled(ctx, beads.SettlementEngineV2, Settlement{Root: "gcg-x", Bead: "gcg-x", Outcome: beadmeta.OutcomePass})
	_ = emitter.EmitAttemptSettled(ctx, beads.SettlementEngineV2, Settlement{Root: "gcg-x", Bead: "gcg-l", Outcome: beadmeta.OutcomeFail, Attempt: 1})
	_ = emitter.EmitWorkflowFinalized(ctx, beads.SettlementEngineV2, Settlement{Root: "gcg-x", Bead: "gcg-f", Outcome: beadmeta.OutcomePass})

	n1, e1, f1 := tierACounts(t, gs)
	if n1 != n0 || e1 != e0 || f1 != f0 {
		t.Fatalf("Tier-A changed after settlement emits: nodes %d->%d edges %d->%d frontier %d->%d (want unchanged)", n0, n1, e0, e1, f0, f1)
	}
	// But the settlement stream DID grow — provenance landed.
	if evs := readSettlementStream(t, gs, "gcg-x"); len(evs) != 3 {
		t.Fatalf("settlement stream has %d events, want 3", len(evs))
	}
}

// --- anchor-wiring tests (driven through ProcessControl) --------------------

func finalizeFixture(t *testing.T, store beads.Store) (root, finalizer beads.Bead) {
	t.Helper()
	root = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
		},
	})
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "cleanup",
		Type:     "task",
		Status:   "closed",
		Metadata: map[string]string{"gc.outcome": "fail"},
	})
	finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": root.ID,
		},
	})
	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	mustDepAdd(t, store, root.ID, finalizer.ID, "blocks")
	return root, finalizer
}

// TestFinalizeEmitsRootAndWorkflow proves the finalize anchors fire coarse
// settlement.root + settlement.workflow.finalized with the resolved workflow
// outcome, engine v2.
func TestFinalizeEmitsRootAndWorkflow(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, finalizer := finalizeFixture(t, store)
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, finalizer, ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(finalize): %v", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("result = %+v, want processed workflow-fail", result)
	}

	if len(spy.root) != 1 || spy.root[0].Root != root.ID || spy.root[0].Bead != root.ID || spy.root[0].Outcome != "fail" {
		t.Fatalf("root settlements = %+v, want one {root=%s bead=%s outcome=fail}", spy.root, root.ID, root.ID)
	}
	if len(spy.workflow) != 1 || spy.workflow[0].Root != root.ID || spy.workflow[0].Bead != finalizer.ID || spy.workflow[0].Outcome != "fail" {
		t.Fatalf("workflow settlements = %+v, want one {root=%s bead=%s outcome=fail}", spy.workflow, root.ID, finalizer.ID)
	}
	for _, e := range spy.engines {
		if e != beads.SettlementEngineV2 {
			t.Fatalf("engine = %q, want v2", e)
		}
	}
}

// TestFinalizeEmitFailureNeverAltersBead proves the load-bearing guarantee: a
// journal Append failure during emission is swallowed — the finalize returns its
// normal result and the root stays correctly closed. The projection of record
// (the gc.outcome column write) already committed and is untouched.
func TestFinalizeEmitFailureNeverAltersBead(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, finalizer := finalizeFixture(t, store)

	result, err := ProcessControl(store, finalizer, ProcessOptions{
		Settlements: failingSettlementEmitter{err: errors.New("journal append boom")},
	})
	if err != nil {
		t.Fatalf("ProcessControl(finalize) with failing emitter returned err = %v, want nil (provenance must not break finalize)", err)
	}
	if !result.Processed || result.Action != "workflow-fail" {
		t.Fatalf("result = %+v, want processed workflow-fail", result)
	}
	rootAfter, err := store.Get(root.ID)
	if err != nil {
		t.Fatalf("get root: %v", err)
	}
	if rootAfter.Status != "closed" || rootAfter.Metadata["gc.outcome"] != "fail" {
		t.Fatalf("root = status %q outcome %q, want closed/fail (bead state unaffected by emit failure)", rootAfter.Status, rootAfter.Metadata["gc.outcome"])
	}
	finAfter, err := store.Get(finalizer.ID)
	if err != nil {
		t.Fatalf("get finalizer: %v", err)
	}
	if finAfter.Status != "closed" {
		t.Fatalf("finalizer status = %q, want closed", finAfter.Status)
	}
}

// TestFinalizeNilEmitterByteIdentity proves the nil emitter perturbs nothing: the
// control result and root bead state are IDENTICAL with a nil emitter and with a
// (recording) spy emitter — emission is strictly after-the-fact provenance.
func TestFinalizeNilEmitterByteIdentity(t *testing.T) {
	t.Parallel()
	nilStore := beads.NewMemStore()
	nilRoot, nilFinal := finalizeFixture(t, nilStore)
	nilResult, err := ProcessControl(nilStore, nilFinal, ProcessOptions{Settlements: nil})
	if err != nil {
		t.Fatalf("ProcessControl(nil emitter): %v", err)
	}

	spyStore := beads.NewMemStore()
	spyRoot, spyFinal := finalizeFixture(t, spyStore)
	spyResult, err := ProcessControl(spyStore, spyFinal, ProcessOptions{Settlements: &spySettlementEmitter{}})
	if err != nil {
		t.Fatalf("ProcessControl(spy emitter): %v", err)
	}

	if nilResult != spyResult {
		t.Fatalf("result differs: nil=%+v spy=%+v", nilResult, spyResult)
	}
	nilAfter := mustGetBead(t, nilStore, nilRoot.ID)
	spyAfter := mustGetBead(t, spyStore, spyRoot.ID)
	if nilAfter.Status != spyAfter.Status || nilAfter.Metadata["gc.outcome"] != spyAfter.Metadata["gc.outcome"] {
		t.Fatalf("root state differs: nil=%s/%s spy=%s/%s",
			nilAfter.Status, nilAfter.Metadata["gc.outcome"], spyAfter.Status, spyAfter.Metadata["gc.outcome"])
	}
}

// TestRetryEvalEmitsAttempt proves the retry logical settle fires
// settlement.attempt with the logical id, resolved outcome, and attempt number.
func TestRetryEvalEmitsAttempt(t *testing.T) {
	t.Parallel()
	store := newStrictCloseStore()
	root := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:    "workflow",
		Type:     "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	logical := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": "3",
			"gc.on_exhausted": "hard_fail",
		},
	})
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title:  "review attempt 1",
		Type:   "task",
		Status: "closed",
		Metadata: map[string]string{
			"gc.kind":            "retry-run",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
			"gc.outcome":         "pass",
		},
	})
	eval1 := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1",
		Type:  "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    "3",
			"gc.on_exhausted":    "hard_fail",
		},
	})
	mustDepAdd(t, store, logical.ID, eval1.ID, "blocks")
	mustDepAdd(t, store, eval1.ID, run1.ID, "blocks")

	spy := &spySettlementEmitter{}
	result, err := ProcessControl(store, eval1, ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval): %v", err)
	}
	if !result.Processed || result.Action != "pass" {
		t.Fatalf("result = %+v, want processed pass", result)
	}
	if len(spy.attempt) != 1 {
		t.Fatalf("attempt settlements = %+v, want exactly one (coarse)", spy.attempt)
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != logical.ID || got.Outcome != "pass" || got.Attempt != 1 {
		t.Fatalf("attempt settlement = %+v, want {root=%s bead=%s outcome=pass attempt=1}", got, root.ID, logical.ID)
	}
	// Coarse: no root/workflow settlement from an attempt path.
	if len(spy.root) != 0 || len(spy.workflow) != 0 {
		t.Fatalf("unexpected root=%d workflow=%d settlements from a retry-eval", len(spy.root), len(spy.workflow))
	}
}
