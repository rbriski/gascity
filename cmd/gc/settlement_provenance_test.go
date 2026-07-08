package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/dispatch"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/settlementfold"
)

// settlementFactsFor reads a root's provenance and flattens every stream group's
// facts. The cmd-side v1 anchor tests each populate exactly one stream, so a flat
// view is the natural assertion surface.
func settlementFactsFor(t *testing.T, journal beads.Store, rootID string) []settlementfold.SettlementFact {
	t.Helper()
	streams, err := beads.ProvenanceTimeline(context.Background(), journal, rootID)
	if err != nil {
		t.Fatalf("ProvenanceTimeline: %v", err)
	}
	var facts []settlementfold.SettlementFact
	for _, s := range streams {
		facts = append(facts, s.Facts...)
	}
	return facts
}

// openTestJournal opens a fresh journal-backed store for cmd-side provenance
// tests, plus a settlement emitter over it.
func openTestJournal(t *testing.T) (*beads.JournalStore, dispatch.SettlementEmitter) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "cmd-prov-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	journal := beads.NewJournalStore(gs)
	return journal, dispatch.NewJournalSettlementEmitter(journal)
}

// cmdFailingEmitter fails every emit, to prove an emit failure never unwinds a
// cmd-side close.
type cmdFailingEmitter struct{ err error }

func (f cmdFailingEmitter) EmitRootSettled(context.Context, string, dispatch.Settlement) error {
	return f.err
}

func (f cmdFailingEmitter) EmitAttemptSettled(context.Context, string, dispatch.Settlement) error {
	return f.err
}

func (f cmdFailingEmitter) EmitWorkflowFinalized(context.Context, string, dispatch.Settlement) error {
	return f.err
}

// TestMoleculeAutocloseEmitsV1Settlement proves the P5.4 v1 anchor: the reactive
// molecule autoclose emits exactly one coarse settlement.root (engine v1 for a
// contract-less molecule) after it closes the root.
func TestMoleculeAutocloseEmitsV1Settlement(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	stepA, _ := store.Create(beads.Bead{Title: "a", Type: "step", ParentID: root.ID})
	stepB, _ := store.Create(beads.Bead{Title: "b", Type: "step", ParentID: root.ID})
	_ = store.Close(stepA.ID)
	_ = store.Close(stepB.ID)

	journal, emitter := openTestJournal(t)
	var out bytes.Buffer
	doMoleculeAutocloseWithEmitter(store, "", events.Discard, stepB.ID, &out, emitter)

	after, _ := store.Get(root.ID)
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed", after.Status)
	}

	facts := settlementFactsFor(t, journal, root.ID)
	if len(facts) != 1 {
		t.Fatalf("timeline has %d facts, want exactly 1 (coarse, one per root close): %+v", len(facts), facts)
	}
	f := facts[0]
	if f.Engine != beads.SettlementEngineV1 || f.Type != beads.SettlementRootType || f.Root != root.ID || f.Bead != root.ID {
		t.Fatalf("fact = %+v, want {engine=v1 type=settlement.root root=bead=%s}", f, root.ID)
	}
}

// TestMoleculeAutocloseEmitsV2ForGraphContract proves the engine is data-derived:
// a molecule root carrying gc.formula_contract=graph.v2 emits under engine v2.
func TestMoleculeAutocloseEmitsV2ForGraphContract(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{
		Title: "mol", Type: "molecule",
		Metadata: map[string]string{beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2},
	})
	step, _ := store.Create(beads.Bead{Title: "a", Type: "step", ParentID: root.ID})
	_ = store.Close(step.ID)

	journal, emitter := openTestJournal(t)
	var out bytes.Buffer
	doMoleculeAutocloseWithEmitter(store, "", events.Discard, step.ID, &out, emitter)

	facts := settlementFactsFor(t, journal, root.ID)
	if len(facts) != 1 || facts[0].Engine != beads.SettlementEngineV2 {
		t.Fatalf("facts = %+v, want one engine=v2 fact", facts)
	}
}

// TestMoleculeAutocloseNilEmitterByteIdentity proves the anchor is strictly
// after-the-fact: the molecule closes byte-identically with a nil emitter and
// with a journal emitter.
func TestMoleculeAutocloseNilEmitterByteIdentity(t *testing.T) {
	t.Parallel()
	build := func() (beads.Store, string, string) {
		store := beads.NewMemStore()
		root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
		step, _ := store.Create(beads.Bead{Title: "a", Type: "step", ParentID: root.ID})
		_ = store.Close(step.ID)
		return store, root.ID, step.ID
	}

	nilStore, nilRoot, nilStep := build()
	var nilOut bytes.Buffer
	doMoleculeAutocloseWithEmitter(nilStore, "", events.Discard, nilStep, &nilOut, nil)

	emitStore, emitRoot, emitStep := build()
	_, emitter := openTestJournal(t)
	var emitOut bytes.Buffer
	doMoleculeAutocloseWithEmitter(emitStore, "", events.Discard, emitStep, &emitOut, emitter)

	nilAfter, _ := nilStore.Get(nilRoot)
	emitAfter, _ := emitStore.Get(emitRoot)
	if nilAfter.Status != emitAfter.Status || nilAfter.Metadata["close_reason"] != emitAfter.Metadata["close_reason"] {
		t.Fatalf("root state differs: nil=%s/%s emit=%s/%s",
			nilAfter.Status, nilAfter.Metadata["close_reason"], emitAfter.Status, emitAfter.Metadata["close_reason"])
	}
	if nilOut.String() != emitOut.String() {
		t.Fatalf("stdout differs: nil=%q emit=%q", nilOut.String(), emitOut.String())
	}
}

// TestMoleculeAutocloseEmitFailureNeverAltersClose proves a journal emit failure
// is swallowed: the molecule still closes.
func TestMoleculeAutocloseEmitFailureNeverAltersClose(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	step, _ := store.Create(beads.Bead{Title: "a", Type: "step", ParentID: root.ID})
	_ = store.Close(step.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWithEmitter(store, "", events.Discard, step.ID, &out, cmdFailingEmitter{err: errors.New("journal boom")})

	after, _ := store.Get(root.ID)
	if after.Status != "closed" {
		t.Fatalf("root status = %q, want closed (emit failure must not unwind the close)", after.Status)
	}
}

// TestWispGCAbandonedRootEmitsCoarseSettlement proves the wisp-GC anchor is
// COARSE: an abandoned root with MULTIPLE terminal descendants emits exactly ONE
// settlement.root — one per genuine root close, never one per wisp swept.
func TestWispGCAbandonedRootEmitsCoarseSettlement(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID: "mol-root", Status: "open", Type: "molecule",
			CreatedAt: now.Add(-30 * time.Minute), UpdatedAt: now.Add(-30 * time.Minute),
		},
		{ID: "mol-root.1", Status: "closed", Type: "task", CreatedAt: now.Add(-30 * time.Minute), ParentID: "mol-root"},
		{ID: "mol-root.2", Status: "closed", Type: "task", CreatedAt: now.Add(-30 * time.Minute), ParentID: "mol-root"},
		{ID: "mol-root.3", Status: "tombstone", Type: "task", CreatedAt: now.Add(-30 * time.Minute), ParentID: "mol-root"},
	})
	for _, id := range []string{"mol-root.1", "mol-root.2", "mol-root.3"} {
		if err := store.DepAdd(id, "mol-root", "parent-child"); err != nil {
			t.Fatalf("DepAdd(%s): %v", id, err)
		}
	}

	journal, emitter := openTestJournal(t)
	withCloseAbandonedEnforced(t, func() {
		withCloseAbandonedTTL(t, 5*time.Minute, func() {
			if err := closeAbandonedRoots(store, now, emitter); err != nil {
				t.Fatalf("closeAbandonedRoots: %v", err)
			}
		})
	})

	root, err := store.Get("mol-root")
	if err != nil {
		t.Fatalf("Get(mol-root): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("mol-root status = %q, want closed", root.Status)
	}

	facts := settlementFactsFor(t, journal, "mol-root")
	if len(facts) != 1 {
		t.Fatalf("timeline has %d facts, want exactly 1 (one per ROOT, not per wisp): %+v", len(facts), facts)
	}
	if facts[0].Type != beads.SettlementRootType || facts[0].Root != "mol-root" {
		t.Fatalf("fact = %+v, want settlement.root for mol-root", facts[0])
	}
}

// TestGraphJournalCmdRenders proves the hidden `gc graph journal` read surface:
// it folds a root's settlement stream and renders a per-stream, seq-ordered,
// engine-tagged plain table (the command is plain-text only — no --json).
func TestGraphJournalCmdRenders(t *testing.T) {
	t.Parallel()
	journal, emitter := openTestJournal(t)
	ctx := context.Background()
	root := "gcg-root"
	if err := emitter.EmitAttemptSettled(ctx, beads.SettlementEngineV2,
		dispatch.Settlement{Root: root, Bead: "gcg-log", Outcome: "fail", Attempt: 1}); err != nil {
		t.Fatalf("emit attempt: %v", err)
	}
	if err := emitter.EmitRootSettled(ctx, beads.SettlementEngineV2,
		dispatch.Settlement{Root: root, Bead: root, Outcome: "fail"}); err != nil {
		t.Fatalf("emit root: %v", err)
	}
	if err := emitter.EmitWorkflowFinalized(ctx, beads.SettlementEngineV2,
		dispatch.Settlement{Root: root, Bead: "gcg-fin", Outcome: "fail"}); err != nil {
		t.Fatalf("emit workflow: %v", err)
	}

	var out, errb bytes.Buffer
	if code := writeProvenanceTimeline(journal, root, &out, &errb); code != 0 {
		t.Fatalf("writeProvenanceTimeline code=%d stderr=%q", code, errb.String())
	}
	got := out.String()
	wantSubstrings := []string{
		"stream " + beads.SettlementStreamID(root), // per-stream header, no global SEQ
		"SEQ", "ENGINE", "TYPE",
		"settlement.attempt", "settlement.root", "settlement.workflow.finalized", "v2", "fail",
	}
	for _, want := range wantSubstrings {
		if !strings.Contains(got, want) {
			t.Fatalf("plain output missing %q:\n%s", want, got)
		}
	}

	// An unknown root renders the empty notice, not an error.
	var emptyOut, emptyErr bytes.Buffer
	if code := writeProvenanceTimeline(journal, "gcg-unknown", &emptyOut, &emptyErr); code != 0 {
		t.Fatalf("empty-root code=%d stderr=%q", code, emptyErr.String())
	}
	if !strings.Contains(emptyOut.String(), "no settlement provenance") {
		t.Fatalf("empty-root output = %q, want the empty notice", emptyOut.String())
	}
}

// TestWispGCDryRunEmitsNothing proves the dry-run default is inert for provenance
// too: when the sweep only logs (would-close) it emits no settlement.
func TestWispGCDryRunEmitsNothing(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		{
			ID: "mol-root", Status: "open", Type: "molecule",
			CreatedAt: now.Add(-30 * time.Minute), UpdatedAt: now.Add(-30 * time.Minute),
		},
		{ID: "mol-root.1", Status: "closed", Type: "task", CreatedAt: now.Add(-30 * time.Minute), ParentID: "mol-root"},
	})
	if err := store.DepAdd("mol-root.1", "mol-root", "parent-child"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	journal, emitter := openTestJournal(t)
	// closeAbandonedEnforced defaults to reading the (unset) env var → dry-run.
	withCloseAbandonedTTL(t, 5*time.Minute, func() {
		if err := closeAbandonedRoots(store, now, emitter); err != nil {
			t.Fatalf("closeAbandonedRoots: %v", err)
		}
	})

	facts := settlementFactsFor(t, journal, "mol-root")
	if len(facts) != 0 {
		t.Fatalf("dry-run emitted %d settlement facts, want 0", len(facts))
	}
}

// openTestJournalWithGS is openTestJournal plus the underlying graphstore handle
// and the lumen terminal vocabulary registered, so a test can append lumen run
// events directly to a root's run stream.
func openTestJournalWithGS(t *testing.T) (*beads.JournalStore, *graphstore.Store, dispatch.SettlementEmitter) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "journal.db")
	gs, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "cmd-prov-city"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	gs.RegisterEventType("lumen", "lumen.outcome.settled")
	gs.RegisterEventType("lumen", "lumen.run.closed")
	journal := beads.NewJournalStore(gs)
	return journal, gs, dispatch.NewJournalSettlementEmitter(journal)
}

// appendLumenRunEvent appends one lumen event to the run stream keyed by streamID.
func appendLumenRunEvent(t *testing.T, gs *graphstore.Store, streamID, typ, idem string, payload any) {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal lumen payload: %v", err)
	}
	head, err := gs.Head(context.Background(), streamID)
	if err != nil {
		t.Fatalf("head %s: %v", streamID, err)
	}
	if _, err := gs.Append(context.Background(), streamID, "lumen", head, 0,
		[]graphstore.JournalEvent{{Type: typ, IdemToken: idem, Payload: raw}}); err != nil {
		t.Fatalf("append lumen %s: %v", typ, err)
	}
}

// TestGraphJournalCmdRendersTwoStreamsGrouped is the M1 render gate: with BOTH a
// settlement/<root> stream and a lumen run stream <root> populated, the hidden
// read surface renders the two as SEPARATE per-stream tables (a stream header
// each, each with its own per-stream SEQ) — never one fake global sequence — and
// the render is deterministic (byte-identical across repeated calls).
func TestGraphJournalCmdRendersTwoStreamsGrouped(t *testing.T) {
	t.Parallel()
	journal, gs, emitter := openTestJournalWithGS(t)
	ctx := context.Background()
	root := "gcg-root"

	// Settlement stream: two v2 coarse facts (seq 1..2 in its own space).
	if err := emitter.EmitAttemptSettled(ctx, beads.SettlementEngineV2,
		dispatch.Settlement{Root: root, Bead: "gcg-log", Outcome: "fail", Attempt: 1}); err != nil {
		t.Fatalf("emit attempt: %v", err)
	}
	if err := emitter.EmitRootSettled(ctx, beads.SettlementEngineV2,
		dispatch.Settlement{Root: root, Bead: root, Outcome: "fail"}); err != nil {
		t.Fatalf("emit root: %v", err)
	}
	// Lumen run stream <root>: two lumen facts (seq 1..2 in ITS OWN space).
	appendLumenRunEvent(t, gs, root, "lumen.outcome.settled", "lo1",
		map[string]any{"activation": "impl:0", "outcome": "pass"})
	appendLumenRunEvent(t, gs, root, "lumen.run.closed", "lc1",
		map[string]any{"outcome": "pass"})

	var out bytes.Buffer
	if code := writeProvenanceTimeline(journal, root, &out, io.Discard); code != 0 {
		t.Fatalf("writeProvenanceTimeline code=%d", code)
	}
	got := out.String()

	// Both per-stream headers present — the two streams are grouped, not merged.
	for _, want := range []string{"stream settlement/" + root, "stream " + root, "lumen", "v2"} {
		if !strings.Contains(got, want) {
			t.Fatalf("render missing %q:\n%s", want, got)
		}
	}
	// The settlement header must precede the lumen run header (deterministic order).
	if strings.Index(got, "stream settlement/"+root) > strings.Index(got, "stream "+root+" ") {
		t.Fatalf("streams out of order (settlement must precede lumen run):\n%s", got)
	}

	// Deterministic: a second render is byte-identical.
	var out2 bytes.Buffer
	if code := writeProvenanceTimeline(journal, root, &out2, io.Discard); code != 0 {
		t.Fatalf("writeProvenanceTimeline (2) code=%d", code)
	}
	if got != out2.String() {
		t.Fatalf("render is not deterministic:\n first=%q\n second=%q", got, out2.String())
	}
}

// recordingCmdEmitter captures the last Settlement/engine an Emit* received.
type recordingCmdEmitter struct {
	engine string
	last   dispatch.Settlement
	calls  int
}

func (r *recordingCmdEmitter) EmitRootSettled(_ context.Context, engine string, s dispatch.Settlement) error {
	r.engine, r.last = engine, s
	r.calls++
	return nil
}

func (r *recordingCmdEmitter) EmitAttemptSettled(_ context.Context, engine string, s dispatch.Settlement) error {
	r.engine, r.last = engine, s
	r.calls++
	return nil
}

func (r *recordingCmdEmitter) EmitWorkflowFinalized(_ context.Context, engine string, s dispatch.Settlement) error {
	r.engine, r.last = engine, s
	r.calls++
	return nil
}

// TestRootSelfSettlementPayloadAlignsWithDispatcher is the LOW-2 alignment gate: a
// ROOT SELF-settlement built by the cmd closer mints the byte-identical journal
// payload the dispatcher's v2 root emit does (Kind omitted), so a root closed by
// BOTH a late v2 finalize and a reactive cmd close dedupes to one identical fact
// under the shared idem token instead of dropping the second as a divergent reuse.
func TestRootSelfSettlementPayloadAlignsWithDispatcher(t *testing.T) {
	t.Parallel()
	root := "gcg-root"

	// The dispatcher's v2 root self-settlement (dispatch.emitRootSettled): Kind
	// omitted (Settlement{Root, Bead, Outcome}).
	dispatchEv, err := beads.SettlementEvent(beads.SettlementRootType, beads.SettlementPayload{
		Root: root, Bead: root, Outcome: beadmeta.OutcomeFail,
	})
	if err != nil {
		t.Fatalf("dispatch SettlementEvent: %v", err)
	}

	// The cmd reactive root self-settlement for the SAME root, whose bead carries a
	// kind + graph.v2 contract.
	spy := &recordingCmdEmitter{}
	rootBead := beads.Bead{ID: root, Metadata: map[string]string{
		beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
		beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
	}}
	emitCmdRootSettlement(spy, root, rootBead, beadmeta.OutcomeFail)
	if spy.calls != 1 {
		t.Fatalf("emitCmdRootSettlement made %d emit calls, want 1", spy.calls)
	}
	if spy.engine != beads.SettlementEngineV2 {
		t.Fatalf("engine = %q, want v2 (graph.v2 contract)", spy.engine)
	}
	if spy.last.Kind != "" {
		t.Fatalf("root self-settlement Kind = %q, want empty (aligned with the dispatcher's Kind-less root emit)", spy.last.Kind)
	}
	cmdEv, err := beads.SettlementEvent(beads.SettlementRootType, beads.SettlementPayload{
		Root: spy.last.Root, Bead: spy.last.Bead, Kind: spy.last.Kind,
		Outcome: spy.last.Outcome, Attempt: spy.last.Attempt, StoreRef: spy.last.StoreRef,
	})
	if err != nil {
		t.Fatalf("cmd SettlementEvent: %v", err)
	}
	if cmdEv.IdemToken != dispatchEv.IdemToken {
		t.Fatalf("idem token diverges: cmd=%q dispatch=%q", cmdEv.IdemToken, dispatchEv.IdemToken)
	}
	if !bytes.Equal(cmdEv.Payload, dispatchEv.Payload) {
		t.Fatalf("root self-settlement payload diverges (a dual-close would drop the second as a divergent idem reuse):\n cmd=%s\n dispatch=%s",
			cmdEv.Payload, dispatchEv.Payload)
	}
}

// TestControlBeadSettlementRetainsKind is the negative half of LOW-2: when a
// DISTINCT control bead settles a (missing) root — Bead != Root, the orphan/
// quarantine closers — Kind stays as meaningful provenance, and cannot collide
// with a root self-settlement.
func TestControlBeadSettlementRetainsKind(t *testing.T) {
	t.Parallel()
	spy := &recordingCmdEmitter{}
	control := beads.Bead{ID: "gcg-ctl", Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindFanout}}
	emitCmdRootSettlement(spy, "gcg-missing-root", control, beadmeta.OutcomeFail)
	if spy.last.Kind != beadmeta.KindFanout {
		t.Fatalf("control-bead settlement Kind = %q, want %q (kept when Bead != Root)", spy.last.Kind, beadmeta.KindFanout)
	}
	if spy.last.Bead != "gcg-ctl" || spy.last.Root != "gcg-missing-root" {
		t.Fatalf("settlement = %+v, want bead=gcg-ctl root=gcg-missing-root", spy.last)
	}
}

// TestMoleculeAutocloseNoCloseOpensNoJournal is the LOW-3 gate: an autoclose that
// closes NOTHING must not build the (journal-opening) emitter at all.
func TestMoleculeAutocloseNoCloseOpensNoJournal(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	// A plain closed task belonging to no molecule root triggers no autoclose.
	b, _ := store.Create(beads.Bead{Title: "orphan task", Type: "task"})
	_ = store.Close(b.ID)

	built := 0
	lazy := &lazySettlementEmitter{build: func() dispatch.SettlementEmitter {
		built++
		return nil
	}}
	var out bytes.Buffer
	doMoleculeAutocloseWithEmitter(store, "", events.Discard, b.ID, &out, lazy)
	if built != 0 {
		t.Fatalf("lazy emitter built %d times on a no-close autoclose, want 0 (must not open the journal)", built)
	}
}

// TestMoleculeAutocloseLazyBuildsOnClose proves the LOW-3 lazy emitter still fires
// (exactly once) when a close actually happens, and the fact lands.
func TestMoleculeAutocloseLazyBuildsOnClose(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	root, _ := store.Create(beads.Bead{Title: "mol", Type: "molecule"})
	step, _ := store.Create(beads.Bead{Title: "a", Type: "step", ParentID: root.ID})
	_ = store.Close(step.ID)

	journal, realEmitter := openTestJournal(t)
	built := 0
	lazy := &lazySettlementEmitter{build: func() dispatch.SettlementEmitter {
		built++
		return realEmitter
	}}
	var out bytes.Buffer
	doMoleculeAutocloseWithEmitter(store, "", events.Discard, step.ID, &out, lazy)
	if built != 1 {
		t.Fatalf("lazy emitter built %d times on a real close, want exactly 1", built)
	}
	facts := settlementFactsFor(t, journal, root.ID)
	if len(facts) != 1 {
		t.Fatalf("timeline has %d facts, want 1: %+v", len(facts), facts)
	}
}

// TestWispAutocloseEmitsRootSettlement is the M3 anchor gate: the on-close
// wisp-autoclose closer (the sibling of molecule-autoclose on the same event path)
// emits exactly one coarse settlement.root for each attached root it actually
// closes.
func TestWispAutocloseEmitsRootSettlement(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	work, _ := store.Create(beads.Bead{Title: "work item"})
	attached, _ := store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: work.ID})
	_ = store.Close(work.ID)

	journal, emitter := openTestJournal(t)
	var out bytes.Buffer
	doWispAutocloseWithEmitter(store, work.ID, &out, emitter)

	after, _ := store.Get(attached.ID)
	if after.Status != "closed" {
		t.Fatalf("attached molecule status = %q, want closed", after.Status)
	}
	facts := settlementFactsFor(t, journal, attached.ID)
	if len(facts) != 1 {
		t.Fatalf("timeline has %d facts, want exactly 1 (one per attached root closed): %+v", len(facts), facts)
	}
	f := facts[0]
	if f.Engine != beads.SettlementEngineV1 || f.Type != beads.SettlementRootType || f.Root != attached.ID || f.Bead != attached.ID {
		t.Fatalf("fact = %+v, want {engine=v1 type=settlement.root root=bead=%s}", f, attached.ID)
	}
}

// TestWispAutocloseNilEmitterByteIdentity proves the M3 anchor is strictly
// after-the-fact: the attached root closes byte-identically with a nil emitter and
// with a journal emitter.
func TestWispAutocloseNilEmitterByteIdentity(t *testing.T) {
	t.Parallel()
	build := func() (beads.Store, string, string) {
		store := beads.NewMemStore()
		work, _ := store.Create(beads.Bead{Title: "work item"})
		attached, _ := store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: work.ID})
		_ = store.Close(work.ID)
		return store, work.ID, attached.ID
	}

	nilStore, nilWork, nilAttached := build()
	var nilOut bytes.Buffer
	doWispAutocloseWithEmitter(nilStore, nilWork, &nilOut, nil)

	emitStore, emitWork, emitAttached := build()
	_, emitter := openTestJournal(t)
	var emitOut bytes.Buffer
	doWispAutocloseWithEmitter(emitStore, emitWork, &emitOut, emitter)

	nilAfter, _ := nilStore.Get(nilAttached)
	emitAfter, _ := emitStore.Get(emitAttached)
	if nilAfter.Status != emitAfter.Status {
		t.Fatalf("attached status differs: nil=%s emit=%s", nilAfter.Status, emitAfter.Status)
	}
	if nilOut.String() != emitOut.String() {
		t.Fatalf("stdout differs: nil=%q emit=%q", nilOut.String(), emitOut.String())
	}
}

// TestControlQuarantineEmitsRootSettlement is the M2 gate: a non-transient
// ProcessControl failure quarantines the control bead and then emits exactly ONE
// coarse settlement.root — keyed on the workflow root, the control as the settled
// bead, outcome fail, engine derived from the control's contract — on an opted
// city's shared journal, strictly after the quarantine column write.
func TestControlQuarantineEmitsRootSettlement(t *testing.T) {
	clearGCEnv(t)

	cityPath := t.TempDir()
	if err := migrateGraphJournalInit(cityPath); err != nil {
		t.Fatalf("graph-journal init: %v", err)
	}

	store := beads.NewMemStore()
	workflow, _ := store.Create(beads.Bead{Title: "workflow", Type: "task", Metadata: map[string]string{
		"gc.kind": "workflow", "gc.formula_contract": "graph.v2",
	}})
	subject, _ := store.Create(beads.Bead{Title: "closed subject", Type: "task", Metadata: map[string]string{
		"gc.scope_ref": "missing-scope", "gc.scope_role": "member",
		"gc.root_bead_id": workflow.ID, "gc.outcome": "fail",
	}})
	_ = store.Close(subject.ID)
	control, _ := store.Create(beads.Bead{Title: "Finalize missing scope", Type: "task", Metadata: map[string]string{
		"gc.kind": "scope-check", "gc.root_bead_id": workflow.ID,
		"gc.scope_ref": "missing-scope", "gc.scope_role": "control",
	}})
	if err := store.DepAdd(control.ID, subject.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	var stderr bytes.Buffer
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	if err := runControlDispatcherWithStoreAndConfig(cityPath, cityPath, store, control, control.ID, cfg, io.Discard, &stderr); err != nil {
		t.Fatalf("runControlDispatcherWithStoreAndConfig: %v", err)
	}

	after, _ := store.Get(control.ID)
	if after.Status != "closed" || after.Metadata[beadmeta.ControlQuarantinedMetadataKey] != "true" {
		t.Fatalf("control not quarantined: status=%q quarantined=%q", after.Status, after.Metadata[beadmeta.ControlQuarantinedMetadataKey])
	}

	journal := cachedCityGraphJournal(cityPath)
	if journal == nil {
		t.Fatal("city has no graph journal after init")
	}
	facts := settlementFactsFor(t, journal, workflow.ID)
	if len(facts) != 1 {
		t.Fatalf("timeline has %d facts, want exactly 1: %+v", len(facts), facts)
	}
	f := facts[0]
	if f.Engine != beads.SettlementEngineV1 || f.Type != beads.SettlementRootType ||
		f.Root != workflow.ID || f.Bead != control.ID || f.Outcome != beadmeta.OutcomeFail {
		t.Fatalf("fact = %+v, want {engine=v1 type=settlement.root root=%s bead=%s outcome=fail}", f, workflow.ID, control.ID)
	}
}

// TestControlQuarantineEmitDoesNotAffectQuarantine proves the M2 emit is inert to
// the quarantine both ways: a failing emitter and a nil emitter each leave the
// quarantine column write (closed / fail / control_quarantined) untouched.
func TestControlQuarantineEmitDoesNotAffectQuarantine(t *testing.T) {
	t.Parallel()
	build := func() (beads.Store, beads.Bead) {
		store := beads.NewMemStore()
		workflow, _ := store.Create(beads.Bead{
			Title: "workflow", Type: "task",
			Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
		})
		control, _ := store.Create(beads.Bead{
			Title: "ctl", Type: "task",
			Metadata: map[string]string{"gc.kind": "scope-check", "gc.root_bead_id": workflow.ID},
		})
		return store, control
	}
	quarantineThenEmit := func(emitter dispatch.SettlementEmitter) beads.Bead {
		store, control := build()
		cause := fmt.Errorf("%w: bad graph", dispatch.ErrControlGraphMalformed)
		if err := quarantineControlFailureBead(store, control.ID, cause); err != nil {
			t.Fatalf("quarantineControlFailureBead: %v", err)
		}
		// Mirror the cmd path: emit AFTER the quarantine column write.
		root := control.Metadata[beadmeta.RootBeadIDMetadataKey]
		emitCmdRootSettlement(emitter, root, control, beadmeta.OutcomeFail)
		after, _ := store.Get(control.ID)
		return after
	}

	for name, emitter := range map[string]dispatch.SettlementEmitter{
		"failing": cmdFailingEmitter{err: errors.New("journal boom")},
		"nil":     nil,
	} {
		after := quarantineThenEmit(emitter)
		if after.Status != "closed" ||
			after.Metadata[beadmeta.OutcomeMetadataKey] != beadmeta.OutcomeFail ||
			after.Metadata[beadmeta.ControlQuarantinedMetadataKey] != "true" {
			t.Fatalf("%s emitter altered the quarantine: %+v", name, after)
		}
	}
}
