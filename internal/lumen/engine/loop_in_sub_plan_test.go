package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// --- loop-in-sub-formula lowering construction helpers (LIS slice) ------------

// lisSubFormula renders a sub-IR doc entry with an explicit input-fields JSON array
// and node list (greeterFormula hardcodes a single `name` field, which the marquee's
// number-typed sub input cannot express).
func lisSubFormula(name, inputFieldsJSON, nodesJSON string) string {
	return `"` + name + `":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"` + name + `","input":{"name":"` + name + `.input","fields":[` + inputFieldsJSON + `]},` +
		`"nodes":[` + nodesJSON + `]}`
}

// lisMaxRoundsField renders the marquee's required number-typed `max_review_rounds` budget
// input field (the two-digit typed sub input the loop cond compares numerically).
func lisMaxRoundsField() string {
	return `{"name":"max_review_rounds","type":{"kind":"atomic","name":"number"},"required":true,"body":false}`
}

// lisStrField renders a required string-typed input field.
func lisStrField(name string) string {
	return `{"name":"` + name + `","type":{"kind":"atomic","name":"string"},"required":true,"body":false}`
}

// lisRepeatLeaf renders a repeat loop "loop" with a leaf exec body "body" and the given
// cond — the shape the lowering pins assert (bodyBareID "body", ns loop).
func lisRepeatLeaf(cond string) string {
	return `{"kind":"repeat","id":"loop","name":"loop","after":[],` +
		`"iterationName":"iteration","cond":` + cond + `,` +
		`"body":{"kind":"exec","id":"body","name":"body","after":[],` +
		`"interpreter":{"program":{"kind":"shell"}},"body":{"raw":"echo hi"},` +
		`"exitMap":{"pass":[0],"retryable":[]}}}`
}

// lisRepeatRunBody renders a repeat loop whose body is the `run round -> inner given
// {name<-who}` transparent call, with the given cond — the marquee run-body-in-sub
// shape (body id fixed at "round", the id the pins assert).
func lisRepeatRunBody(loopID, cond string) string {
	body := `{"kind":"run","id":"round","name":"round","after":[],` +
		`"target":{"kind":"by-name","name":"inner"},` +
		`"environment":{"fields":[{"name":"name","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}}]},` +
		`"outcome":"transparent"}`
	return `{"kind":"repeat","id":"` + loopID + `","name":"` + loopID + `","after":[],` +
		`"iterationName":"iteration","cond":` + cond + `,"body":` + body + `}`
}

// lisMarqueeCond builds `<bodyRef>.outcome == "pass" || iteration >= <numRef>` — the
// §2.1 marquee exit over a bare body ref and a TYPED number sub input.
func lisMarqueeCond(bodyRef, numRef string) string {
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"` + bodyRef + `","field":"outcome"},` +
		`{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"ref","name":"` + numRef + `"}]}]}`
}

// lisWrapperRun renders the main-level `run wrap -> wrapper given {who<-who,
// max_review_rounds<-12}` node that binds the wrapper's string input and a literal number
// budget.
func lisWrapperRun() string {
	return `{"kind":"run","id":"wrap","name":"wrap","after":[],` +
		`"target":{"kind":"by-name","name":"wrapper"},` +
		`"environment":{"fields":[` +
		`{"name":"who","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"max_review_rounds","value":{"kind":"expr","expr":{"kind":"literal","value":12}}}` +
		`]},"outcome":"transparent"}`
}

// TestLowerLeafLoopInSubFormulaLowers pins that a repeat LEAF-body loop INSIDE a run
// sub-formula lowers (the prefix fence is deleted): the loop unit is namespaced under
// the run and carries the bare body id for its cond/freeze bare-name comparisons.
func TestLowerLeafLoopInSubFormulaLowers(t *testing.T) {
	wrapper := lisSubFormula("wrapper", lisStrField("who")+","+lisMaxRoundsField(),
		lisRepeatLeaf(lisMarqueeCond("body", "max_review_rounds")))
	doc := decodeBundle(t, runMainDoc(lisWrapperRun(), wrapper))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a leaf loop inside a sub-formula: %v", err)
	}
	loop := unitByNode(units, "wrap/loop")
	if loop == nil || loop.kind != unitLoop {
		t.Fatalf("wrap/loop = %+v, want a unitLoop namespaced under the run", loop)
	}
	if loop.ns != "wrap/" {
		t.Errorf("loop ns = %q, want wrap/", loop.ns)
	}
	if loop.loop.bodyNodeID != "wrap/body" {
		t.Errorf("bodyNodeID = %q, want the QUALIFIED wrap/body", loop.loop.bodyNodeID)
	}
	if loop.loop.bodyBareID != "body" {
		t.Errorf("bodyBareID = %q, want the BARE body (⚑B2)", loop.loop.bodyBareID)
	}
}

// TestLowerRunBodyLoopInSubFormulaLowers pins the marquee dry-run: a repeat whose body
// is a `run` INSIDE a wrapper sub-formula lowers — the attempt-0 dry-run mint at the
// depth prefix (wrap/round/0/) succeeds, and the loop is namespaced under the wrapper.
func TestLowerRunBodyLoopInSubFormulaLowers(t *testing.T) {
	inner := lisSubFormula("inner", lisStrField("name"),
		execNode("hello", nil, "echo hi"))
	wrapper := lisSubFormula("wrapper", lisStrField("who")+","+lisMaxRoundsField(),
		lisRepeatRunBody("loop", lisMarqueeCond("round", "max_review_rounds")))
	doc := decodeBundle(t, runMainDoc(lisWrapperRun(), wrapper+","+inner))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a run-body loop inside a sub-formula (marquee dry-run): %v", err)
	}
	loop := unitByNode(units, "wrap/loop")
	if loop == nil || loop.kind != unitLoop || loop.loop.bodyRun == nil {
		t.Fatalf("wrap/loop = %+v, want a run-body unitLoop", loop)
	}
	if loop.loop.bodyBareID != "round" {
		t.Errorf("bodyBareID = %q, want round (bare)", loop.loop.bodyBareID)
	}
	if loop.loop.bodyNodeID != "wrap/round" {
		t.Errorf("bodyNodeID = %q, want wrap/round (qualified)", loop.loop.bodyNodeID)
	}
}

// lisWrapperRunNoEnv renders the main-level `run wrap -> wrapper` node with an empty
// environment (for wrapper formulas that declare no inputs).
func lisWrapperRunNoEnv() string {
	return `{"kind":"run","id":"wrap","name":"wrap","after":[],` +
		`"target":{"kind":"by-name","name":"wrapper"},` +
		`"environment":{"fields":[]},"outcome":"transparent"}`
}

// TestLowerLoopCondRefCharsetBan pins the lowerGuard-parity charset sweep: a loop
// cond ref carrying '/' or ':' (a forged cross-namespace flat key) refuses at ALL
// levels — root and inside a sub-formula.
func TestLowerLoopCondRefCharsetBan(t *testing.T) {
	forged := `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"foo/bar"},{"kind":"literal","value":"x"}]}`
	// Root.
	doc := decodeBundle(t, plainDoc(lisRepeatLeaf(forged)))
	if _, err := buildUnits(doc, true, true); err == nil || !strings.Contains(err.Error(), "reserved delimiter") {
		t.Fatalf("root loop cond '/'-ref = %v, want a reserved-delimiter refusal", err)
	}
	// Inside a sub-formula.
	wrapper := lisSubFormula("wrapper", "", lisRepeatLeaf(forged))
	sub := decodeBundle(t, runMainDoc(lisWrapperRunNoEnv(), wrapper))
	if _, err := buildUnits(sub, true, true); err == nil || !strings.Contains(err.Error(), "reserved delimiter") {
		t.Fatalf("ns loop cond '/'-ref = %v, want a reserved-delimiter refusal", err)
	}
}

// TestLowerLoopCondRefSynthBodyBanned pins the SEPARATE ban-only synth sweep: a loop
// cond ref naming a sibling guard's synthesized `then` body refuses (the same
// inline/pool decision-divergence hole guard cond refs get), while the loop's OWN bare
// body id and its iteration counter are EXEMPT (both resolve via loopScope arms).
func TestLowerLoopCondRefSynthBodyBanned(t *testing.T) {
	// A guard whose then id is `gthen`, and a repeat loop whose cond reads `gthen`.
	guard := guardNode("g", nil, condRefEq("who", "go"), execNode("gthen", nil, "echo t"))
	condReadsSynth := `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"gthen","field":"outcome"},{"kind":"literal","value":"pass"}]}`
	doc := decodeBundle(t, plainDoc(guard+","+lisRepeatLeaf(condReadsSynth)))
	if _, err := buildUnits(doc, true, true); err == nil || !strings.Contains(err.Error(), "synthesized decision body") {
		t.Fatalf("loop cond ref to a synth then = %v, want a synth-body refusal", err)
	}

	// EXEMPT: a loop cond reading its OWN bare body id and iteration lowers even though
	// the body id is registered in synthBodies (the loop's own attempt activation).
	okCond := lisMarqueeCond("body", "iteration") // reads body.outcome + iteration
	okDoc := decodeBundle(t, plainDoc(lisRepeatLeaf(okCond)))
	if _, err := buildUnits(okDoc, true, true); err != nil {
		t.Fatalf("loop cond reading its own body id + iteration refused (exemption missing): %v", err)
	}
}

// TestLowerLoopCondRefNoNewFoldEdges is the MUTATION PIN (§1.1.3): a leaf loop whose
// cond references a real sibling node gains NO fold edge to it — the ban-only sweep
// appends ZERO gates (gating leaf cond refs would be a root behavior change). Pinned
// root AND inside a sub-formula.
func TestLowerLoopCondRefNoNewFoldEdges(t *testing.T) {
	cond := `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"sib","field":"outcome"},{"kind":"literal","value":"pass"}]}`
	// Root: `sib` is a real sibling exec; the loop must NOT gate on it.
	root := decodeBundle(t, plainDoc(execNode("sib", nil, "echo s")+","+lisRepeatLeaf(cond)))
	rootUnits, err := buildUnits(root, true, true)
	if err != nil {
		t.Fatalf("root buildUnits: %v", err)
	}
	if loop := unitByNode(rootUnits, "loop"); loop == nil || len(loop.afterDeps) != 0 {
		t.Fatalf("root loop afterDeps = %v, want none (cond refs never gate a leaf loop)", deref(loop).afterDeps)
	}
	// Inside a sub-formula: same invariant one namespace deeper.
	wrapper := lisSubFormula("wrapper", "",
		execNode("sib", nil, "echo s")+","+lisRepeatLeaf(cond))
	nsDoc := decodeBundle(t, runMainDoc(lisWrapperRunNoEnv(), wrapper))
	nsUnits, err := buildUnits(nsDoc, true, true)
	if err != nil {
		t.Fatalf("ns buildUnits: %v", err)
	}
	loop := unitByNode(nsUnits, "wrap/loop")
	if loop == nil {
		t.Fatal("no wrap/loop unit")
	}
	for _, dep := range loop.afterDeps {
		if strings.Contains(dep, "sib") {
			t.Errorf("ns loop gained a fold edge to sib (%v); cond refs must add ZERO gates", loop.afterDeps)
		}
	}
}

// TestLowerRunBodyLoopFreezeUsesSubInputs pins the Q-D inputNames threading: inside a
// wrapper sub-formula, a run-body loop's re-decide freeze consults the WRAPPER's
// declared inputs — a cond reading a wrapper sub input lowers; one reading a MAIN
// input that the wrapper does not declare refuses (a non-write-once external ref).
func TestLowerRunBodyLoopFreezeUsesSubInputs(t *testing.T) {
	inner := lisSubFormula("inner", lisStrField("name"), execNode("hello", nil, "echo hi"))
	// Legal: cond reads `max_review_rounds`, a declared wrapper sub input.
	okWrap := lisSubFormula("wrapper", lisStrField("who")+","+lisMaxRoundsField(),
		lisRepeatRunBody("loop", lisMarqueeCond("round", "max_review_rounds")))
	okDoc := decodeBundle(t, runMainDoc(lisWrapperRun(), okWrap+","+inner))
	if _, err := buildUnits(okDoc, true, true); err != nil {
		t.Fatalf("run-body loop cond reading a wrapper sub input refused: %v", err)
	}
	// Illegal: cond reads `who` (a wrapper input — fine) but ALSO a bare `secret` that
	// the wrapper never declares → a run-body cond external ref → refuse.
	badCond := lisMarqueeCond("round", "secret")
	badWrap := lisSubFormula("wrapper", lisStrField("who")+","+lisMaxRoundsField(),
		lisRepeatRunBody("loop", badCond))
	badDoc := decodeBundle(t, runMainDoc(lisWrapperRun(), badWrap+","+inner))
	if _, err := buildUnits(badDoc, true, true); err == nil || !strings.Contains(err.Error(), "run-body cond reads") {
		t.Fatalf("run-body loop cond reading an undeclared external ref = %v, want a freeze refusal", err)
	}
}

// TestLowerRunBodyLoopEnvSelfRefBanAtDepth pins ⚑B2 (ii+iii): the ⚑S5 env self-ref
// ban (an env binding that reads the body's own bare id or the iteration counter) still
// FIRES inside a sub-formula — the comparison is against the BARE body id, so it does
// not silently stop firing at depth (which would reopen the attempt-varying corruption).
func TestLowerRunBodyLoopEnvSelfRefBanAtDepth(t *testing.T) {
	inner := lisSubFormula("inner", lisStrField("name"), execNode("hello", nil, "echo hi"))
	// Env binds name <- round (the body's own bare id) — refuse at depth.
	selfRefBody := `{"kind":"run","id":"round","name":"round","after":[],` +
		`"target":{"kind":"by-name","name":"inner"},` +
		`"environment":{"fields":[{"name":"name","value":{"kind":"expr","expr":{"kind":"ref","name":"round"}}}]},` +
		`"outcome":"transparent"}`
	loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],"iterationName":"iteration",` +
		`"cond":` + lisMarqueeCond("round", "max_review_rounds") + `,"body":` + selfRefBody + `}`
	wrapper := lisSubFormula("wrapper", lisStrField("who")+","+lisMaxRoundsField(), loop)
	doc := decodeBundle(t, runMainDoc(lisWrapperRun(), wrapper+","+inner))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "its own body id") {
		t.Fatalf("env self-ref to the body id at depth = %v, want the ⚑S5 ban to fire", err)
	}
	// The message prints the BARE authored id (⚑B2 iii) — never the qualified one.
	if !strings.Contains(err.Error(), `"round"`) || strings.Contains(err.Error(), "wrap/round") {
		t.Errorf("ban message = %q, want the bare body id %q (not the qualified wrap/round)", err.Error(), "round")
	}
}

// TestLowerRunBodyLoopEnvIterationBanAtDepth pins the OTHER ⚑S5 direction at depth: an
// env binding that reads the repeat's iteration counter refuses INSIDE a sub-formula
// (the path-dependent render corruption is namespace-invariant).
func TestLowerRunBodyLoopEnvIterationBanAtDepth(t *testing.T) {
	inner := lisSubFormula("inner", lisStrField("name"), execNode("hello", nil, "echo hi"))
	iterRefBody := `{"kind":"run","id":"round","name":"round","after":[],` +
		`"target":{"kind":"by-name","name":"inner"},` +
		`"environment":{"fields":[{"name":"name","value":{"kind":"expr","expr":{"kind":"ref","name":"iteration"}}}]},` +
		`"outcome":"transparent"}`
	loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],"iterationName":"iteration",` +
		`"cond":` + lisMarqueeCond("round", "max_review_rounds") + `,"body":` + iterRefBody + `}`
	wrapper := lisSubFormula("wrapper", lisStrField("who")+","+lisMaxRoundsField(), loop)
	doc := decodeBundle(t, runMainDoc(lisWrapperRun(), wrapper+","+inner))
	if _, err := buildUnits(doc, true, true); err == nil || !strings.Contains(err.Error(), "iteration counter") {
		t.Fatalf("env iteration-ref at depth = %v, want the ⚑S5 iteration ban to fire", err)
	}
}

// TestLowerRepeatRunBodyMintedGraphInputNames pins §1.1.4's MINTED-GRAPH leg (the
// mintRunBody inputNames mutant killer): buildUnits ACCEPTS an OUTER repeat-run-body
// whose body formula (mid) itself contains the marquee run-body loop reading MID's
// declared sub input. The outer dry-run mint lowers mid through mintRunBody's fresh
// lowerer, whose inputNames MUST be mid's declared fields — with the mutant
// (inputNames: nil) the inner freeze falsely refuses `max_review_rounds` and this fails.
func TestLowerRepeatRunBodyMintedGraphInputNames(t *testing.T) {
	innerLoop := lisRepeatRunBody("rounds",
		lisMarqueeCond("round", "max_review_rounds"))
	mid := lisSubFormula("mid", lisStrField("who")+","+lisMaxRoundsField(), innerLoop)
	inner := lisSubFormula("inner", lisStrField("name"), execNode("hello", nil, "echo hi"))
	outerBody := `{"kind":"run","id":"stage","name":"stage","after":[],` +
		`"target":{"kind":"by-name","name":"mid"},` +
		`"environment":{"fields":[` +
		`{"name":"who","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}},` +
		`{"name":"max_review_rounds","value":{"kind":"expr","expr":{"kind":"literal","value":12}}}]},` +
		`"outcome":"transparent"}`
	outerCond := `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"stage","field":"outcome"},{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":2}]}]}`
	outer := `{"kind":"repeat","id":"outer","name":"outer","after":[],"iterationName":"iteration",` +
		`"cond":` + outerCond + `,"body":` + outerBody + `}`
	doc := decodeBundle(t, runMainDoc(outer, mid+","+inner))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused the minted-graph marquee (mintRunBody inputNames missing?): %v", err)
	}
	loop := unitByNode(units, "outer")
	if loop == nil || loop.kind != unitLoop || loop.loop.bodyRun == nil {
		t.Fatalf("outer = %+v, want a run-body unitLoop whose dry-run validated the nested marquee", loop)
	}
}

// TestLowerLoopCondRefSynthBodyBannedInSub is the ns copy of the synth-ban pin (the
// synthBodies keying mutant killer): INSIDE a run sub-formula, a loop cond naming a
// sibling guard's synthesized then refuses. The registry keys are QUALIFIED
// (wrap/gthen) while the ref is bare — dropping the u.ns qualification in the sweep's
// lookup silently un-bans every depth placement.
func TestLowerLoopCondRefSynthBodyBannedInSub(t *testing.T) {
	guard := guardNode("g", nil, condRefEq("who", "go"), execNode("gthen", nil, "echo t"))
	condReadsSynth := `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"gthen","field":"outcome"},{"kind":"literal","value":"pass"}]}`
	wrapper := lisSubFormula("wrapper", "", guard+","+lisRepeatLeaf(condReadsSynth))
	doc := decodeBundle(t, runMainDoc(lisWrapperRunNoEnv(), wrapper))
	if _, err := buildUnits(doc, true, true); err == nil || !strings.Contains(err.Error(), "synthesized decision body") {
		t.Fatalf("ns loop cond ref to a sibling synth then = %v, want a synth-body refusal (ns-keyed lookup)", err)
	}
}

// TestLowerLoopCondIterationNameExemptFromSynthBan pins the iterationName EXEMPTION (the
// exemption-deletion mutant killer): a SIBLING guard whose synthesized then is literally
// NAMED "iteration", beside a repeat loop whose cond refs `iteration`, must LOWER — the
// iteration ref resolves via loopScope's iterationName arm (never the children view), so
// the synth ban must not false-refuse it.
func TestLowerLoopCondIterationNameExemptFromSynthBan(t *testing.T) {
	guard := guardNode("g", nil, condRefEq("who", "go"), execNode("iteration", nil, "echo t"))
	cond := `{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"literal","value":5}]}`
	doc := decodeBundle(t, plainDoc(guard+","+lisRepeatLeaf(cond)))
	if _, err := buildUnits(doc, true, true); err != nil {
		t.Fatalf("loop cond reading `iteration` beside a synth body named \"iteration\" refused (exemption missing): %v", err)
	}
}

// TestLowerRepeatIterationNameCharsetBan pins the binder-parity charset ban: a
// hand-crafted '/'- or ':'-bearing iterationName refuses at lowering (the qualified
// seed u.ns+iterationName would land in the wrong namespace, silently rendering "" at
// depth while appearing to work at root).
func TestLowerRepeatIterationNameCharsetBan(t *testing.T) {
	for _, bad := range []string{"it/er", "it:er"} {
		loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],` +
			`"iterationName":` + jsonStr(bad) + `,` +
			`"cond":{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":5}]},` +
			`"body":{"kind":"exec","id":"body","name":"body","after":[],` +
			`"interpreter":{"program":{"kind":"shell"}},"body":{"raw":"echo hi"},` +
			`"exitMap":{"pass":[0],"retryable":[]}}}`
		doc := decodeBundle(t, plainDoc(loop))
		_, err := buildUnits(doc, true, true)
		if err == nil || !strings.Contains(err.Error(), "iterationName") || !strings.Contains(err.Error(), "reserved delimiter") {
			t.Fatalf("iterationName %q = %v, want an iterationName reserved-delimiter refusal", bad, err)
		}
	}
}

// TestRepeatRunInSubFixtureLowers guards the hand-authored repeat-run-in-sub dolt-e2e bundle
// fixture: the §2.1 marquee (a repeat run-body loop inside the reviewWrapper sub-formula,
// budget 12 bound via env) decodes and lowers under BOTH the inline and controller-loop pool
// flag pairs — so a fixture typo fails fast HERE, not 350s into the e2e — and the attempt-0
// dry-run mint lands at wrapper/round/0/review.
func TestRepeatRunInSubFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "repeat-run-in-sub.lumen.json")
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
			t.Fatalf("buildUnits(allowCombineDo=%v) refused the marquee fixture: %v", combineDo, err)
		}
		loop := unitByNode(units, "wrapper/rounds")
		if loop == nil || loop.kind != unitLoop || loop.loop.bodyRun == nil {
			t.Fatalf("wrapper/rounds = %+v, want a run-body unitLoop", loop)
		}
		if loop.loop.bodyBareID != "round" || loop.loop.bodyNodeID != "wrapper/round" {
			t.Errorf("loop body ids = bare %q qualified %q, want round / wrapper/round", loop.loop.bodyBareID, loop.loop.bodyNodeID)
		}
	}
}
