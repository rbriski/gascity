package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// darActivatedSet returns the set of node ids carrying a node.activated row — the
// zero-mint assertions need activations, not just settles (an activated-unsettled row
// is invisible to settledOutcomeByID).
func darActivatedSet(t *testing.T, events []graphstore.StoredEvent) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	for _, e := range events {
		if e.Type != engine.EventNodeActivated {
			continue
		}
		var p struct {
			NodeID string `json:"node_id"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode node.activated payload: %v", err)
		}
		out[p.NodeID] = true
	}
	return out
}

// errInjectedDARCrash is the sentinel the DAR after-dispatch crash test injects.
var errInjectedDARCrash = errors.New("injected DAR crash after dispatch")

// --- dispatch-arm-body=run (DAR) behavioral fixtures ------------------------
//
// The corpus marquee: a `dispatch policy { "separate": run drainSeparate given {…},
// "same-session": run drainShared given {…} }` — the MATCHED arm mints the target
// sub-formula's WHOLE sub-graph under `<armBodyID>/`, the sub rendering the env-bound
// values; the arm aggregate is transparent at `<armBodyID>:0` and the dispatch settles
// transparently from it. The UNCHOSEN arm mints NOTHING.

// darEnvReviewer renders a run env binding of the sub-input `reviewer` to a literal value
// (the marquee per-arm literal distinguishing the two arms' renders).
func darEnvReviewer(lit string) string {
	return `{"name":"reviewer","value":{"kind":"expr","expr":{"kind":"literal","value":"` + lit + `"}}}`
}

// darEnvRef renders a run env binding to a bare parent-scope ref.
func darEnvRef(name, ref string) string {
	return `{"name":"` + name + `","value":{"kind":"expr","expr":{"kind":"ref","name":"` + ref + `"}}}`
}

// darRunArm renders one dispatch arm whose body is a run node (id armID) targeting the
// given sub-formula with the given raw environment.fields.
func darRunArm(match, armID, target, fieldsJSON string) string {
	return `{"match":{"kind":"literal","value":"` + match + `"},"body":` +
		runNodeRawEnv(armID, nil, target, fieldsJSON) + `}`
}

// darExecArm renders one "separate"-matched dispatch arm whose body is a plain
// exec leaf (id armID) — every caller matches the "separate" subject value.
func darExecArm(armID, script string) string {
	return `{"match":{"kind":"literal","value":"separate"},"body":` + execNode(armID, script, nil) + `}`
}

// darDoArm renders one dispatch arm whose body is a pool-materializable do (id armID).
func darDoArm(match, armID, prompt string) string {
	return `{"match":{"kind":"literal","value":"` + match + `"},"body":` + doNode(armID, prompt, nil) + `}`
}

// darDispatch renders a dispatch node (id "d") over subject ref subjectRef with arms.
func darDispatch(subjectRef string, arms ...string) string {
	return `{"kind":"dispatch","id":"d","name":"d","after":[],` +
		`"subject":{"kind":"ref","name":"` + subjectRef + `"},"arms":[` + strings.Join(arms, ",") + `]}`
}

// darLaneEnv is the marquee arm env: reviewer <- a per-arm literal, target <- an input ref.
func darLaneEnv(reviewerLit string) string {
	return `[` + darEnvReviewer(reviewerLit) + `,` + darEnvRef("target", "target") + `]`
}

// darLaneExecSub renders a sub-formula (accepts reviewer + target) with one exec node.
func darLaneExecSub(name string) string {
	return subDoc(name, strField("reviewer")+","+strField("target"),
		execNode("drain", `echo "item={{ reviewer }} target={{ target }}"`, nil))
}

// darLaneDoSub renders a sub-formula (accepts reviewer + target) with one do node.
func darLaneDoSub(name string) string {
	return subDoc(name, strField("reviewer")+","+strField("target"),
		doNode("drain", "drain {{ reviewer }} for {{ target }}", nil))
}

// TestDispatchRunArmCorpusInlineMintsChosen (§2.1 marquee, inline) proves the marquee at
// ROOT: a dispatch over `policy` with two RUN arms mints ONLY the matched arm's sub-graph
// (separate → sepLane/drain), rendering its distinct env (reviewer literal + input-bound
// target); the arm aggregate settles transparent at sepLane:0 and the dispatch settles
// transparently from it. The OTHER arm (sharedLane) has ZERO activations/rows.
func TestDispatchRunArmCorpusInlineMintsChosen(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")),
			darRunArm("same-session", "sharedLane", "drainShared", darLaneEnv("shared"))),
		darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "separate", "target": "release-7"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["sepLane/drain"]; got != "item=fanout target=release-7" {
		t.Errorf("chosen arm sub = %q, want %q", got, "item=fanout target=release-7")
	}
	if got := res.NodeOutputs["sepLane"]; got != "item=fanout target=release-7" {
		t.Errorf("arm aggregate output = %q, want the transparent sub output", got)
	}
	if got := res.NodeOutputs["d"]; got != "item=fanout target=release-7" {
		t.Errorf("dispatch output = %q, want the transparent arm output", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["sepLane/drain"] != engine.OutcomePass || settled["sepLane"] != engine.OutcomePass || settled["d"] != engine.OutcomePass {
		t.Errorf("settles = {sub:%q agg:%q d:%q}, want all pass", settled["sepLane/drain"], settled["sepLane"], settled["d"])
	}
	// The UNCHOSEN arm minted NOTHING: no aggregate, no sub-node, no output.
	if _, ok := settled["sharedLane"]; ok {
		t.Errorf("unchosen arm sharedLane settled %q, want ZERO activations", settled["sharedLane"])
	}
	if _, ok := settled["sharedLane/drain"]; ok {
		t.Errorf("unchosen arm sub sharedLane/drain settled, want ZERO activations")
	}
	if got := res.NodeOutputs["sharedLane"]; got != "" {
		t.Errorf("unchosen arm output = %q, want empty (never minted)", got)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestDispatchRunArmOtherValueMintsOtherArm (§2.1 both-values pin) proves the SAME doc
// selects the OTHER arm when the subject differs: policy="same-session" mints sharedLane,
// and sepLane is now the zero-activation arm.
func TestDispatchRunArmOtherValueMintsOtherArm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")),
			darRunArm("same-session", "sharedLane", "drainShared", darLaneEnv("shared"))),
		darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "same-session", "target": "rel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["sharedLane/drain"]; got != "item=shared target=rel" {
		t.Errorf("chosen arm sub = %q, want %q", got, "item=shared target=rel")
	}
	if got := res.NodeOutputs["d"]; got != "item=shared target=rel" {
		t.Errorf("dispatch output = %q, want the shared arm output", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if _, ok := settled["sepLane"]; ok {
		t.Errorf("unchosen arm sepLane settled, want ZERO activations")
	}
}

// TestAdvanceDispatchRunArmParksUnsettled (§2.2 ⚑B2 settle precondition) proves a pool run
// arm dispatches its sub-do and PARKS with the dispatch node still UNSETTLED — the settle
// precondition (settle ONLY when the arm agg is Settled). The settle-precondition-deletion
// mutant would settle the dispatch PASS here (settleDecisionFromBody defaults PASS/"" on the
// unsettled agg), so this assertion goes red under that mutant.
func TestAdvanceDispatchRunArmParksUnsettled(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout"))),
		darLaneDoSub("drainSeparate")))
	input := map[string]any{"policy": "separate", "target": "rel"}

	r1, err := engine.Advance(ctx, store, doc, "gcg-dar-park", input, fake.opts())
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if r1.Sealed || !r1.Parked {
		t.Fatalf("advance 1 = %+v, want Parked (the arm sub-do is dispatched, awaited)", r1)
	}
	if fake.dispatchCount() != 1 || len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != "sepLane/drain" {
		t.Fatalf("dispatch=%d inFlight=%+v, want one sub-do sepLane/drain", fake.dispatchCount(), r1.InFlight)
	}
	// The DISPATCH (and the arm aggregate) MUST stay UNSETTLED while parked.
	parked := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-park"))
	if _, ok := parked["d"]; ok {
		t.Fatalf("dispatch d settled %q while a sub-do is in flight; the settle precondition demands UNSETTLED", parked["d"])
	}
	if _, ok := parked["sepLane"]; ok {
		t.Fatalf("arm aggregate sepLane settled while its sub-do is in flight; want UNSETTLED")
	}
	if got := fake.dispatchPromptFor(t, "sepLane/drain:0"); got != "drain fanout for rel" {
		t.Errorf("sub-do prompt = %q, want %q (env-rendered arm sub)", got, "drain fanout for rel")
	}

	// Close the sub-do → next advance settles the arm agg + the dispatch, seals pass.
	fake.settleAct(t, "sepLane/drain:0", engine.OutcomePass, "done")
	r2, err := engine.Advance(ctx, store, doc, "gcg-dar-park", input, fake.opts())
	if err != nil || !r2.Sealed {
		t.Fatalf("advance 2 = %+v err %v, want Sealed", r2, err)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-park"))
	if settled["sepLane"] != engine.OutcomePass || settled["d"] != engine.OutcomePass {
		t.Errorf("final settles = {agg:%q d:%q}, want both pass", settled["sepLane"], settled["d"])
	}
	assertProjectionEqualsRefold(t, store, "gcg-dar-park")
}

// TestAdvanceDispatchRunArmWriteOnceRedundantAdvance (§2.2 dedupe / write-once + the parked
// same-arm re-select pin) proves, over a TWO-run-arm dispatch, that a redundant Advance with
// NO new settlement is a no-op: the chosen arm's sub-do stays in flight, no re-dispatch, no
// head movement — the stateless re-mint + write-once appends dedupe cleanly — AND the pure
// matchingArm re-select picks the SAME arm every pass: the second arm stays ZERO-minted (no
// activations, no settles) across the parked passes and through the seal.
func TestAdvanceDispatchRunArmWriteOnceRedundantAdvance(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")),
			darRunArm("same-session", "sharedLane", "drainShared", darLaneEnv("shared"))),
		darLaneDoSub("drainSeparate")+","+darLaneDoSub("drainShared")))
	input := map[string]any{"policy": "separate", "target": "rel"}
	r1, err := engine.Advance(ctx, store, doc, "gcg-dar-writeonce", input, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	head := r1.Head
	for i := 0; i < 2; i++ {
		r, err := engine.Advance(ctx, store, doc, "gcg-dar-writeonce", input, fake.opts())
		if err != nil || !r.Parked {
			t.Fatalf("redundant advance %d = %+v err %v, want Parked", i, r, err)
		}
		if r.Head != head {
			t.Fatalf("redundant advance %d moved head %d -> %d (a double append)", i, head, r.Head)
		}
		// The same arm is re-selected every pass: the UNCHOSEN arm stays zero-minted.
		activated := darActivatedSet(t, streamStored(t, store, "gcg-dar-writeonce"))
		if activated["sharedLane"] || activated["sharedLane/drain"] {
			t.Fatalf("redundant advance %d activated the unchosen arm (sharedLane rows); the pure re-select must pick the SAME arm", i)
		}
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("dispatch count across redundant advances = %d, want 1 (dispatched once; an already-bead'd sub-do re-mint is a no-op — write-once)", fake.dispatchCount())
	}
	// Close the sub-do → seal; the unchosen arm is still zero-minted at the end.
	fake.settleAct(t, "sepLane/drain:0", engine.OutcomePass, "done")
	r2, err := engine.Advance(ctx, store, doc, "gcg-dar-writeonce", input, fake.opts())
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("final advance = %+v err %v, want Sealed pass", r2, err)
	}
	final := darActivatedSet(t, streamStored(t, store, "gcg-dar-writeonce"))
	if final["sharedLane"] || final["sharedLane/drain"] {
		t.Fatalf("unchosen arm sharedLane has rows after seal; want ZERO mints across all passes")
	}
	assertProjectionEqualsRefold(t, store, "gcg-dar-writeonce")
}

// TestDispatchMixedLeafChosenBesideRun (§2.3, the routing-mutant killer) proves a MIXED
// dispatch whose LEAF arm is chosen runs the leaf exactly as today, with the sibling RUN arm
// ZERO-perturbed (no mint). The "any arm has bodyRun" mis-key mutant would route the matched
// LEAF arm into the run-mint driver (dispatchArmRunBody over a nil bodyRun) and break.
func TestDispatchMixedLeafChosenBesideRun(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darExecArm("leafArm", `echo "leaf-out"`),
			darRunArm("same-session", "runArm", "drainShared", darLaneEnv("shared")))+","+
			execNode("done", `echo "d={{ d }}"`, []string{"d"}),
		darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "separate", "target": "rel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["leafArm"]; got != "leaf-out" {
		t.Errorf("leaf arm output = %q, want leaf-out (leaf journal byte-identical to today)", got)
	}
	if got := res.NodeOutputs["d"]; got != "leaf-out" {
		t.Errorf("dispatch output = %q, want the leaf output (transparent)", got)
	}
	if got := res.NodeOutputs["done"]; got != "d=leaf-out" {
		t.Errorf("downstream = %q, want d=leaf-out", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if _, ok := settled["runArm"]; ok {
		t.Errorf("run arm runArm settled %q, want ZERO perturbation (leaf chosen)", settled["runArm"])
	}
	if _, ok := settled["runArm/drain"]; ok {
		t.Errorf("run arm sub runArm/drain settled, want ZERO perturbation")
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestDispatchMixedRunChosenBesideLeaf (§2.3) proves the MIXED dispatch's RUN arm mints when
// chosen, and the sibling LEAF arm's bodyID:0 stays nil (no leaf activation).
func TestDispatchMixedRunChosenBesideLeaf(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darExecArm("leafArm", `echo "leaf-out"`),
			darRunArm("same-session", "runArm", "drainShared", darLaneEnv("shared"))),
		darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "same-session", "target": "rel"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["runArm/drain"]; got != "item=shared target=rel" {
		t.Errorf("run arm sub = %q, want the minted env render", got)
	}
	if got := res.NodeOutputs["d"]; got != "item=shared target=rel" {
		t.Errorf("dispatch output = %q, want the run arm transparent output", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if _, ok := settled["leafArm"]; ok {
		t.Errorf("leaf arm leafArm settled %q, want nil (run arm chosen)", settled["leafArm"])
	}
}

// TestDispatchRunArmExecPoolInlineJournalParity (§2.9 residual routing pin) proves an
// EXEC-bodied run arm driven inline (Run) and pooled (Advance — routes to the run-arm mint
// and seals in ONE pass, never dispatching) journals BYTE-IDENTICALLY, and neither emits
// attempt.minted (Q-C stateless).
func TestDispatchRunArmExecPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout"))),
		darLaneExecSub("drainSeparate")))
	input := map[string]any{"policy": "separate", "target": "t"}

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, input)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-dar-parity", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec run arm seals inline)", r, err)
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (an exec run arm dispatches nothing)", fake.dispatchCount())
	}
	if n := countAttemptMinted(inRes.Events); n != 0 {
		t.Fatalf("inline attempt.minted = %d, want 0 (Q-C stateless)", n)
	}
	if n := countAttemptMinted(r.Run.Events); n != 0 {
		t.Fatalf("pool attempt.minted = %d, want 0 (Q-C stateless)", n)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestDispatchRunArmDownstreamReads (§2.5) pins the downstream consumers of a PASSING run
// arm: a `{{d}}` consumer renders the transparent output, and a sibling guard reading
// d.outcome == "pass" runs its then (the ⚑B1 outcome read of a transparent dispatch).
func TestDispatchRunArmDownstreamReads(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")))+","+
			execNode("consumer", `echo "got: {{ d }}"`, []string{"d"})+","+
			guardExecAfter("g", []string{"d"}, condOutcomeEq("d", "pass"), "gthen", `echo "guard ran"`),
		darLaneExecSub("drainSeparate")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "separate", "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// A {{d}} consumer renders the transparent output of the chosen arm.
	if got := res.NodeOutputs["consumer"]; got != "got: item=fanout target=t" {
		t.Errorf("downstream consumer = %q, want %q (transparent output plumbed)", got, "got: item=fanout target=t")
	}
	// A sibling guard reads d.outcome == "pass" and runs its then.
	if got := res.NodeOutputs["gthen"]; got != "guard ran" {
		t.Errorf("guard then = %q, want %q (d.outcome==pass downstream read)", got, "guard ran")
	}
}

// TestDispatchRunArmFailedTransparentPropagates (§2.5, the FAILED-arm clause) pins that a
// FAILED transparent run arm propagates failure downstream: the arm's sub-exec fails → the
// arm aggregate fails → the dispatch settles FAILED transparently, and BOTH downstream
// consumers skip-cascade — a node gated `after:[d]` AND a sibling guard whose cond reads
// d.outcome (a failed transparent decision is a step that ran and failed — sharply distinct
// from a no-match PASS, which does not skip-cascade).
func TestDispatchRunArmFailedTransparentPropagates(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	failSub := subDoc("drainSeparate", strField("reviewer")+","+strField("target"),
		execNode("drain", `echo "start"; exit 1`, nil))
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")))+","+
			execNode("after", `echo "after: {{ d }}"`, []string{"d"})+","+
			guardExecAfter("g", nil, condOutcomeEq("d", "failed"), "gthen", `echo "saw fail"`),
		failSub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "separate", "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["sepLane"] != engine.OutcomeFailed || settled["d"] != engine.OutcomeFailed {
		t.Errorf("settles = {agg:%q d:%q}, want both failed (transparent from the failed arm sub)", settled["sepLane"], settled["d"])
	}
	if settled["after"] != engine.OutcomeSkipped {
		t.Errorf("downstream after = %q, want skipped (a failed transparent dispatch skip-cascades its `after` dependents)", settled["after"])
	}
	// PINNED SEMANTIC (intended, surprising): a guard whose cond READS d.outcome never
	// gets to evaluate it after a FAILED d — cond-ref gates are BLOCKING afterDeps
	// (resolveDeps installs d:0 as the guard's gate to freeze the decision input), and a
	// failed gate skip-cascades. So `g: guard (d.outcome == "failed") -> then` settles
	// SKIPPED — the then NEVER runs even though the cond would have been true. Reading a
	// failure outcome downstream requires a construct whose dep is a DRAIN (recover/cleanup),
	// not a decision gate.
	if settled["g"] != engine.OutcomeSkipped {
		t.Errorf("sibling guard g = %q, want skipped (cond-ref gate on d is BLOCKING; the cond never evaluates)", settled["g"])
	}
	if _, ok := settled["gthen"]; ok {
		t.Errorf("guard then gthen settled %q, want never-run (the guard was skip-cascaded before deciding)", settled["gthen"])
	}
}

// condOutcomeEq builds `<ref>.outcome == <lit>`.
func condOutcomeEq(ref, lit string) string {
	return `{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"` + ref + `","field":"outcome"},{"kind":"literal","value":"` + lit + `"}]}`
}

// TestDispatchRunArmGateUnionAndCrossArmSkipCascade (§2.6) pins BOTH the arm-envRefs gate
// union AND the cross-arm skip-cascade: arm B's env reads a FAILED node; the subject selects
// arm A. The static union gates the WHOLE dispatch on arm B's failed env dep → the dispatch
// SKIPS (zero mints), sharply different from a no-match PASS.
func TestDispatchRunArmGateUnionAndCrossArmSkipCascade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// `bad` fails; arm B (same-session) binds target <- ref bad, so the dispatch gates on
	// bad:0 (the static union). Subject selects arm A (separate).
	badNode := execNode("bad", `echo x; exit 1`, nil)
	armAEnv := `[` + darEnvReviewer("a") + `,` + darEnvRef("target", "target") + `]`
	armBEnv := `[` + darEnvReviewer("b") + `,` + darEnvRef("target", "bad") + `]`
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		badNode+","+
			darDispatch("policy",
				darRunArm("separate", "sepLane", "drainSeparate", armAEnv),
				darRunArm("same-session", "sharedLane", "drainShared", armBEnv))+","+
			execNode("after", `echo "after: {{ d }}"`, []string{"d"}),
		darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "separate", "target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["d"] != engine.OutcomeSkipped {
		t.Errorf("dispatch d = %q, want skipped (the cross-arm env-dep failed → skip-cascade, NOT no-match pass)", settled["d"])
	}
	// Zero mints: the CHOSEN arm never materialized either.
	if _, ok := settled["sepLane"]; ok {
		t.Errorf("chosen arm sepLane settled %q, want ZERO mints (dispatch skip-cascaded)", settled["sepLane"])
	}
	// The downstream `after:[d]` skip-cascades off the skipped dispatch (unconditional:
	// it must SETTLE, and settle skipped).
	if settled["after"] != engine.OutcomeSkipped {
		t.Errorf("downstream after = %q, want skipped (skip-cascade off the skipped dispatch)", settled["after"])
	}
}

// TestDispatchRunArmSubjectGate (§2.6 subject gate) pins the subject-ref gate: the dispatch
// defers until the subject node settles, then selects the arm over the STABLE subject value.
func TestDispatchRunArmSubjectGate(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		strField("target"),
		execNode("pick", `printf separate`, nil)+","+
			darDispatch("pick",
				darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")),
				darRunArm("same-session", "sharedLane", "drainShared", darLaneEnv("shared"))),
		darLaneExecSub("drainSeparate")+","+darLaneExecSub("drainShared")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"target": "t"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["sepLane/drain"]; got != "item=fanout target=t" {
		t.Errorf("chosen arm (subject from node `pick`) = %q, want the separate arm mint", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if _, ok := settled["sharedLane"]; ok {
		t.Errorf("unchosen arm sharedLane settled, want ZERO activations")
	}
}

// TestDispatchRunArmDepthComposedForEachInArm (§2.4) pins the depth-composed arm: the arm
// target sub-formula body is a RUN-bodied for-each — the corpus armBody/fan/<i>/ THREE-deep
// mint (dispatch → sepLane/ → sepLane/fan/<i>/ → sepLane/fan/<i>/review). A guard + a leaf
// repeat loop in the target exercise nested decision/loop composition inside the arm.
func TestDispatchRunArmDepthComposedForEachInArm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// reviewLane: exec over the binder + tag.
	reviewLane := subDoc("reviewLane", strField("r")+","+strField("tag"),
		execNode("review", `echo "item={{ r }} target={{ tag }}"`, nil))
	// fanLane: a run-bodied for-each over `arr`, PLUS a guard reading the fan outcome, PLUS a
	// leaf repeat loop (one iteration) — nested composition inside the arm target.
	innerEnv := `[` + darEnvRef("r", "r") + `,` + darEnvRef("tag", "tag") + `]`
	loopBody := `{"kind":"exec","id":"tick","name":"tick","after":[],"interpreter":{"program":{"kind":"shell"}},"body":{"raw":"echo tick"},"exitMap":{"pass":[0],"retryable":[]}}`
	leafLoop := `{"kind":"repeat","id":"lp","name":"lp","after":["fan"],"iterationName":"iteration",` +
		`"cond":{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":1}]},"body":` + loopBody + `}`
	fanLane := subDoc("fanLane", arrField("arr")+","+strField("tag"),
		forEachNode(nil, "r", "continue", refOver("arr"),
			runNodeRawEnv("lane", nil, "reviewLane", innerEnv))+","+
			guardExecAfter("gg", []string{"fan"}, condOutcomeEq("fan", "pass"), "ggthen", `echo "fan ok"`)+","+
			leafLoop)
	armEnv := `[` + darEnvRef("arr", "items") + `,` + darEnvRef("tag", "label") + `]`
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+arrField("items")+","+strField("label"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "fanLane", armEnv)),
		reviewLane+","+fanLane))
	res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "separate", "items": []any{"a", "b"}, "label": "L"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	// The three-deep member sub-execs render through dispatch → arm ns → fan-member ns.
	if got := res.NodeOutputs["sepLane/fan/0/review"]; got != "item=a target=L" {
		t.Errorf("3-deep member 0 = %q, want item=a target=L", got)
	}
	if got := res.NodeOutputs["sepLane/fan/1/review"]; got != "item=b target=L" {
		t.Errorf("3-deep member 1 = %q, want item=b target=L", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["sepLane/fan"] != engine.OutcomePass || settled["sepLane"] != engine.OutcomePass || settled["d"] != engine.OutcomePass {
		t.Errorf("settles = {fan:%q arm:%q d:%q}, want all pass", settled["sepLane/fan"], settled["sepLane"], settled["d"])
	}
	if got := res.NodeOutputs["sepLane/ggthen"]; got != "fan ok" {
		t.Errorf("nested guard then = %q, want 'fan ok' (guard in arm target ran)", got)
	}
	if settled["sepLane/lp"] != engine.OutcomePass {
		t.Errorf("nested leaf loop sepLane/lp = %q, want pass (loop in arm target ran)", settled["sepLane/lp"])
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestDispatchRunArmRootDefaultGotcha (§2.8) pins the ga-ospbql FLIP of the root-default gotcha
// (the DAR twin of the DAD pins): an OMITTED subject input with a declared default is now SEEDED
// at genesis (resolveDeclaredInput lands the default in d.input, baseScope flattens it), so the
// subject evaluates "separate" → the run arm MATCHES and mints its whole sub-graph. Pre-INS the
// omitted default was never seeded and the dispatch silently PASSed with zero mints; this slice
// closes that gotcha.
func TestDispatchRunArmRootDefaultGotcha(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// policy has a declared DEFAULT "separate" but is OMITTED from the input map.
	defaultedPolicy := `{"name":"policy","type":{"kind":"atomic","name":"string"},"required":false,"body":false,"default":"separate"}`
	doc := decodeIR(t, bundleDoc(
		defaultedPolicy+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout"))),
		darLaneExecSub("drainSeparate")))
	res, err := engine.Run(ctx, store, doc, map[string]any{"target": "t"}) // policy omitted → seeded "separate"
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["d"] != engine.OutcomePass {
		t.Errorf("dispatch d = %q, want pass (transparent from the chosen arm)", settled["d"])
	}
	if settled["sepLane"] != engine.OutcomePass {
		t.Errorf("arm sepLane = %q, want pass (the seeded default 'separate' matched the run arm)", settled["sepLane"])
	}
	if got := res.NodeOutputs["sepLane/drain"]; got != "item=fanout target=t" {
		t.Errorf("chosen arm drain = %q, want item=fanout target=t (omitted default seeded the subject → match)", got)
	}
}

// TestDispatchRunArmDropRefold (§2.9 DET) pins that the '/'-bearing dynamic arm rows (the arm
// aggregate sepLane:0 and its sub-node sepLane/drain:0) refold byte-identically, for BOTH the
// matched-run and no-match cases.
func TestDispatchRunArmDropRefold(t *testing.T) {
	ctx := context.Background()
	for _, policy := range []string{"separate", "neither"} {
		store := newStore(t)
		doc := decodeIR(t, bundleDoc(
			strField("policy")+","+strField("target"),
			darDispatch("policy",
				darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")))+","+
				execNode("done", `echo "d={{ d }}"`, []string{"d"}),
			darLaneExecSub("drainSeparate")))
		res, err := engine.Run(ctx, store, doc, map[string]any{"policy": policy, "target": "t"})
		if err != nil {
			t.Fatalf("run(policy=%s): %v", policy, err)
		}
		assertProjectionEqualsRefold(t, store, res.StreamID)
	}
}

// TestAdvanceDispatchRunArmCrashAfterDispatchReAdopts (§2.9 crash window, pool) pins the
// crash seam over a run arm: the arm sub-do's work bead is created, the crash fires BEFORE
// its owned.admitted fact commits. The re-Advance re-looks-up the SAME bead (no duplicate)
// via the STATELESS re-mint route — never fast-pathing the (nil) arm agg into leaf machinery
// — then converges with a byte-identical refold.
func TestAdvanceDispatchRunArmCrashAfterDispatchReAdopts(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"policy": "separate", "target": "t"}
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout"))),
		darLaneDoSub("drainSeparate")))

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterDispatch && act == "sepLane/drain:0" {
			return errInjectedDARCrash
		}
		return nil
	})
	_, err := engine.Advance(ctx, store, doc, "gcg-dar-crashdisp", input, fake.opts())
	restore()
	if err == nil {
		t.Fatal("advance did not surface the injected crash")
	}
	if fake.dispatchCount() != 1 {
		t.Fatalf("DispatchWork calls before crash = %d, want 1", fake.dispatchCount())
	}
	// Re-Advance: the arm sub-do is re-looked-up (SAME bead, no dup) via the stateless
	// re-mint; still parked.
	r2, err := engine.Advance(ctx, store, doc, "gcg-dar-crashdisp", input, fake.opts())
	if err != nil || !r2.Parked || len(r2.InFlight) != 1 {
		t.Fatalf("re-advance = %+v err %v, want Parked with the sub-do in flight", r2, err)
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 1 {
		t.Fatalf("distinct beads minted = %d, want 1 (re-adopted, not re-minted)", minted)
	}
	// Close → seal, refold identity.
	fake.settleAllDispatchedPass()
	r3, err := engine.Advance(ctx, store, doc, "gcg-dar-crashdisp", input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("final advance = %+v err %v, want Sealed pass", r3, err)
	}
	assertProjectionEqualsRefold(t, store, "gcg-dar-crashdisp")
}

// TestDispatchRunArmCrashMidMemberResumes (§2.9 crash mid-mint, inline) pins the inline crash
// window: the crash fires AFTER the arm sub-do settled but before the arm aggregate settles
// (CrashAfterSettle on the sub-do). Resume re-mints via the kind route, reloads the settled
// sub, settles the arm agg + dispatch, converges. Host called exactly once (at-most-once).
func TestDispatchRunArmCrashMidMemberResumes(t *testing.T) {
	// The subject comes from a ROOT node (not an input) so injectCrashThenResume's nil input
	// still selects the arm; the arm env binds a LITERAL reviewer (no input ref).
	sub := subDoc("drainSeparate", strField("reviewer"),
		doNode("drain", "drain {{ reviewer }}", nil))
	env := `[` + darEnvReviewer("fanout") + `]`
	docJSON := bundleDoc(
		"",
		execNode("pick", `printf separate`, nil)+","+
			darDispatch("pick", darRunArm("separate", "sepLane", "drainSeparate", env)),
		sub)
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"sepLane/drain": {Outcome: enginehost.OutcomePass, Output: "r0"},
	}}
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, docJSON), host,
		engine.CrashAfterSettle, "sepLane/drain:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	if settled["sepLane"] != engine.OutcomePass || settled["d"] != engine.OutcomePass {
		t.Errorf("resumed settles = {agg:%q d:%q}, want both pass (re-minted via kind route)", settled["sepLane"], settled["d"])
	}
	if calls := host.Calls(); len(calls) != 1 {
		t.Errorf("host calls = %d across crash+resume, want 1 (at-most-once; sub reloaded, not re-invoked)", len(calls))
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestAdvanceDispatchRunArmChosenAggSettledWindowResumes (⚑B2 §2.2, the chosenArm route —
// agg-SETTLED window) drives the ONLY state in which chosenArm fires: the crash between the
// arm aggregate's settle and the dispatch's settle. Pass 1 dispatches the arm sub-do and
// parks; the sub closes; pass 2 crashes at CrashAfterSettle("sepLane:0") — the arm aggregate
// is SETTLED on disk while the dispatch is UNSETTLED. Pass 3's chosenArm returns the run arm
// (its body node exists) and MUST take the kind-route re-mint (an idempotent no-op over the
// settled fold) then settle the dispatch — with no duplicate bead, no re-dispatch, and a
// byte-identical refold.
func TestAdvanceDispatchRunArmChosenAggSettledWindowResumes(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"policy": "separate", "target": "rel"}
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout"))),
		darLaneDoSub("drainSeparate")))

	r1, err := engine.Advance(ctx, store, doc, "gcg-dar-aggsettled", input, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	fake.settleAct(t, "sepLane/drain:0", engine.OutcomePass, "done")

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterSettle && act == "sepLane:0" {
			return errInjectedDARCrash
		}
		return nil
	})
	_, err = engine.Advance(ctx, store, doc, "gcg-dar-aggsettled", input, fake.opts())
	restore()
	if !errors.Is(err, errInjectedDARCrash) {
		t.Fatalf("advance 2 returned %v, want the injected crash at the arm aggregate settle", err)
	}
	// THE WINDOW: the arm aggregate is settled, the dispatch is UNSETTLED — chosenArm's
	// durable record exists while the decision is uncommitted.
	mid := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-aggsettled"))
	if mid["sepLane"] != engine.OutcomePass {
		t.Fatalf("crash window: arm aggregate sepLane = %q, want settled pass", mid["sepLane"])
	}
	if _, ok := mid["d"]; ok {
		t.Fatalf("crash window: dispatch d settled %q, want UNSETTLED (the window under test)", mid["d"])
	}

	// Pass 3: chosenArm → kind-route → idempotent re-mint → settle. No dup mints.
	r3, err := engine.Advance(ctx, store, doc, "gcg-dar-aggsettled", input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 3 = %+v err %v, want Sealed pass via the chosenArm kind-route", r3, err)
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 1 {
		t.Fatalf("distinct beads = %d, want 1 (the re-mint is a pure no-op, no dup bead)", minted)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-aggsettled"))
	if settled["d"] != engine.OutcomePass {
		t.Errorf("dispatch d = %q, want pass (settled from the already-settled arm aggregate)", settled["d"])
	}
	assertProjectionEqualsRefold(t, store, "gcg-dar-aggsettled")
}

// TestAdvanceDispatchRunArmChosenAggActivatedUnsettledWindowResumes (⚑B2 §2.2, THE flagship
// mutant killer — agg-ACTIVATED-UNSETTLED window) injects the crash BETWEEN the arm
// aggregate's two appends (CrashAfterActivate at "sepLane:0"): the aggregate node EXISTS in
// the fold but is UNSETTLED, so pass 3's chosenArm fires while the fast path is undecidable.
// chosenArm may ONLY skip re-matchingArm — the route MUST still be the kind-route re-mint
// (whose byte-identical node.activated re-append dedupes on the idem token) and the drive to
// settle. The mutant (chosenArm fast-pathing the run arm into the LEAF machinery) re-appends
// the same activation token with a DIVERGENT leaf payload (no Members edges) →
// ErrIdemTokenReuse — a loud wedge this test turns RED.
func TestAdvanceDispatchRunArmChosenAggActivatedUnsettledWindowResumes(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"policy": "separate", "target": "rel"}
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout"))),
		darLaneDoSub("drainSeparate")))

	r1, err := engine.Advance(ctx, store, doc, "gcg-dar-aggact", input, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	fake.settleAct(t, "sepLane/drain:0", engine.OutcomePass, "done")

	restore := engine.SetCrashHookForTest(func(b, _, act string) error {
		if b == engine.CrashAfterActivate && act == "sepLane:0" {
			return errInjectedDARCrash
		}
		return nil
	})
	_, err = engine.Advance(ctx, store, doc, "gcg-dar-aggact", input, fake.opts())
	restore()
	if !errors.Is(err, errInjectedDARCrash) {
		t.Fatalf("advance 2 returned %v, want the injected crash between the aggregate's two appends", err)
	}
	// THE WINDOW: the arm aggregate is ACTIVATED (chosenArm will fire) but UNSETTLED
	// (the fast path has nothing to settle from); the dispatch is unsettled too.
	events := streamStored(t, store, "gcg-dar-aggact")
	mid := settledOutcomeByID(t, events)
	activated := darActivatedSet(t, events)
	if !activated["sepLane"] {
		t.Fatalf("crash window: arm aggregate sepLane not activated — the boundary did not leave the activated-unsettled state")
	}
	if _, ok := mid["sepLane"]; ok {
		t.Fatalf("crash window: arm aggregate sepLane settled %q, want ACTIVATED-UNSETTLED", mid["sepLane"])
	}
	if _, ok := mid["d"]; ok {
		t.Fatalf("crash window: dispatch d settled %q, want UNSETTLED", mid["d"])
	}

	// Pass 3: chosenArm fires → the kind-route re-mints byte-identically (idem-token
	// dedupe), drives the aggregate to settle, and settles the dispatch. Converges.
	r3, err := engine.Advance(ctx, store, doc, "gcg-dar-aggact", input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 3 = %+v err %v, want Sealed pass (re-mint over the activated-unsettled aggregate)", r3, err)
	}
	fake.mu.Lock()
	minted := fake.seq
	fake.mu.Unlock()
	if minted != 1 {
		t.Fatalf("distinct beads = %d, want 1 (no dup mints across the crash)", minted)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-aggact"))
	if settled["sepLane"] != engine.OutcomePass || settled["d"] != engine.OutcomePass {
		t.Errorf("final settles = {agg:%q d:%q}, want both pass", settled["sepLane"], settled["d"])
	}
	assertProjectionEqualsRefold(t, store, "gcg-dar-aggact")
}

// TestDispatchRunArmAggActivatedUnsettledInlineResumes (⚑B2 §2.2, the inline twin of the
// flagship window) crashes the INLINE driver between the arm aggregate's two appends and
// resumes: matchingArm re-selects the same arm, the kind-route re-mints (byte-identical
// dedupe), the settled sub reloads (host at-most-once), the aggregate settles, the dispatch
// settles. The leaf-route mutant re-appends the aggregate activation with a divergent leaf
// payload and wedges loudly.
func TestDispatchRunArmAggActivatedUnsettledInlineResumes(t *testing.T) {
	sub := subDoc("drainSeparate", strField("reviewer"),
		doNode("drain", "drain {{ reviewer }}", nil))
	env := `[` + darEnvReviewer("fanout") + `]`
	docJSON := bundleDoc(
		"",
		execNode("pick", `printf separate`, nil)+","+
			darDispatch("pick", darRunArm("separate", "sepLane", "drainSeparate", env)),
		sub)
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"sepLane/drain": {Outcome: enginehost.OutcomePass, Output: "r0"},
	}}
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, docJSON), host,
		engine.CrashAfterActivate, "sepLane:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	if settled["sepLane"] != engine.OutcomePass || settled["d"] != engine.OutcomePass {
		t.Errorf("resumed settles = {agg:%q d:%q}, want both pass (kind-route re-mint over the activated-unsettled agg)", settled["sepLane"], settled["d"])
	}
	if calls := host.Calls(); len(calls) != 1 {
		t.Errorf("host calls = %d across crash+resume, want 1 (sub reloaded, not re-invoked)", len(calls))
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestAdvanceDispatchMixedLeafDoChosenBesideRun (§2.3 under ADVANCE — the routing branch's
// mixed coverage) proves a MIXED dispatch whose LEAF do arm is matched under a pool router
// parks/settles exactly as a pure-leaf dispatch does today, with the RUN sibling
// zero-perturbed across both passes (its dry-run already ran at lowering; no activations, no
// beads). The "any arm has bodyRun" mis-key in advanceDispatch would route this matched LEAF
// arm into the run-mint driver (a nil-bodyRun mint) and break loudly.
func TestAdvanceDispatchMixedLeafDoChosenBesideRun(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	input := map[string]any{"policy": "separate", "target": "rel"}
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darDoArm("separate", "leafArm", "drain leaf for {{ target }}"),
			darRunArm("same-session", "runArm", "drainShared", darLaneEnv("shared"))),
		darLaneDoSub("drainShared")))

	r1, err := engine.Advance(ctx, store, doc, "gcg-dar-mixedadv", input, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked (the leaf do arm is dispatched, awaited)", r1, err)
	}
	if len(r1.InFlight) != 1 || r1.InFlight[0].NodeID != "leafArm" {
		t.Fatalf("InFlight = %+v, want the leafArm do materialized", r1.InFlight)
	}
	if got := fake.dispatchPromptFor(t, "leafArm:0"); got != "drain leaf for rel" {
		t.Errorf("leaf arm prompt = %q, want %q (today's leaf render)", got, "drain leaf for rel")
	}
	activated := darActivatedSet(t, streamStored(t, store, "gcg-dar-mixedadv"))
	if activated["runArm"] || activated["runArm/drain"] {
		t.Fatalf("run sibling has activations after pass 1; want ZERO perturbation (leaf chosen)")
	}

	fake.settleAct(t, "leafArm:0", engine.OutcomePass, "leaf-out")
	r2, err := engine.Advance(ctx, store, doc, "gcg-dar-mixedadv", input, fake.opts())
	if err != nil || !r2.Sealed || r2.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 2 = %+v err %v, want Sealed pass", r2, err)
	}
	if got := r2.Run.NodeOutputs["d"]; got != "leaf-out" {
		t.Errorf("dispatch output = %q, want leaf-out (transparent from the leaf arm)", got)
	}
	final := darActivatedSet(t, streamStored(t, store, "gcg-dar-mixedadv"))
	if final["runArm"] || final["runArm/drain"] {
		t.Errorf("run sibling has rows after seal; want ZERO perturbation")
	}
	if fake.dispatchCount() != 1 {
		t.Errorf("dispatch count = %d, want 1 (only the leaf arm's do)", fake.dispatchCount())
	}
	assertProjectionEqualsRefold(t, store, "gcg-dar-mixedadv")
}

// TestAdvanceDispatchRunArmDepthComposedFanParks (§2.4 pool twin) drives the three-deep
// corpus composition on Advance: a matched run arm whose target holds a RUN-BODIED for-each
// (dispatch → sepLane/ → sepLane/fan/<i>/ → sepLane/fan/<i>/review). Pass 1 dispatches BOTH
// fan members' sub-dos in ONE pass (the concurrency survives the arm nesting) and parks with
// the dispatch UNSETTLED; the members settle across passes with byte-stable re-mints (no
// re-dispatch, stable env renders); the final pass drains the fan, seals the arm aggregate
// transparently, and settles the dispatch — refold byte-identical.
func TestAdvanceDispatchRunArmDepthComposedFanParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	reviewLane := subDoc("reviewLane", strField("r")+","+strField("tag"),
		doNode("review", "review {{ r }} for {{ tag }}", nil))
	innerEnv := `[` + darEnvRef("r", "r") + `,` + darEnvRef("tag", "tag") + `]`
	fanLane := subDoc("fanLane", arrField("arr")+","+strField("tag"),
		forEachNode(nil, "r", "continue", refOver("arr"),
			runNodeRawEnv("lane", nil, "reviewLane", innerEnv)))
	armEnv := `[` + darEnvRef("arr", "items") + `,` + darEnvRef("tag", "label") + `]`
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+arrField("items")+","+strField("label"),
		darDispatch("policy",
			darRunArm("separate", "sepLane", "fanLane", armEnv)),
		reviewLane+","+fanLane))
	input := map[string]any{"policy": "separate", "items": []any{"a", "b"}, "label": "L"}

	r1, err := engine.Advance(ctx, store, doc, "gcg-dar-depthfan", input, fake.opts())
	if err != nil || !r1.Parked {
		t.Fatalf("advance 1 = %+v err %v, want Parked", r1, err)
	}
	if fake.dispatchCount() != 2 || len(r1.InFlight) != 2 {
		t.Fatalf("dispatch=%d inFlight=%d, want BOTH fan members' sub-dos in ONE pass (concurrency through the arm)", fake.dispatchCount(), len(r1.InFlight))
	}
	if got := fake.dispatchPromptFor(t, "sepLane/fan/0/review:0"); got != "review a for L" {
		t.Errorf("member 0 prompt = %q, want %q (env threaded through the arm seam)", got, "review a for L")
	}
	if got := fake.dispatchPromptFor(t, "sepLane/fan/1/review:0"); got != "review b for L" {
		t.Errorf("member 1 prompt = %q, want %q", got, "review b for L")
	}
	mid := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-depthfan"))
	for _, id := range []string{"d", "sepLane", "sepLane/fan"} {
		if _, ok := mid[id]; ok {
			t.Fatalf("%s settled %q while the fan is in flight; want UNSETTLED (parked)", id, mid[id])
		}
	}

	// Member 0 closes; the fan (and everything above it) stays parked on member 1, and
	// the re-mint is byte-stable: no re-dispatch, no new beads.
	fake.settleAct(t, "sepLane/fan/0/review:0", engine.OutcomePass, "ok0")
	r2, err := engine.Advance(ctx, store, doc, "gcg-dar-depthfan", input, fake.opts())
	if err != nil || !r2.Parked {
		t.Fatalf("advance 2 = %+v err %v, want Parked (member 1 still open)", r2, err)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("dispatch count after mid-pass = %d, want 2 (byte-stable re-mints, no re-dispatch)", fake.dispatchCount())
	}
	mid2 := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-depthfan"))
	if mid2["sepLane/fan/0"] != engine.OutcomePass {
		t.Errorf("member 0 aggregate = %q, want pass (settled transparently mid-fan)", mid2["sepLane/fan/0"])
	}
	if _, ok := mid2["d"]; ok {
		t.Fatalf("dispatch d settled %q mid-fan; want UNSETTLED until the arm aggregate settles", mid2["d"])
	}

	// Member 1 closes → fan drains, arm aggregate seals transparently, dispatch settles.
	fake.settleAct(t, "sepLane/fan/1/review:0", engine.OutcomePass, "ok1")
	r3, err := engine.Advance(ctx, store, doc, "gcg-dar-depthfan", input, fake.opts())
	if err != nil || !r3.Sealed || r3.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance 3 = %+v err %v, want Sealed pass", r3, err)
	}
	settled := settledOutcomeByID(t, streamStored(t, store, "gcg-dar-depthfan"))
	for _, id := range []string{"sepLane/fan/0", "sepLane/fan/1", "sepLane/fan", "sepLane", "d"} {
		if settled[id] != engine.OutcomePass {
			t.Errorf("%s = %q, want pass (the three-deep transparent chain)", id, settled[id])
		}
	}
	assertProjectionEqualsRefold(t, store, "gcg-dar-depthfan")
}

// TestDispatchMixedLeafAndNoMatchPoolInlineJournalParity (§2.9 old-path parity) pins that the
// PRE-EXISTING dispatch paths — a chosen LEAF arm and a NO-MATCH pass — journal
// byte-identically inline (Run) vs pooled (Advance), THROUGH the new kind-route + gate-union
// code (the doc carries a run arm sibling so the new code is live on both drivers).
func TestDispatchMixedLeafAndNoMatchPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	doc := decodeIR(t, bundleDoc(
		strField("policy")+","+strField("target"),
		darDispatch("policy",
			darExecArm("leafArm", `echo "leaf-out"`),
			darRunArm("same-session", "runArm", "drainShared", darLaneEnv("shared"))),
		darLaneExecSub("drainShared")))
	for _, tc := range []struct{ name, policy string }{
		{"leaf-arm-chosen", "separate"},
		{"no-match", "neither"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := map[string]any{"policy": tc.policy, "target": "t"}
			inStore := newStore(t)
			inRes, err := engine.Run(ctx, inStore, doc, input)
			if err != nil {
				t.Fatalf("inline run: %v", err)
			}
			poolStore := newStore(t)
			fake := newFakeWorkStore()
			r, err := engine.Advance(ctx, poolStore, doc, "gcg-dar-parity-"+tc.name, input, fake.opts())
			if err != nil || !r.Sealed {
				t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec leaf / no-match)", r, err)
			}
			if fake.dispatchCount() != 0 {
				t.Fatalf("pool dispatch count = %d, want 0", fake.dispatchCount())
			}
			assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
		})
	}
}

// TestDispatchRunArmCrashPreMintResumes (§2.9 pre-mint crash cell) crashes BEFORE anything of
// the matched arm is on disk (CrashBeforeActivate at the arm's first sub-unit): the dispatch
// is activated and the arm was matched in memory, but no sub-node, no aggregate, no bead
// exists. Resume re-selects from scratch (pure matchingArm — no chosenArm record), re-mints,
// drives the sub (host exactly once), settles the chain. The ⚑B1 purity makes the re-select
// deterministic — the same arm, byte-identical mint.
func TestDispatchRunArmCrashPreMintResumes(t *testing.T) {
	sub := subDoc("drainSeparate", strField("reviewer"),
		doNode("drain", "drain {{ reviewer }}", nil))
	env := `[` + darEnvReviewer("fanout") + `]`
	docJSON := bundleDoc(
		"",
		execNode("pick", `printf separate`, nil)+","+
			darDispatch("pick", darRunArm("separate", "sepLane", "drainSeparate", env)),
		sub)
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"sepLane/drain": {Outcome: enginehost.OutcomePass, Output: "r0"},
	}}
	resumed, store, stream := injectCrashThenResume(t, decodeIR(t, docJSON), host,
		engine.CrashBeforeActivate, "sepLane/drain:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	settled := settledOutcomeByID(t, resumed.Events)
	if settled["sepLane/drain"] != engine.OutcomePass || settled["sepLane"] != engine.OutcomePass || settled["d"] != engine.OutcomePass {
		t.Errorf("resumed settles = {sub:%q agg:%q d:%q}, want all pass (pure re-select from scratch)", settled["sepLane/drain"], settled["sepLane"], settled["d"])
	}
	if calls := host.Calls(); len(calls) != 1 {
		t.Errorf("host calls = %d, want 1 (nothing was minted pre-crash; the sub runs exactly once on resume)", len(calls))
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestDispatchRunArmAuthoredAfterGate (§2.6 authored-after leg — the corpus dispatch carries
// after:["repeat_11"]) proves an authored `after` on a run-arm dispatch behaves as a gate:
// when the gate node PASSES the matched arm mints and the chain settles; when it FAILS the
// dispatch SKIP-CASCADES with ZERO mints (dispatchArmRunBody threads the dispatch's
// afterDeps/rawAfter into the mint, and the blocked() intercept fires before any arm drive).
func TestDispatchRunArmAuthoredAfterGate(t *testing.T) {
	ctx := context.Background()
	for _, tc := range []struct {
		name       string
		prepScript string
		wantD      string
		wantMint   bool
	}{
		{"gate-pass", `echo ready`, engine.OutcomePass, true},
		{"gate-fail", `exit 1`, engine.OutcomeSkipped, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := newStore(t)
			gatedDispatch := `{"kind":"dispatch","id":"d","name":"d","after":["prep"],` +
				`"subject":{"kind":"ref","name":"policy"},"arms":[` +
				darRunArm("separate", "sepLane", "drainSeparate", darLaneEnv("fanout")) + `]}`
			doc := decodeIR(t, bundleDoc(
				strField("policy")+","+strField("target"),
				execNode("prep", tc.prepScript, nil)+","+gatedDispatch,
				darLaneExecSub("drainSeparate")))
			res, err := engine.Run(ctx, store, doc, map[string]any{"policy": "separate", "target": "t"})
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			settled := settledOutcomeByID(t, res.Events)
			if settled["d"] != tc.wantD {
				t.Errorf("dispatch d = %q, want %q", settled["d"], tc.wantD)
			}
			activated := darActivatedSet(t, res.Events)
			if tc.wantMint {
				if settled["sepLane"] != engine.OutcomePass || res.NodeOutputs["sepLane/drain"] == "" {
					t.Errorf("gated-pass arm = {agg:%q sub:%q}, want the arm minted and settled", settled["sepLane"], res.NodeOutputs["sepLane/drain"])
				}
			} else if activated["sepLane"] || activated["sepLane/drain"] {
				t.Errorf("gate-fail minted the arm (sepLane rows); a skip-cascaded dispatch must mint NOTHING")
			}
			assertProjectionEqualsRefold(t, store, res.StreamID)
		})
	}
}
