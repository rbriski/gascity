package engine

import (
	"encoding/json"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// --- timeout (TNK) lowering fixtures ----------------------------------------

// litDurationJSON renders a closed-expr literal duration value `{kind:"literal",value:<v>}`.
func litDurationJSON(v string) string {
	return `{"kind":"literal","value":` + jsonStr(v) + `}`
}

// timeoutNodeIR renders a timeout node (advisory check-with-budget wrapper) with an explicit
// duration expr and a single-leaf body node JSON.
func timeoutNodeIR(id string, after []string, durationJSON, bodyJSON string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"timeout","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"duration":` + durationJSON + `,"body":` + bodyJSON + `}`
}

// timeoutDoBody renders a `do` node usable as a timeout body (an authored leaf, so it bypasses
// lowerNode's id check like a guard then).
func timeoutDoBody(id, prompt string) string {
	return `{"kind":"do","id":"` + id + `","name":"` + id + `","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"source":{"kind":"prompt"},` +
		`"interpreter":{"kind":"agent","mode":{"kind":"do"},"origin":{"uri":"t","line":1,"col":0}},` +
		`"body":{"raw":` + jsonStr(prompt) + `,"source":{"kind":"inline"},"language":"markdown","origin":{"uri":"t","line":1,"col":0}}}`
}

// TestLowerTimeoutLowers pins the positive shape: a timeout lowers to a unitTimeout carrying
// the raw duration literal VERBATIM and the single-leaf body spec, with NO separate body unit
// (the body is synthesized at run time, exactly like a guard then).
func TestLowerTimeoutLowers(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		execNode("prep", nil, "echo p")+","+
			timeoutNodeIR("check", []string{"prep"}, litDurationJSON("5m"),
				execNode("v", nil, "echo checked"))+","+
			execNode("done", []string{"check"}, "echo d"),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	tu := unitByNode(units, "check")
	if tu == nil || tu.kind != unitTimeout || tu.timeout == nil {
		t.Fatalf("check = %+v, want a unitTimeout", tu)
	}
	if tu.timeout.duration != "5m" {
		t.Errorf("duration = %q, want the raw literal %q (rides VERBATIM, never parsed)", tu.timeout.duration, "5m")
	}
	if tu.timeout.bodyNodeID != "v" || tu.timeout.bodyIRKind != ir.NodeExec {
		t.Errorf("body spec = {%q,%q}, want {v,exec}", tu.timeout.bodyNodeID, tu.timeout.bodyIRKind)
	}
	if tu.irKind != ir.NodeTimeout {
		t.Errorf("irKind = %q, want timeout", tu.irKind)
	}
	// The wrapper's authored `after` gate RIDES the unit (rawAfter) and resolves to a blocking
	// dep (afterDeps) — dropping it would silently unfence the wrapper from its upstream (no
	// skip-cascade; the body would run despite a failed dep).
	if len(tu.rawAfter) != 1 || tu.rawAfter[0] != "prep" {
		t.Errorf("rawAfter = %v, want [prep] (the authored gate must ride the unit)", tu.rawAfter)
	}
	if len(tu.afterDeps) != 1 || tu.afterDeps[0] != "prep:0" {
		t.Errorf("afterDeps = %v, want [prep:0] (the resolved blocking gate)", tu.afterDeps)
	}
	// The body is NOT a separate plan unit (synthesized at run time).
	if bu := unitByNode(units, "v"); bu != nil {
		t.Errorf("body v is a separate unit %+v, want it synthesized (not lowered)", bu)
	}
}

// TestLowerTimeoutDurationRidesVerbatim proves the raw literal string rides UNPARSED: an absurd
// but well-shaped duration ("999999h") lowers without overflow — no time.ParseDuration on the
// new path (§2.4 no-clock).
func TestLowerTimeoutDurationRidesVerbatim(t *testing.T) {
	for _, d := range []string{"0m", "500ms", "30s", "2h", "999999h"} {
		doc := decodeBundle(t, plainDoc(
			timeoutNodeIR("check", nil, litDurationJSON(d), execNode("v", nil, "echo ok"))))
		units, err := buildUnits(doc, true, true)
		if err != nil {
			t.Fatalf("duration %q: buildUnits: %v", d, err)
		}
		if got := unitByNode(units, "check").timeout.duration; got != d {
			t.Errorf("duration %q rode as %q, want verbatim", d, got)
		}
	}
}

// TestLowerTimeoutAdmitsDoBody proves a do body lowers (budget is advisory for ALL body kinds
// — guard-parity, ruled).
func TestLowerTimeoutAdmitsDoBody(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		timeoutNodeIR("check", nil, litDurationJSON("5m"), timeoutDoBody("v", "do the check"))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	tu := unitByNode(units, "check")
	if tu == nil || tu.timeout == nil || tu.timeout.bodyIRKind != ir.NodeDo {
		t.Fatalf("check = %+v, want a do-bodied unitTimeout", tu)
	}
}

// TestLowerTimeoutRefusesMissingDuration pins the absent-duration refusal.
func TestLowerTimeoutRefusesMissingDuration(t *testing.T) {
	bad := `{"kind":"timeout","id":"check","name":"check","after":[],"body":` +
		execNode("v", nil, "echo ok") + `}`
	_, err := buildUnits(decodeBundle(t, plainDoc(bad)), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "missing duration") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode missing-duration refusal", err)
	}
}

// TestLowerTimeoutRefusesMissingBody pins the absent-body refusal — the FLIPPED
// TestDeferredKindsRefusedCleanly bodyless fixture (§0.2), now a supported kind refusing on
// its missing body.
func TestLowerTimeoutRefusesMissingBody(t *testing.T) {
	bad := `{"kind":"timeout","id":"check","name":"check","after":[],"duration":` +
		litDurationJSON("30s") + `}`
	_, err := buildUnits(decodeBundle(t, plainDoc(bad)), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "missing body") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode missing-body refusal", err)
	}
}

// TestLowerTimeoutRefusesNonLiteralDuration pins that a non-literal duration expr (a ref) is
// refused (no clock — the raw literal must ride verbatim).
func TestLowerTimeoutRefusesNonLiteralDuration(t *testing.T) {
	dur := `{"kind":"ref","name":"budget"}`
	_, err := buildUnits(decodeBundle(t, plainDoc(
		timeoutNodeIR("check", nil, dur, execNode("v", nil, "echo ok")))), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "duration must be a literal") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode non-literal-duration refusal", err)
	}
}

// TestLowerTimeoutRefusesMalformedDuration pins the pure-regex refusal against every malformed
// shape the reference compile-time regex rejects (bad unit, leading zero, negative, empty,
// composite, decimal).
func TestLowerTimeoutRefusesMalformedDuration(t *testing.T) {
	for _, bad := range []string{"5x", "05m", "-5m", "", "1h30m", "1.5m", "m", "5", "5 m"} {
		_, err := buildUnits(decodeBundle(t, plainDoc(
			timeoutNodeIR("check", nil, litDurationJSON(bad), execNode("v", nil, "echo ok")))), true, true)
		if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "duration") {
			t.Errorf("duration %q: err = %v, want an ErrUnsupportedNode malformed-duration refusal", bad, err)
		}
	}
}

// TestLowerTimeoutRefusesNonStringDuration pins that a non-string literal value (a number) is
// refused — the raw duration is a string literal like "5m", never a bare number.
func TestLowerTimeoutRefusesNonStringDuration(t *testing.T) {
	dur := `{"kind":"literal","value":300}`
	_, err := buildUnits(decodeBundle(t, plainDoc(
		timeoutNodeIR("check", nil, dur, execNode("v", nil, "echo ok")))), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "duration") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode malformed-duration refusal", err)
	}
}

// TestLowerTimeoutRefusesNonLeafBody pins that every non-leaf body kind is refused (only a
// single exec/do leaf body lowers) — the FULL non-leaf matrix (red-team P3-4): block, scatter,
// gather, guard, repeat, retry, run, dispatch, cleanup, recover, timeout-in-timeout — plus
// settle (leaf, but not admitted as a timeout body). Every row must classify as
// errors.Is(ErrUnsupportedNode), killing the misclassification mutant (a row sneaking into a
// decode error that is not an ErrUnsupportedNode refusal).
func TestLowerTimeoutRefusesNonLeafBody(t *testing.T) {
	cases := map[string]string{
		"block":    `{"kind":"block","id":"b","after":[],"members":[]}`,
		"scatter":  `{"kind":"scatter","id":"sc","name":"sc","after":[],"form":"members","on_fail":"continue","members":[]}`,
		"gather":   `{"kind":"gather","id":"gt","name":"gt","after":[]}`,
		"guard":    guardNode("g", nil, condRefEq("x", "y"), execNode("gt", nil, "echo t")),
		"repeat":   repeatMemberForgedCond("rl", "rb"),
		"retry":    retryMember("rt", "rtb", "echo r"),
		"run":      runNode("r", nil, "greeter", "name", "who"),
		"dispatch": dispatchNode("dp", nil, "mode", [2]string{"a", "echo a"}),
		"cleanup":  `{"kind":"cleanup","id":"cl","after":[]}`,
		"recover":  `{"kind":"recover","id":"rc","after":[]}`,
		"timeout":  timeoutNodeIR("inner", nil, litDurationJSON("5m"), execNode("iv", nil, "echo i")),
		"settle":   `{"kind":"settle","id":"s","name":"s","after":[],"outcome":"pass"}`,
	}
	for name, body := range cases {
		_, err := buildUnits(decodeBundle(t, plainDoc(
			timeoutNodeIR("check", nil, litDurationJSON("5m"), body))), true, true)
		if !errorsIsUnsupported(err) {
			t.Errorf("body kind %s: err = %v, want errors.Is(ErrUnsupportedNode)", name, err)
		}
	}
}

// TestLowerTimeoutRefusesMissingBodyID pins the empty-body-id refusal at LOWERING (red-team
// P3-3): a body with `"id":""` must refuse loudly at buildUnits — without the check it would
// lower onto activationFor("") and only break at run time in the fold, the wrong layer.
func TestLowerTimeoutRefusesMissingBodyID(t *testing.T) {
	_, err := buildUnits(decodeBundle(t, plainDoc(
		timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("", nil, "echo x")))), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "body missing id") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode body-missing-id refusal", err)
	}
}

// TestLowerTimeoutRefusesBodyIdCharset pins the '/'+':' charset ban on the body id (the
// decodeLeafSub precedent — an authored body bypasses lowerNode's id check).
func TestLowerTimeoutRefusesBodyIdCharset(t *testing.T) {
	for _, id := range []string{"a/b", "a:b"} {
		_, err := buildUnits(decodeBundle(t, plainDoc(
			timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode(id, nil, "echo x")))), true, true)
		if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "must not contain '/' or ':'") {
			t.Errorf("body id %q: err = %v, want an ErrUnsupportedNode charset refusal", id, err)
		}
	}
}

// TestLowerTimeoutRefusesBodyAfterGate pins the LOUD refusal of a non-empty body `after` — a
// timeout body is a single synthesized leaf with no gate slot (decodeLeafSub parity).
func TestLowerTimeoutRefusesBodyAfterGate(t *testing.T) {
	_, err := buildUnits(decodeBundle(t, plainDoc(
		execNode("prep", nil, "echo p")+","+
			timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", []string{"prep"}, "echo x")))), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "must not carry an 'after' gate") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode body-after refusal", err)
	}
}

// TestLowerTimeoutRefusesBodyIdSiblingCollision pins the addSynth (b) arm exists: a body id
// colliding with a sibling node id refuses via the synth registry (the SILENT sibling-id
// aliasing miss made loud).
func TestLowerTimeoutRefusesBodyIdSiblingCollision(t *testing.T) {
	_, err := buildUnits(decodeBundle(t, plainDoc(
		execNode("v", nil, "echo sibling")+","+
			timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", nil, "echo body")))), true, true)
	if err == nil || !strings.Contains(err.Error(), "body id") || !strings.Contains(err.Error(), "collides with node") {
		t.Fatalf("err = %v, want a decision-body collision refusal naming node v (addSynth arm)", err)
	}
}

// --- positioning table (§1.1.4 / §2.8) --------------------------------------

// TestLowerTimeoutAdmitBlockMember pins ADMIT: a timeout inside a block lowers.
func TestLowerTimeoutAdmitBlockMember(t *testing.T) {
	block := `{"kind":"block","id":"b","after":[],"members":[` +
		timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", nil, "echo ok")) + `]}`
	units, err := buildUnits(decodeBundle(t, plainDoc(block)), true, true)
	if err != nil {
		t.Fatalf("block-member timeout: %v", err)
	}
	if tu := unitByNode(units, "check"); tu == nil || tu.kind != unitTimeout {
		t.Fatalf("check = %+v, want a unitTimeout (block member admitted)", tu)
	}
}

// TestLowerTimeoutAdmitSubFormulaTopLevel pins ADMIT: a timeout at a run sub-formula's top level
// (prefix != "") lowers, ns-qualified.
func TestLowerTimeoutAdmitSubFormulaTopLevel(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("stage", nil, "greeter", "name", "who"),
		greeterFormula("greeter",
			timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", nil, "echo {{ name }}"))),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("sub-formula timeout: %v", err)
	}
	tu := unitByNode(units, "stage/check")
	if tu == nil || tu.kind != unitTimeout || tu.ns != "stage/" || tu.timeout.bodyNodeID != "stage/v" {
		t.Fatalf("stage/check = %+v, want a ns-qualified unitTimeout over stage/v", tu)
	}
}

// TestLowerTimeoutAdmitScatterMemberRoot pins ADMIT: a timeout as a scatter member at root
// lowers and joins the scatter's member set.
func TestLowerTimeoutAdmitScatterMemberRoot(t *testing.T) {
	sc := scatterOf("lanes",
		execNode("direct", nil, "echo d"),
		timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", nil, "echo ok")))
	units, err := buildUnits(decodeBundle(t, plainDoc(sc)), true, true)
	if err != nil {
		t.Fatalf("scatter-member timeout (root): %v", err)
	}
	agg := unitByNode(units, "lanes")
	if agg == nil || !containsStr(agg.members, "check:0") {
		t.Fatalf("scatter members = %v, want the timeout member check:0", deref(agg).members)
	}
}

// TestLowerTimeoutAdmitScatterMemberSubFormula pins ADMIT: a timeout as a scatter member INSIDE
// a run sub-formula lowers (member accounting is parent-based, ns-qualified).
func TestLowerTimeoutAdmitScatterMemberSubFormula(t *testing.T) {
	sub := greeterFormula("greeter", scatterOf("lanes",
		execNode("direct", nil, "echo d"),
		timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", nil, "echo ok"))))
	doc := decodeBundle(t, runMainDoc(runNode("stage", nil, "greeter", "name", "who"), sub))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("scatter-member timeout (sub-formula): %v", err)
	}
	tu := unitByNode(units, "stage/check")
	if tu == nil || tu.kind != unitTimeout || tu.ns != "stage/" {
		t.Fatalf("stage/check = %+v, want a ns-qualified scatter-member unitTimeout", tu)
	}
	agg := unitByNode(units, "stage/lanes")
	if agg == nil || !containsStr(agg.members, "stage/check:0") {
		t.Fatalf("sub scatter members = %v, want stage/check:0", deref(agg).members)
	}
}

// TestLowerTimeoutRefusedInGatherCombine pins REFUSE: a timeout as a gather-combine member
// falls out of the leaf-only sweep (a non-leaf unit is not executable in a combine block).
func TestLowerTimeoutRefusedInGatherCombine(t *testing.T) {
	sc := scatterOf("lanes", execNode("m", nil, "echo m"))
	gather := gatherOverCombine("g", "lanes",
		timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", nil, "echo ok")))
	_, err := buildUnits(decodeBundle(t, plainDoc(sc+","+gather)), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "combine") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode gather-combine refusal (leaf-only sweep)", err)
	}
}

// TestLowerTimeoutRefusedAsGuardThen pins REFUSE: a timeout as a guard `then` is refused by the
// guard then switch (the representative decision-position refusal).
func TestLowerTimeoutRefusedAsGuardThen(t *testing.T) {
	body := timeoutNodeIR("check", nil, litDurationJSON("5m"), execNode("v", nil, "echo ok"))
	_, err := buildUnits(decodeBundle(t, plainDoc(
		guardNode("g", nil, condRefEq("x", "y"), body))), true, true)
	if !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "then kind") {
		t.Fatalf("err = %v, want an ErrUnsupportedNode guard-then refusal", err)
	}
}

// TestTimeoutPathReadsNoClock pins §2.4's no-clock discipline at SOURCE granularity (the
// package legitimately imports time elsewhere, so an import scan cannot pin this): the four
// TNK functions — lowerTimeout, runTimeout, timeoutBodyUnit, advanceTimeout — must contain no
// `time.` selector and no ParseDuration reference. The raw duration literal is validated by a
// pure regex and rides VERBATIM; parsing it (or reading a clock) anywhere on the new path
// would break the byte-identical resume re-emission under the :act idem token.
func TestTimeoutPathReadsNoClock(t *testing.T) {
	files := map[string][]string{
		"plan.go":    {"lowerTimeout"},
		"engine.go":  {"runTimeout", "timeoutBodyUnit"},
		"advance.go": {"advanceTimeout"},
	}
	for file, names := range files {
		src, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("read %s: %v", file, err)
		}
		fset := token.NewFileSet()
		parsed, err := parser.ParseFile(fset, file, src, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", file, err)
		}
		found := map[string]bool{}
		for _, decl := range parsed.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || !containsStr(names, fn.Name.Name) {
				continue
			}
			found[fn.Name.Name] = true
			// fn.Pos() is the `func` keyword (the doc comment is excluded), so the span is
			// the signature + body, including any inline comments.
			body := string(src[fset.Position(fn.Pos()).Offset:fset.Position(fn.End()).Offset])
			for _, banned := range []string{"time.", "ParseDuration"} {
				if strings.Contains(body, banned) {
					t.Errorf("%s: %s contains %q — the TNK path must read no clock and never parse the duration", file, fn.Name.Name, banned)
				}
			}
		}
		for _, name := range names {
			if !found[name] {
				t.Errorf("%s: function %s not found — the no-clock scan lost its target (renamed?)", file, name)
			}
		}
	}
}

// TestLowerTimeoutComposedProvenanceRefusal pins §2.5's COMPOSED mint-wrap chain: a malformed
// timeout (missing duration) at the BOTTOM of the corpus's deepest route — dispatch arm (run)
// → for-each run body → repeat run body → timeout — refuses at buildUnits, wrapped in every
// leg's provenance (all legs share mintRunBody, so the assertion is errors.Is + chain
// substrings, NOT an exact single-wrap string).
func TestLowerTimeoutComposedProvenanceRefusal(t *testing.T) {
	// checkFormula's only node is a MALFORMED timeout (no duration).
	badTimeout := `{"kind":"timeout","id":"check","name":"check","after":[],"body":` +
		execNode("v", nil, "echo ok") + `}`
	checkFormula := `"checkFormula":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"checkFormula","input":{"name":"checkFormula.input","fields":[` +
		`{"name":"seed","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + badTimeout + `]}`
	// loopFormula: a repeat whose run body targets checkFormula (RBL leg).
	loopFormula := `"loopFormula":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"loopFormula","input":{"name":"loopFormula.input","fields":[` +
		`{"name":"seed","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + repeatRunNode(runNode("stage", nil, "checkFormula", "seed", "seed"), repeatRunCondPassOrIter()) + `]}`
	// fanFormula: a for-each run body targeting loopFormula (FBR leg).
	fanFormula := `"fanFormula":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"fanFormula","input":{"name":"fanFormula.input","fields":[` +
		`{"name":"items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}]},` +
		`"nodes":[` + fbrFanNode(refV("items"), fbrRunMember("loopFormula", envF("seed", "reviewer"))) + `]}`
	// Main: a dispatch whose "go" arm run body targets fanFormula (DAR leg, the outermost frame).
	main := `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[` +
		`{"name":"policy","type":{"kind":"atomic","name":"string"},"required":true,"body":false},` +
		`{"name":"items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}]},` +
		`"nodes":[` + darDispatch("policy", darRunArm("go", "fan", "fanFormula", envF("items", "items"))) + `],` +
		`"formulas":{` + fanFormula + `,` + loopFormula + `,` + checkFormula + `}}`

	_, err := buildUnits(decodeBundle(t, main), true, true)
	if !errorsIsUnsupported(err) {
		t.Fatalf("err = %v, want ErrUnsupportedNode (the composed mint-wrap chain)", err)
	}
	msg := err.Error()
	for _, want := range []string{"dispatch", "for-each", "repeat", "timeout", "missing duration", "does not lower"} {
		if !strings.Contains(msg, want) {
			t.Errorf("composed refusal %q missing chain substring %q", msg, want)
		}
	}
}

// TestTimeoutCheckFixtureLowers guards the hand-authored TNK dolt-e2e bundle fixture
// (repeat-driven: repeat { run stage -> draft do + check timeout-exec + report gate } until
// stage.outcome == pass): it decodes and lowers under BOTH pool flag pairs, so a fixture typo
// (incl. a malformed timeout in the DRY-RUN mint of the run body) fails fast HERE, not 300s
// into the e2e. The repeat lowers to ONE top-level loop unit whose run body is dry-run minted
// — the timeout's shape is validated in that mint, so a bad duration/body refuses here.
func TestTimeoutCheckFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "timeout-check.lumen.json")
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
			t.Fatalf("buildUnits(allowCombineDo=%v) refused the timeout-check fixture: %v", combineDo, err)
		}
		loop := unitByNode(units, "loop")
		if loop == nil || loop.kind != unitLoop || loop.loop == nil || loop.loop.bodyIRKind != ir.NodeRun {
			t.Fatalf("loop = %+v, want a repeat unitLoop with a run body (dry-run mint validated the deep timeout)", loop)
		}
	}
}
