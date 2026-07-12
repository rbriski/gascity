package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// --- dispatch-arm-body=run (DAR) lowering fixtures --------------------------

// darRunArm renders one dispatch arm whose body is a run node (id armBodyID) targeting
// the given sub-formula with the given environment.fields.
func darRunArm(match, armBodyID, target, envFields string) string {
	return `{"match":{"kind":"literal","value":` + jsonStr(match) + `},"body":` +
		`{"kind":"run","id":"` + armBodyID + `","name":"` + armBodyID + `","after":[],` +
		`"target":{"kind":"by-name","name":"` + target + `"},` +
		`"environment":{"fields":[` + envFields + `]},"outcome":"transparent"}}`
}

// darExecArm renders one dispatch arm whose body is a plain exec leaf (id armBodyID).
func darExecArm(match, armBodyID, script string) string {
	return `{"match":{"kind":"literal","value":` + jsonStr(match) + `},"body":` +
		execNode(armBodyID, nil, script) + `}`
}

// darDispatch renders a dispatch node (id "d") over subject ref subjectRef with the
// given raw arm JSONs.
func darDispatch(subjectRef string, arms ...string) string {
	return `{"kind":"dispatch","id":"d","name":"d","after":[],` +
		`"subject":{"kind":"ref","name":"` + subjectRef + `"},"arms":[` + strings.Join(arms, ",") + `]}`
}

// darLaneFormula renders a sub-formula (accepts reviewer + optional target) with nodes.
func darLaneFormula(name, nodes string) string {
	return `"` + name + `":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"` + name + `","input":{"name":"` + name + `.input","fields":[` +
		`{"name":"reviewer","type":{"kind":"atomic","name":"string"},"required":true,"body":false},` +
		`{"name":"target","type":{"kind":"atomic","name":"string"},"required":false,"body":false}]},` +
		`"nodes":[` + nodes + `]}`
}

// darMainDoc wraps top-level nodes + a formulas bundle into a full IR doc with policy +
// target string inputs (the dispatch subject + a shared env ref).
func darMainDoc(mainNodes, formulas string) string {
	return `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[` +
		`{"name":"policy","type":{"kind":"atomic","name":"string"},"required":true,"body":false},` +
		`{"name":"target","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + mainNodes + `],"formulas":{` + formulas + `}}`
}

// darCorpusEnv is the marquee env: reviewer <- a literal per-arm value, target <- the input.
func darCorpusEnv(reviewerLit string) string {
	return `{"name":"reviewer","value":{"kind":"expr","expr":{"kind":"literal","value":` + jsonStr(reviewerLit) + `}}},` +
		envF("target", "target")
}

// TestLowerDispatchRunArmStashesSpec (§1.1) pins the DAR lowering shape: a dispatch with
// a RUN arm lowers to a SINGLE unitDispatch whose matched arm carries bodyRun (+ the
// re-lowering context) and bodyIRKind==NodeRun, with NO sub-units emitted (the sub-graph
// is minted when the arm is matched). The run node's authored id becomes the arm body id.
func TestLowerDispatchRunArmStashesSpec(t *testing.T) {
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy",
			darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("fanout"))),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v (a dispatch run arm must lower)", err)
	}
	d := unitByNode(units, "d")
	if d == nil || d.kind != unitDispatch || d.dispatch == nil {
		t.Fatalf("d = %+v, want a unitDispatch", d)
	}
	if len(d.dispatch.arms) != 1 {
		t.Fatalf("arms = %d, want 1", len(d.dispatch.arms))
	}
	arm := d.dispatch.arms[0]
	if arm.bodyRun == nil || arm.bodyIRKind != ir.NodeRun {
		t.Fatalf("arm body = {bodyRun:%v kind:%q}, want a run body", arm.bodyRun, arm.bodyIRKind)
	}
	if arm.bodyNodeID != "sepLane" {
		t.Errorf("arm bodyNodeID = %q, want sepLane (the run node's authored id)", arm.bodyNodeID)
	}
	if arm.bodyRun.target != "reviewLane" {
		t.Errorf("bodyRun target = %q, want reviewLane", arm.bodyRun.target)
	}
	if arm.bodyFormula == nil || len(arm.bodyFormula.Nodes) != 1 {
		t.Errorf("bodyFormula = %+v, want the reviewLane sub-formula", arm.bodyFormula)
	}
	// No sub-units are lowered; neither is the arm run id as a real unit.
	if unitByNode(units, "sepLane") != nil || unitByNode(units, "sepLane/review") != nil {
		t.Errorf("arm sub-units must be runtime-minted; got %v", nodeIDs(units))
	}
}

// TestLowerDispatchRunArmMintCoordinates (§1.2, §2.1) pins the arm mint coordinates: the
// arm aggregate settles at sepLane:0 (nodeID sepLane), parented under the DISPATCH
// activation d:0, carries NO member index (a dispatch arm is not a fan member), and its
// sub-node lives one level deeper at sepLane/review.
func TestLowerDispatchRunArmMintCoordinates(t *testing.T) {
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy",
			darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("fanout"))),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	d := unitByNode(units, "d")
	arm := d.dispatch.arms[0]
	sub, agg, err := mintRunBody(arm.runBodyStash, arm.bodyRun, arm.bodyNodeID, arm.bodyNodeID+"/",
		activationFor(arm.bodyNodeID), d.activation, d.ns, d.afterDeps, d.rawAfter, nil)
	if err != nil {
		t.Fatalf("mint arm: %v", err)
	}
	if agg.activation != "sepLane:0" || agg.nodeID != "sepLane" || agg.irKind != ir.NodeRun {
		t.Errorf("arm agg = {%q %q %q}, want {sepLane:0 sepLane run}", agg.activation, agg.nodeID, agg.irKind)
	}
	if agg.parent != "d:0" {
		t.Errorf("arm agg parent = %q, want d:0 (parented under the dispatch)", agg.parent)
	}
	if agg.memberIndex != nil {
		t.Errorf("arm agg memberIndex = %v, want nil (an arm is not a fan member)", agg.memberIndex)
	}
	if agg.ns != "" {
		t.Errorf("arm agg ns = %q, want \"\" (dispatch is root-only)", agg.ns)
	}
	wantSub := "sepLane/review"
	if len(sub) != 1 || sub[0].nodeID != wantSub || sub[0].ns != "sepLane/" {
		t.Errorf("arm sub = %v, want [%s] in ns sepLane/", nodeIDs(sub), wantSub)
	}
	if len(agg.members) != 1 || agg.members[0] != wantSub+":0" {
		t.Errorf("arm agg members = %v, want [%s:0]", agg.members, wantSub)
	}
}

// TestLowerDispatchRunArmEnvRefGatesDispatch (§1.1.4, §2.6 gate) pins that a dispatch with
// a run arm gates on the parent NODES its arm body run's environment reads: target<-ref
// prep (a node) gates the dispatch on prep:0; an env ref to an input is no gate. The gate
// is the LIS separate-contribution (gate-only), UNIONED with the subject-ref gate.
func TestLowerDispatchRunArmEnvRefGatesDispatch(t *testing.T) {
	env := `{"name":"reviewer","value":{"kind":"expr","expr":{"kind":"literal","value":"x"}}},` + envF("target", "prep")
	doc := decodeBundle(t, darMainDoc(
		execNode("prep", nil, "echo p")+","+
			execNode("pick", nil, "echo separate")+","+
			darDispatch("pick", darRunArm("separate", "sepLane", "reviewLane", env)),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	d := unitByNode(units, "d")
	if d == nil {
		t.Fatalf("no dispatch unit; got %v", nodeIDs(units))
	}
	if !containsStr(d.afterDeps, "prep:0") {
		t.Errorf("dispatch afterDeps = %v, want the env-ref gate prep:0", d.afterDeps)
	}
	if !containsStr(d.afterDeps, "pick:0") {
		t.Errorf("dispatch afterDeps = %v, want the subject-ref gate pick:0 (union)", d.afterDeps)
	}
}

// TestLowerDispatchRunArmEnvRefSynthBodyExempt (§1.1.4 ban-exempt) pins that an arm env ref
// naming a SIBLING guard's synthesized then id is accepted-UNGATED (static-run parity): a
// synth id is never in byNodeID, so the DAR gate contributes no edge, and the env is
// ban-EXEMPT (unlike the subject ref, which keeps the synth-ban).
func TestLowerDispatchRunArmEnvRefSynthBodyExempt(t *testing.T) {
	env := `{"name":"reviewer","value":{"kind":"expr","expr":{"kind":"literal","value":"x"}}},` + envF("target", "gthen")
	doc := decodeBundle(t, darMainDoc(
		guardNode("g", nil, condRefEq("target", "go"), execNode("gthen", nil, "echo t"))+","+
			execNode("pick", nil, "echo separate")+","+
			darDispatch("pick", darRunArm("separate", "sepLane", "reviewLane", env)),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused an arm-env-ref-to-synth-body: %v (must be accepted-ungated)", err)
	}
	d := unitByNode(units, "d")
	if d == nil {
		t.Fatalf("no dispatch unit; got %v", nodeIDs(units))
	}
	if containsStr(d.afterDeps, "gthen:0") {
		t.Errorf("dispatch afterDeps = %v, want NO gthen-derived gate (synth env refs are gate-exempt)", d.afterDeps)
	}
}

// TestLowerDispatchRunArmEnvRefSilentClosureSubstituted (§1.1.4) pins that an arm env ref to
// a SILENT node is closure-substituted (gates on the silent value's non-silent inputs), NOT
// refused: target<-ref msg (a silent interp over {{seed}}) gates the dispatch on seed:0.
func TestLowerDispatchRunArmEnvRefSilentClosureSubstituted(t *testing.T) {
	silent := `{"kind":"interp","id":"msg","name":"msg","after":["seed"],"body":{"raw":"{{ seed }}"}}`
	env := `{"name":"reviewer","value":{"kind":"expr","expr":{"kind":"literal","value":"x"}}},` + envF("target", "msg")
	doc := decodeBundle(t, darMainDoc(
		execNode("seed", nil, "echo s")+","+silent+","+
			execNode("pick", nil, "echo separate")+","+
			darDispatch("pick", darRunArm("separate", "sepLane", "reviewLane", env)),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v (an env-ref-to-silent-node must closure-substitute, not refuse)", err)
	}
	d := unitByNode(units, "d")
	if d == nil {
		t.Fatalf("no dispatch unit; got %v", nodeIDs(units))
	}
	if !containsStr(d.afterDeps, "seed:0") {
		t.Errorf("dispatch afterDeps = %v, want the silent closure seed:0", d.afterDeps)
	}
	if containsStr(d.afterDeps, "msg:0") {
		t.Errorf("dispatch afterDeps = %v, want NO gate on the silent node msg:0 directly", d.afterDeps)
	}
}

// TestLowerDispatchSubjectRefSynthBanUnchanged (§2.7) pins that the SUBJECT ref keeps the
// synth-body ban (unlike arm envRefs): a dispatch whose subject names a sibling guard's
// synth then refuses loudly. (The subjectRefs footprint stays synth-banned.)
func TestLowerDispatchSubjectRefSynthBanUnchanged(t *testing.T) {
	doc := decodeBundle(t, darMainDoc(
		guardNode("g", nil, condRefEq("target", "go"), execNode("gthen", nil, "echo t"))+","+
			darDispatch("gthen", darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("fanout"))),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }}"`))))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") ||
		!strings.Contains(err.Error(), "gthen") || !strings.Contains(err.Error(), "dispatch") {
		t.Errorf("err = %v, want a dispatch subject-ref synth-body refusal naming gthen", err)
	}
}

// TestLowerDispatchRunArmEnvNamesArmBodyRefused (§1.1.4, §2.6) pins the RBL ⚑S5 parity: an
// arm env ref that names ANY of the SAME dispatch's arm body ids is refused loudly (the
// stable-"" oddity) — even naming ANOTHER arm's body id, and across the static union.
func TestLowerDispatchRunArmEnvNamesArmBodyRefused(t *testing.T) {
	// arm "separate" (body sepLane) binds target <- ref sharedLane (arm B's body id).
	envA := `{"name":"reviewer","value":{"kind":"expr","expr":{"kind":"literal","value":"x"}}},` + envF("target", "sharedLane")
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy",
			darRunArm("separate", "sepLane", "reviewLane", envA),
			darRunArm("same-session", "sharedLane", "reviewLane", darCorpusEnv("shared"))),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) ||
		!strings.Contains(err.Error(), "arm body id") || !strings.Contains(err.Error(), "sharedLane") {
		t.Fatalf("err = %v, want an arm-env-names-arm-body refusal naming sharedLane", err)
	}
}

// TestLowerDispatchRunArmSubjectCharsetBan (§1.1.2 ⚑B1, §2.7) pins the subject-ref charset
// ban: a subject ref carrying '/' or ':' is refused loudly (guard-cond parity). Load-bearing
// for the stateless re-select — an ungated '/'-forged subject would flip the arm mid-mint.
func TestLowerDispatchRunArmSubjectCharsetBan(t *testing.T) {
	dispatch := `{"kind":"dispatch","id":"d","name":"d","after":[],` +
		`"subject":{"kind":"ref","name":"a/b"},"arms":[` +
		darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("fanout")) + `]}`
	doc := decodeBundle(t, darMainDoc(dispatch,
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }}"`))))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) ||
		!strings.Contains(err.Error(), "reserved delimiters") || !strings.Contains(err.Error(), "subject") {
		t.Fatalf("err = %v, want a dispatch subject-ref charset refusal", err)
	}
}

// TestLowerDispatchArmBodyIdCharsetBan (§1.1.2 ⚑B1, §2.7) pins the arm-body-id charset ban:
// a forged arm body id carrying '/' or ':' is refused loudly. A forged id `armA/x` would
// alias arm A's minted sub-unit activation and corrupt chosenArm mid-mint.
func TestLowerDispatchArmBodyIdCharsetBan(t *testing.T) {
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy", darExecArm("separate", "arm/x", "echo a")),
		""))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) ||
		!strings.Contains(err.Error(), "reserved delimiters") || !strings.Contains(err.Error(), "body id") {
		t.Fatalf("err = %v, want an arm-body-id charset refusal", err)
	}
}

// TestLowerDispatchRunArmDecodeInheritance (§2.7) pins that the DAR arm inherits every
// decodeRunNode refusal (shared path, no drift) — the full six-row set: with-agent /
// runInput / non-transparent / missing-target / delimiter-bearing env ref / recursive
// cycle. The cycle fires through the arm DRY-RUN mint's targetStack (the arm target's
// sub-formula runs the arm's own target), composed under the arm provenance wrap.
func TestLowerDispatchRunArmDecodeInheritance(t *testing.T) {
	plainLane := darLaneFormula("reviewLane", execNode("review", nil, "echo hi"))
	arm := func(body string) string {
		return `{"match":{"kind":"literal","value":"separate"},"body":` + body + `}`
	}
	runBody := func(extra, target, env string) string {
		return `{"kind":"run","id":"sepLane","name":"sepLane","after":[],` + extra +
			`"target":{"kind":"by-name","name":"` + target + `"},` +
			`"environment":{"fields":[` + env + `]},"outcome":"transparent"}`
	}
	// The recursive-cycle sub: reviewLane's own body runs reviewLane again, so the arm's
	// dry-run mint (targetStack ["reviewLane"]) refuses the inner run's cycle.
	selfRecursiveLane := `"reviewLane":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"reviewLane","input":{"name":"reviewLane.input","fields":[` +
		`{"name":"reviewer","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + runNode("inner", nil, "reviewLane", "reviewer", "reviewer") + `]}`
	cases := []struct {
		name string
		body string
		sub  string
		want string
	}{
		{"with-agent override", runBody(`"with":{"kind":"agent","name":"x"},`, "reviewLane", darCorpusEnv("f")), plainLane, "with-agent override"},
		{"runInput form", runBody(`"runInput":{},`, "reviewLane", darCorpusEnv("f")), plainLane, "runInput form"},
		{"non-transparent outcome", `{"kind":"run","id":"sepLane","name":"sepLane","after":[],"target":{"kind":"by-name","name":"reviewLane"},"environment":{"fields":[` + darCorpusEnv("f") + `]},"outcome":"detached"}`, plainLane, "only transparent"},
		{"missing target", runBody("", "nonexistent", darCorpusEnv("f")), plainLane, "not present"},
		{"charset env ref", runBody("", "reviewLane", envF("target", "a/b")), plainLane, "reserved delimiters"},
		{"recursive cycle", runBody("", "reviewLane", envF("reviewer", "policy")), selfRecursiveLane, "cycle"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeBundle(t, darMainDoc(darDispatch("policy", arm(tc.body)), tc.sub))
			_, err := buildUnits(doc, true, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a %q refusal containing %q, got %v", tc.name, tc.want, err)
			}
		})
	}
}

// TestLowerDispatchRunArmDispatchInTargetFenced (§2.7 fence composition) pins that a DISPATCH
// inside an arm's target sub-formula refuses via the prefix fence (dispatch is root-only this
// slice), surfaced through the arm DRY-RUN with the arm provenance wrap composed on top.
func TestLowerDispatchRunArmDispatchInTargetFenced(t *testing.T) {
	// reviewLane's body is itself a dispatch — refused by the prefix fence, wrapped in the
	// arm run-body dry-run provenance.
	innerDispatch := `{"kind":"dispatch","id":"inner","name":"inner","after":[],` +
		`"subject":{"kind":"ref","name":"reviewer"},"arms":[` +
		darExecArm("x", "innerArm", "echo x") + `]}`
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy", darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("f"))),
		darLaneFormula("reviewLane", innerDispatch)))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") ||
		!strings.Contains(err.Error(), "sub-formula") {
		t.Fatalf("err = %v, want a dispatch-in-arm-target fence refusal wrapped in the arm dry-run provenance", err)
	}
	if !strings.Contains(err.Error(), "dispatch") || !strings.Contains(err.Error(), "arm") {
		t.Errorf("refusal %v should name the dispatch arm provenance", err)
	}
}

// TestLowerDispatchRunArmDryRunRefusesUnlowerableTarget (§1.1.3 ⚑S4) pins that a run arm whose
// target sub-formula contains an UN-LOWERABLE node refuses with the dispatch arm run-body
// provenance wrap — so a bad target refuses at buildUnits, before any effect. The DRY-RUN
// mints EVERY run arm (Q-B: arms target different formulas, so validating one validates none
// of the others).
func TestLowerDispatchRunArmDryRunRefusesUnlowerableTarget(t *testing.T) {
	// arm A (separate) targets a clean lane; arm B (same-session) targets an un-lowerable one
	// ('/'-forged repeat cond). The clean arm lowering must NOT mask arm B's refusal.
	badLane := darLaneFormula("badLane", repeatMemberForgedCond("darLoop", "darBody"))
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy",
			darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("f")),
			darRunArm("same-session", "sharedLane", "badLane", darCorpusEnv("s"))),
		darLaneFormula("reviewLane", execNode("review", nil, "echo ok"))+","+badLane))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") ||
		!strings.Contains(err.Error(), "reserved delimiter") {
		t.Fatalf("want a dispatch arm run-body dry-run refusal (does not lower / reserved delimiter), got %v", err)
	}
	if !strings.Contains(err.Error(), "dispatch") || !strings.Contains(err.Error(), "same-session") {
		t.Errorf("refusal %v should name the dispatch arm (same-session) provenance", err)
	}
}

// TestLowerDispatchBlockAndScatterArmRefused (§2.7) pins that a block AND a scatter arm body
// still refuse with the UPDATED message ("only exec/do leaf or run arm bodies").
func TestLowerDispatchBlockAndScatterArmRefused(t *testing.T) {
	block := `{"match":{"kind":"literal","value":"separate"},"body":` +
		`{"kind":"block","id":"blk","after":[],"members":[` + execNode("m", nil, "echo m") + `]}}`
	scatter := `{"match":{"kind":"literal","value":"separate"},"body":` +
		scatterOf("sc", execNode("m", nil, "echo m")) + `}`
	for _, tc := range []struct{ name, arm string }{{"block", block}, {"scatter", scatter}} {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeBundle(t, darMainDoc(darDispatch("policy", tc.arm), ""))
			_, err := buildUnits(doc, true, true)
			if err == nil || !errorsIsUnsupported(err) ||
				!strings.Contains(err.Error(), "only exec/do leaf or run arm bodies") {
				t.Fatalf("%s arm err = %v, want the updated 'only exec/do leaf or run arm bodies' refusal", tc.name, err)
			}
		})
	}
}

// TestLowerDispatchDuplicateArmBodyIdRefused (§2.7) pins the duplicate-arm-body-id refusal is
// unchanged for run arms (two arms sharing a body id collide on activationFor(bodyID)).
func TestLowerDispatchDuplicateRunArmBodyIdRefused(t *testing.T) {
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy",
			darRunArm("separate", "dup", "reviewLane", darCorpusEnv("a")),
			darRunArm("same-session", "dup", "reviewLane", darCorpusEnv("b"))),
		darLaneFormula("reviewLane", execNode("review", nil, "echo ok"))))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "duplicate arm body id") {
		t.Fatalf("err = %v, want a duplicate-arm-body-id refusal", err)
	}
}

// TestLowerDispatchRunArmSelfRefSubjectRefused (§2.7) pins the self-ref refusal at the arm
// position: a subject that reads a run arm's body id is refused (self-referential decision).
func TestLowerDispatchRunArmSelfRefSubjectRefused(t *testing.T) {
	doc := decodeBundle(t, darMainDoc(
		darDispatch("sepLane", darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("a"))),
		darLaneFormula("reviewLane", execNode("review", nil, "echo ok"))))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "self-referential") {
		t.Fatalf("err = %v, want a self-referential-decision refusal", err)
	}
}

// TestLowerDispatchMixedLeafAndRunArms (§2.3) pins that a MIXED dispatch (a leaf arm beside a
// run arm) lowers: the leaf arm keeps bodyRun nil (byte-identical to today), the run arm
// carries bodyRun. The branch keys on the MATCHED arm, not "any arm has bodyRun".
func TestLowerDispatchMixedLeafAndRunArms(t *testing.T) {
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy",
			darExecArm("separate", "leafArm", `echo "leaf"`),
			darRunArm("same-session", "runArm", "reviewLane", darCorpusEnv("shared"))),
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits (mixed): %v", err)
	}
	d := unitByNode(units, "d")
	if len(d.dispatch.arms) != 2 {
		t.Fatalf("arms = %d, want 2", len(d.dispatch.arms))
	}
	if d.dispatch.arms[0].bodyRun != nil || d.dispatch.arms[0].bodyIRKind != ir.NodeExec {
		t.Errorf("leaf arm = {bodyRun:%v kind:%q}, want a nil-bodyRun exec leaf (byte-identical)", d.dispatch.arms[0].bodyRun, d.dispatch.arms[0].bodyIRKind)
	}
	if d.dispatch.arms[1].bodyRun == nil || d.dispatch.arms[1].bodyIRKind != ir.NodeRun {
		t.Errorf("run arm = {bodyRun:%v kind:%q}, want a run body", d.dispatch.arms[1].bodyRun, d.dispatch.arms[1].bodyIRKind)
	}
}

// TestDispatchRunArmFixtureLowers guards the hand-authored dispatch-run-arm dolt-e2e bundle
// fixture: it decodes and lowers under BOTH the inline and controller-loop pool flag pairs,
// so a fixture typo fails fast here — not 10min into the e2e — and the chosen arm mints at
// sepLane/drain.
func TestDispatchRunArmFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "dispatch-run-arm.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for _, combineDo := range []bool{true, false} {
		units, err := buildUnits(doc, true, combineDo)
		if err != nil {
			t.Fatalf("lower dispatch-run-arm (allowCombineDo=%v): %v", combineDo, err)
		}
		d := unitByNode(units, "d")
		if d == nil || d.kind != unitDispatch || d.dispatch == nil || len(d.dispatch.arms) != 2 {
			t.Fatalf("no two-arm dispatch unit; got %v", nodeIDs(units))
		}
		var sepArm *dispatchArm
		for i := range d.dispatch.arms {
			if d.dispatch.arms[i].matchValue == "separate" {
				sepArm = &d.dispatch.arms[i]
			}
			if d.dispatch.arms[i].bodyRun == nil {
				t.Errorf("arm %q bodyRun = nil, want a run arm", d.dispatch.arms[i].matchValue)
			}
		}
		if sepArm == nil {
			t.Fatalf("no separate arm")
		}
		sub, agg, err := mintRunBody(sepArm.runBodyStash, sepArm.bodyRun, sepArm.bodyNodeID, sepArm.bodyNodeID+"/",
			activationFor(sepArm.bodyNodeID), d.activation, d.ns, d.afterDeps, d.rawAfter, nil)
		if err != nil {
			t.Fatalf("mint separate arm (allowCombineDo=%v): %v", combineDo, err)
		}
		if unitByNode(sub, sepArm.bodyNodeID+"/drain") == nil || agg.nodeID != sepArm.bodyNodeID {
			t.Fatalf("minted arm = %v (agg %q), want a sub-do at %s/drain under agg %s", nodeIDs(sub), agg.nodeID, sepArm.bodyNodeID, sepArm.bodyNodeID)
		}
	}
}

// TestLowerDispatchRunArmAuthoredAfterThreads (§2.6 authored-after leg — the corpus dispatch
// carries after:["repeat_11"]) pins that an authored `after` on a run-arm dispatch lands as
// the dispatch's gate AND threads into the arm mint: dispatchArmRunBody passes u.afterDeps /
// u.rawAfter to mintRunBody, which propagates the gate onto every minted sub-unit and the
// arm aggregate (fold-edge honesty across a drop+refold even though a gated-off dispatch
// never mints).
func TestLowerDispatchRunArmAuthoredAfterThreads(t *testing.T) {
	gatedDispatch := `{"kind":"dispatch","id":"d","name":"d","after":["prep"],` +
		`"subject":{"kind":"ref","name":"policy"},"arms":[` +
		darRunArm("separate", "sepLane", "reviewLane", darCorpusEnv("fanout")) + `]}`
	doc := decodeBundle(t, darMainDoc(
		execNode("prep", nil, "echo p")+","+gatedDispatch,
		darLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	d := unitByNode(units, "d")
	if d == nil {
		t.Fatalf("no dispatch unit; got %v", nodeIDs(units))
	}
	if !containsStr(d.afterDeps, "prep:0") {
		t.Fatalf("dispatch afterDeps = %v, want the authored gate prep:0", d.afterDeps)
	}
	arm := d.dispatch.arms[0]
	sub, agg, err := mintRunBody(arm.runBodyStash, arm.bodyRun, arm.bodyNodeID, arm.bodyNodeID+"/",
		activationFor(arm.bodyNodeID), d.activation, d.ns, d.afterDeps, d.rawAfter, nil)
	if err != nil {
		t.Fatalf("mint arm: %v", err)
	}
	if !containsStr(agg.afterDeps, "prep:0") {
		t.Errorf("arm aggregate afterDeps = %v, want the threaded gate prep:0", agg.afterDeps)
	}
	if !containsStr(agg.rawAfter, "prep") {
		t.Errorf("arm aggregate rawAfter = %v, want the threaded raw gate prep", agg.rawAfter)
	}
	if len(sub) != 1 || !containsStr(sub[0].afterDeps, "prep:0") {
		t.Errorf("minted sub afterDeps = %v, want the propagated gate prep:0 on every sub-unit", nodeIDs(sub))
	}
}
