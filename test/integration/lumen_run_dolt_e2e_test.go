//go:build integration

package integration

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// Dolt-backed `run` (sub-formula) e2e's (slice R1c). They prove the run kind's
// transparent-outcome + value-plumbing behavior end-to-end on a real city: a
// top-level `run` inlines a sub-formula whose `do` node is dispatched as an
// ORDINARY pooled work bead under a `<runID>/` namespace, claimed and closed by a
// native pool worker, observed by the controller, and folded up through the
// transparent run aggregate — exactly like a plain do, but one namespace deep and
// with the sub-formula's result flowing back to the parent scope. Same dolt harness
// as lumen_do_dolt_e2e_test.go (file-provider can't do cross-process claiming).

func runDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "run-do.lumen.json")
}

func runChainDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "run-do-chain.lumen.json")
}

// runGreeterIRPath is the bundle PRODUCED by gascity-tools/scripts/bundle-lumen.mjs
// from scripts/fixtures/lumen-bundle/run-greeter.formula (R2). It proves the
// producer's output — not a hand-authored bundle — runs end-to-end.
func runGreeterIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "run-greeter.lumen.json")
}

// TestLumenRunDoltE2E_ProducedBundle (R2 acceptance) proves the bundle producer's
// output runs on a real city: the PRODUCED run-greeter bundle (main `run greeter`,
// greeter a single do rendering `{{ name }}` <- who) slings, dispatches the
// namespaced sub-do greeting/hello, a pooled worker claims+closes it, and the
// transparent run seals pass — with the sub-do prompt rendered through the run
// boundary exactly as the hand-authored fixtures, but from a compiler-produced
// bundle.
func TestLumenRunDoltE2E_ProducedBundle(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	const subActivation = "greeting/hello:0"
	const runActivation = "greeting:0"
	const wantPrompt = "Say hello to Gas City, then settle this step."

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, runGreeterIRPath(t), "--input", `{"who":"Gas City"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (produced bundle) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF produced-bundle streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}
	_ = waitForPooledSession(t, cityDir, lumenDoRoute, 90*time.Second)
	admitted := waitForOwnedAdmitted(t, gs, streamID, 60*time.Second)
	realBeadID := admitted.BeadID

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 4*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	if got := outcomeSettledFor(t, events, subActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled %s = %q, want pass", subActivation, got)
	}
	if got := outcomeSettledFor(t, events, runActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled %s = %q, want pass (transparent run)", runActivation, got)
	}
	assertLumenPromptReadback(t, cityDir, realBeadID, wantPrompt)
	t.Logf("PROOF produced bundle sealed pass; sub-do prompt readback = %q (producer -> engine -> seal)", wantPrompt)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}

func scatterRetryDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "scatter-retry-do.lumen.json")
}

func guardDoIRPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(repoRoot(t), "examples", "lumen", "guard-do.lumen.json")
}

// TestLumenGuardDoltE2E_CondTrueDispatchesThen (guard acceptance) proves a guard's
// decision arm on the pool: with mode=go the cond is true, so the guard dispatches
// its `then` do as ordinary work; a pooled worker claims+closes it; the guard settles
// transparently from it and the run seals pass. (mode!=go would settle the guard pass
// with no dispatch — the no-op branch, covered by the Advance unit test.)
func TestLumenGuardDoltE2E_CondTrueDispatchesThen(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()
	const wantPrompt = "Do the gated work, then settle this step."

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, guardDoIRPath(t), "--input", `{"mode":"go"}`)
	if err != nil {
		t.Fatalf("gc lumen sling (guard-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF guard-do streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}
	admitted := waitForOwnedAdmitted(t, gs, streamID, 60*time.Second)
	realBeadID := admitted.BeadID

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 4*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	if got := outcomeSettledFor(t, events, "gthen:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled gthen:0 = %q, want pass (the gated do)", got)
	}
	if got := outcomeSettledFor(t, events, "g:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled g:0 = %q, want pass (guard transparent from then)", got)
	}
	assertLumenPromptReadback(t, cityDir, realBeadID, wantPrompt)
	t.Logf("PROOF guard cond-true dispatched the then do -> sealed pass; prompt readback = %q", wantPrompt)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}

// TestLumenRetryInScatterDoltE2E (RN acceptance) proves retry loops nested under a
// scatter drive on the real pool: `scatter { r1: retry{do laneA}, r2: retry{do
// laneB} }` slings, BOTH lane do's dispatch as ordinary work beads (a retry loop is
// a legal scatter member now), two pooled workers claim+close them pass, each retry
// settles pass, and the scatter aggregate seals pass — the mol-review-quorum lane
// shape on a live city.
func TestLumenRetryInScatterDoltE2E(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 2, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, scatterRetryDoIRPath(t))
	if err != nil {
		t.Fatalf("gc lumen sling (scatter-retry-do) failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF scatter-retry-do streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 5*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	// Both lane do bodies (attempt 0) settled pass on the pool.
	if got := outcomeSettledFor(t, events, "laneA:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled laneA:0 = %q, want pass", got)
	}
	if got := outcomeSettledFor(t, events, "laneB:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled laneB:0 = %q, want pass", got)
	}
	// The scatter aggregated both retry-loop members → pass.
	if got := outcomeSettledFor(t, events, "lanes:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled lanes:0 = %q, want pass (scatter over two retry-do lanes)", got)
	}
	// Both lane do's were dispatched as ordinary work beads (>= 2 admits).
	if n := len(lumenEventsOfType(events, engine.EventOwnedAdmitted)); n < 2 {
		t.Fatalf("owned.admitted count = %d, want >= 2 (both retry-do lanes dispatched)", n)
	}
	t.Logf("PROOF both retry-do lanes sealed pass under the scatter; run.closed pass")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}

// TestLumenRunDoltE2E_TransparentSubDo (the run crux) proves a top-level `run` of a
// one-do sub-formula seals transparently through the real-bead path: `gc lumen sling`
// of the bundle → the controller dispatches the REAL work bead for the NAMESPACED sub
// activation greeting/hello:0 (its prompt rendered from the environment binding
// name<-who) → a native pooled worker claims + closes it pass → the controller observes
// → the sub settles → the transparent run aggregate greeting:0 settles pass → the run
// seals. ZERO owned.settled (ordinary claim/close, no Tier-B leg); ZERO control beads;
// the sub-do fold row is a plain step; graphstore.Verify clean.
func TestLumenRunDoltE2E_TransparentSubDo(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do.sh", 1, "GC_LUMEN_E2E_WORK_SECONDS=2")
	ctx := context.Background()

	const subActivation = "greeting/hello:0"
	const subNodeID = "greeting/hello"
	const runActivation = "greeting:0"
	// name<-who = "Gas City", so the sub-do's {{ name }} renders to this exact prompt.
	const wantPrompt = "Say hello to Gas City, then settle this step."

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, runDoIRPath(t), "--input", `{"who":"Gas City"}`)
	if err != nil {
		t.Fatalf("gc lumen sling failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	// The controller advanced the run and created the real bead for the namespaced sub-do.
	if _, err := waitForOwnedAdmittedOrDiag(t, gs, streamID, 3*time.Minute, cityDir); err != nil {
		t.Fatal(err)
	}
	claimant := waitForPooledSession(t, cityDir, lumenDoRoute, 90*time.Second)
	t.Logf("PROOF native-demand spawned pooled session %q", claimant.SessionName)

	admitted := waitForOwnedAdmitted(t, gs, streamID, 60*time.Second)
	if admitted.Kind != engine.OwnedKindWorkBead {
		t.Fatalf("owned.admitted kind = %q, want %q (real-bead path)", admitted.Kind, engine.OwnedKindWorkBead)
	}
	realBeadID := admitted.BeadID
	if realBeadID == "" || realBeadID == subNodeID {
		t.Fatalf("owned.admitted bead_id = %q, want a store-minted id (not the fold node id)", realBeadID)
	}
	t.Logf("PROOF owned.admitted kind=%s bead_id=%s (namespaced sub-do, ordinary claim path)", admitted.Kind, realBeadID)

	// Seal. The run-do journal is longer than a plain do (the transparent aggregate adds
	// its own node.activated + outcome.settled), so use the length-tolerant seal waiter.
	events := waitForLumenSealOrDiagRun(t, gs, streamID, 4*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	if got := outcomeSettledFor(t, events, subActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled %s = %q, want pass (the sub-do)", subActivation, got)
	}
	if got := outcomeSettledFor(t, events, runActivation); got != engine.OutcomePass {
		t.Fatalf("outcome.settled %s = %q, want pass (the TRANSPARENT run aggregate)", runActivation, got)
	}
	if n := len(lumenEventsOfType(events, engine.EventOwnedSettled)); n != 0 {
		t.Fatalf("owned.settled appeared %d× — a Tier-B close leg ran; the real-bead path settles via outcome.settled", n)
	}
	t.Logf("PROOF %s pass + transparent %s pass + run.closed pass; no owned.settled", subActivation, runActivation)

	// The sub-do bead is a REAL ordinary work bead, closed pass, run-linked at the
	// namespaced activation.
	assertLumenRealWorkBeadClosedDolt(t, cityDir, realBeadID, streamID, subActivation, engine.OutcomePass)

	// The sub-do fold row is a PLAIN step (no claimable Tier-A doppelganger).
	assertLumenDoFoldRowIsPlainStepDolt(t, journalPath, streamID, subNodeID)

	// THE env-through-run-boundary proof: the sub-do's dispatched prompt rendered
	// {{ name }} to the value the run bound from the parent input `who`.
	assertLumenPromptReadback(t, cityDir, realBeadID, wantPrompt)
	t.Logf("PROOF sub-do prompt readback = %q (env binding name<-who rendered through the run boundary)", wantPrompt)

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
	t.Logf("PROOF graphstore.Verify(%s) clean", streamID)
}

// TestLumenRunDoltE2E_ValuePlumbingThroughRunBoundary proves a sub-formula's do
// output flows out of the run boundary into a downstream parent do's prompt: the sub
// do closes with gc.output_json=aval; the transparent run output is aval; the
// downstream `consume` do's dispatched prompt resolves {{ greeting }} to "use aval"
// (not the unresolved "use {{ greeting }}"). The load-bearing proof is the consume
// prompt readback across a real pooled claim.
func TestLumenRunDoltE2E_ValuePlumbingThroughRunBoundary(t *testing.T) {
	cityDir, _ := setupLumenDoDoltCity(t, "lumen-do-chain.sh", 1, "")
	ctx := context.Background()

	slingOut, err := gcDolt(cityDir, "lumen", "sling", lumenDoRoute, runChainDoIRPath(t), "--input", `{"who":"Gas City"}`)
	if err != nil {
		t.Fatalf("gc lumen sling failed: %v\noutput: %s", err, slingOut)
	}
	streamID := parseLumenStreamID(t, slingOut)
	t.Logf("PROOF streamID = %s", streamID)

	journalPath := filepath.Join(cityDir, ".gc", "graph", "journal.db")
	gs, err := graphstore.Open(ctx, journalPath, graphstore.Options{})
	if err != nil {
		t.Fatalf("opening run journal %q: %v", journalPath, err)
	}
	defer func() { _ = gs.Close() }()

	events := waitForLumenSealOrDiagRun(t, gs, streamID, 6*time.Minute, cityDir)
	closed := decodeRunClosed(t, findEvent(t, events, engine.EventRunClosed).Payload)
	if closed.Outcome != engine.OutcomePass {
		t.Fatalf("run.closed outcome = %q, want pass", closed.Outcome)
	}
	if got := outcomeSettledFor(t, events, "greeting/hello:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled greeting/hello:0 = %q, want pass (sub-do producer)", got)
	}
	if got := outcomeSettledFor(t, events, "greeting:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled greeting:0 = %q, want pass (transparent run)", got)
	}
	if got := outcomeSettledFor(t, events, "consume:0"); got != engine.OutcomePass {
		t.Fatalf("outcome.settled consume:0 = %q, want pass (downstream consumer)", got)
	}

	byActivation := lumenDoltRunBeadsByActivation(t, cityDir, streamID)
	producer, okP := byActivation["greeting/hello:0"]
	consume, okC := byActivation["consume:0"]
	if !okP || !okC {
		t.Fatalf("both do beads must be queryable; have %v, want greeting/hello:0 and consume:0", keysOfBeads(byActivation))
	}
	if got := metaValue(producer, "gc.output_json"); got != "aval" {
		t.Fatalf("sub-do %s gc.output_json = %q, want aval", producer.ID, got)
	}
	// THE value-plumbing-through-run-boundary proof.
	assertLumenPromptReadback(t, cityDir, consume.ID, "use aval")
	t.Logf("PROOF sub-do gc.output_json=aval → transparent run output → consume prompt resolved to %q", "use aval")

	assertZeroControlBeadsDolt(t, cityDir, journalPath, streamID)
	if err := gs.Verify(ctx, streamID); err != nil {
		t.Fatalf("graphstore.Verify(%s) failed: %v", streamID, err)
	}
}
