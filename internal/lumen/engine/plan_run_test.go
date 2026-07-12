package engine

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// TestRunDoFixtureLowers guards the hand-authored dolt-e2e bundle fixture: it
// decodes and lowers (allowDo) so a fixture typo fails fast here, not 300s into
// the integration e2e. The sub-do inlines as greeting/hello.
func TestRunDoFixtureLowers(t *testing.T) {
	for _, name := range []string{"run-do.lumen.json", "run-do-chain.lumen.json", "run-greeter.lumen.json"} {
		path := filepath.Join("..", "..", "..", "examples", "lumen", name)
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read fixture %s: %v", name, err)
		}
		doc, err := ir.Decode(data)
		if err != nil {
			t.Fatalf("decode fixture %s: %v", name, err)
		}
		units, err := buildUnits(doc, true, true)
		if err != nil {
			t.Fatalf("lower fixture %s: %v", name, err)
		}
		if unitByNode(units, "greeting/hello") == nil {
			t.Errorf("fixture %s: no greeting/hello unit; got %v", name, nodeIDs(units))
		}
	}
}

// TestScatterRetryDoFixtureLowers guards the RN dolt-e2e fixture: a scatter of two
// retry-do lanes lowers (two loop members under the scatter aggregate).
func TestScatterRetryDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "scatter-retry-do.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("lower scatter-retry-do: %v", err)
	}
	agg := unitByNode(units, "lanes")
	if agg == nil || agg.kind != unitScatterAgg {
		t.Fatalf("no scatter aggregate; got %v", nodeIDs(units))
	}
	if !containsStr(agg.members, "r1:0") || !containsStr(agg.members, "r2:0") {
		t.Errorf("scatter members = %v, want the two retry loops", agg.members)
	}
}

// TestGuardDoFixtureLowers guards the guard dolt-e2e fixture.
func TestGuardDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "guard-do.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("lower guard-do: %v", err)
	}
	g := unitByNode(units, "g")
	if g == nil || g.kind != unitGuard || g.guard == nil || g.guard.thenNodeID != "gthen" {
		t.Fatalf("guard unit = %+v (spec %+v), want a unitGuard over gthen", g, deref(g).guard)
	}
}

// TestDispatchDoFixtureLowers guards the dispatch dolt-e2e fixture.
func TestDispatchDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "dispatch-do.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("lower dispatch-do: %v", err)
	}
	d := unitByNode(units, "d")
	if d == nil || d.kind != unitDispatch || d.dispatch == nil || len(d.dispatch.arms) != 2 {
		t.Fatalf("dispatch unit = %+v, want a unitDispatch with 2 arms", d)
	}
}

// TestForEachDoFixtureLowers guards the for-each dolt-e2e fixture: a scatter(form:each)
// over the input array `items` with a single do body lowers to one unitForEach.
func TestForEachDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "for-each-do.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("lower for-each-do: %v", err)
	}
	fan := unitByNode(units, "fan")
	if fan == nil || fan.kind != unitForEach || fan.forEach == nil {
		t.Fatalf("fan unit = %+v, want a unitForEach", fan)
	}
	if fan.forEach.binder != "item" || fan.forEach.bodyIRKind != ir.NodeDo {
		t.Fatalf("for-each spec = %+v, want binder=item bodyIRKind=do", fan.forEach)
	}
}

// TestCleanupDoFixtureLowers guards the cleanup dolt-e2e fixture: a cleanup with a do
// guarded + a do finally body lowers to one unitCleanup.
func TestCleanupDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "cleanup-do.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("lower cleanup-do: %v", err)
	}
	clean := unitByNode(units, "clean")
	if clean == nil || clean.kind != unitCleanup || clean.cleanup == nil {
		t.Fatalf("clean unit = %+v, want a unitCleanup", clean)
	}
	if clean.cleanup.guardedNodeID != "work" || clean.cleanup.bodyNodeID != "unlock" {
		t.Fatalf("cleanup spec = %+v, want guarded=work body=unlock", clean.cleanup)
	}
}

// TestRecoverDoFixtureLowers guards the recover dolt-e2e fixture: a recover with a
// settle guarded + a do catch body lowers to one unitRecover.
func TestRecoverDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "recover-do.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("lower recover-do: %v", err)
	}
	rec := unitByNode(units, "rec")
	if rec == nil || rec.kind != unitRecover || rec.recover == nil {
		t.Fatalf("rec unit = %+v, want a unitRecover", rec)
	}
	if rec.recover.guardedNodeID != "charge" || rec.recover.bodyNodeID != "refund" || rec.recover.errorBinding != "error" {
		t.Fatalf("recover spec = %+v, want guarded=charge body=refund errorBinding=error", rec.recover)
	}
}

// decodeBundle builds an *ir.IR from a JSON literal, failing the test on a
// decode/validate error. It is the R1a lowering fixtures' front door.
func decodeBundle(t *testing.T, doc string) *ir.IR {
	t.Helper()
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	return d
}

// unitByNode finds the lowered unit for a (qualified) node id.
func unitByNode(units []planUnit, nodeID string) *planUnit {
	for i := range units {
		if units[i].nodeID == nodeID {
			return &units[i]
		}
	}
	return nil
}

// runMainDoc wraps a main-formula node list + a formulas bundle into a full IR doc.
func runMainDoc(mainNodes, formulas string) string {
	return `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[{"name":"who","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + mainNodes + `],` +
		`"formulas":{` + formulas + `}}`
}

func execNode(id string, after []string, script string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"exec","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"interpreter":{"program":{"kind":"shell"}},"body":{"raw":` + jsonStr(script) + `}}`
}

func jsonStr(s string) string { b, _ := json.Marshal(s); return string(b) }

// runNode emits a run node targeting sub, binding a single env field (name<-value ref).
func runNode(id string, after []string, target, envField, envRef string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"run","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"target":{"kind":"by-name","name":"` + target + `"},` +
		`"environment":{"fields":[{"name":"` + envField + `","value":{"kind":"expr","expr":{"kind":"ref","name":"` + envRef + `"}}}]},` +
		`"outcome":"transparent"}`
}

func greeterFormula(name string, nodes string) string {
	return `"` + name + `":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"` + name + `","input":{"name":"` + name + `.input","fields":[{"name":"name","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + nodes + `]}`
}

// guardNode renders a guard node: cond is a closed expr, then is a single leaf.
func guardNode(id string, after []string, cond, then string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"guard","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"cond":` + cond + `,"then":` + then + `}`
}

// condRefEq builds a closed cond `<ref> == <literal>` over an input/node ref.
func condRefEq(ref, lit string) string {
	return `{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"` + ref + `"},` +
		`{"kind":"literal","value":` + jsonStr(lit) + `}]}`
}

// TestLowerGuardLowers (guard) pins that a guard lowers to a unitGuard carrying its
// cond + then, with NO separate then unit (the then is synthesized at run time).
func TestLowerGuardLowers(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		execNode("prep", nil, "echo p")+","+
			guardNode("g", []string{"prep"}, condRefEq("mode", "go"), execNode("gthen", nil, "echo ran"))+","+
			execNode("done", []string{"g"}, "echo d"),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	g := unitByNode(units, "g")
	if g == nil || g.kind != unitGuard {
		t.Fatalf("g = %+v, want a unitGuard", g)
	}
	if g.guard == nil || g.guard.thenNodeID != "gthen" {
		t.Errorf("guard spec = %+v, want then node gthen", g.guard)
	}
	// The then is NOT a separate unit (synthesized at run time, like a loop body).
	if unitByNode(units, "gthen") != nil {
		t.Errorf("gthen should not be a separate unit; got %v", nodeIDs(units))
	}
	// done gates on the guard (bare id g).
	done := unitByNode(units, "done")
	if done == nil || !containsStr(done.afterDeps, "g:0") {
		t.Errorf("done afterDeps = %v, want to include g:0", deref(done).afterDeps)
	}
}

// dispatchNode renders a dispatch node over a subject ref with the given arms
// (each `match:<lit> -> exec body`).
func dispatchNode(id string, after []string, subjectRef string, arms ...[2]string) string {
	a, _ := json.Marshal(after)
	var armJSON []string
	for i, arm := range arms {
		bodyID := id + "_arm" + string(rune('0'+i))
		armJSON = append(armJSON, `{"match":{"kind":"literal","value":`+jsonStr(arm[0])+`},"body":`+
			execNode(bodyID, nil, arm[1])+`}`)
	}
	return `{"kind":"dispatch","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"subject":{"kind":"ref","name":"` + subjectRef + `"},"arms":[` + strings.Join(armJSON, ",") + `]}`
}

// TestLowerDispatchLowers pins that a dispatch lowers to a unitDispatch carrying its
// subject + arms, gated on the subject's node-refs (DET), with no separate arm units.
func TestLowerDispatchLowers(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		execNode("pick", nil, "echo separate")+","+
			dispatchNode("d", nil, "pick", [2]string{"separate", "echo a"}, [2]string{"shared", "echo b"}),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	d := unitByNode(units, "d")
	if d == nil || d.kind != unitDispatch || d.dispatch == nil {
		t.Fatalf("d = %+v, want a unitDispatch", d)
	}
	if len(d.dispatch.arms) != 2 {
		t.Errorf("arms = %d, want 2", len(d.dispatch.arms))
	}
	// The dispatch gates on subject-ref node `pick` (stable decision).
	if !containsStr(d.afterDeps, "pick:0") {
		t.Errorf("dispatch afterDeps = %v, want to include pick:0 (subject-ref gate)", d.afterDeps)
	}
	// Arm bodies are not separate units (synthesized at run time).
	if unitByNode(units, "d_arm0") != nil {
		t.Errorf("arm body should not be a separate unit; got %v", nodeIDs(units))
	}
}

// TestLowerDispatchDuplicateBodyIdRefused pins that two arms sharing a body id are
// refused (their activations would collide — the write-once decision record).
func TestLowerDispatchDuplicateBodyIdRefused(t *testing.T) {
	arms := `{"match":{"kind":"literal","value":"a"},"body":` + execNode("shared", nil, "echo a") + `},` +
		`{"match":{"kind":"literal","value":"b"},"body":` + execNode("shared", nil, "echo b") + `}`
	node := `{"kind":"dispatch","id":"d","name":"d","after":[],"subject":{"kind":"ref","name":"p"},"arms":[` + arms + `]}`
	doc := decodeBundle(t, plainDoc(node))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "duplicate arm body id") {
		t.Fatalf("want a duplicate-arm-body-id refusal, got %v", err)
	}
}

// TestLowerDispatchArmBodyCollidesWithNodeRefused pins the red-team runner-up fix: a
// dispatch arm body id that collides with a real sibling node is refused (their
// activations would collide, forging the write-once decision record).
func TestLowerDispatchArmBodyCollidesWithNodeRefused(t *testing.T) {
	arms := `{"match":{"kind":"literal","value":"a"},"body":` + execNode("prep", nil, "echo arm") + `}`
	node := `{"kind":"dispatch","id":"d","name":"d","after":[],"subject":{"kind":"ref","name":"p"},"arms":[` + arms + `]}`
	doc := decodeBundle(t, plainDoc(execNode("prep", nil, "echo real")+","+node))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "collides with node") {
		t.Fatalf("want an arm-body/node collision refusal, got %v", err)
	}
}

// TestLowerGuardCondRefGatesGuard pins the red-team DET fix: a guard whose cond
// reads a NODE output must gate on that node, so the cond is evaluated over stable,
// complete state (never flipping across Advance passes as the fold grows). Here the
// cond `b == "x"` reads node b, so g must gate on b even without an authored `after`.
func TestLowerGuardCondRefGatesGuard(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		execNode("b", nil, "echo bv")+","+
			guardNode("g", nil, condRefEq("b", "x"), execNode("gthen", nil, "echo then")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	g := unitByNode(units, "g")
	if g == nil {
		t.Fatal("no guard unit")
	}
	if !containsStr(g.afterDeps, "b:0") {
		t.Errorf("guard afterDeps = %v, want to include b:0 (cond-ref gate — stable decision)", g.afterDeps)
	}
}

// TestLowerGuardSelfRefCondRefused pins the refusal of a self-referential guard
// cond (one that reads its own then output — nonsensical + a resume-flip hazard).
func TestLowerGuardSelfRefCondRefused(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		guardNode("g", nil, condRefEq("gthen", "x"), execNode("gthen", nil, "echo t")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "self-referential") {
		t.Fatalf("want a self-referential-cond refusal, got %v", err)
	}
}

// TestLowerGuardInSubFormulaLowers (§3 lowering-success shape at prefix) pins the core
// slice: a guard inlined in a run sub-formula lowers to a unitGuard at the qualified id
// `greeting/g`, carrying the namespace (ns "greeting/"), a QUALIFIED thenNodeID
// `greeting/gthen`, and a QUALIFIED rawAfter (`greeting/b` — the authored `after: b`
// follows the prefix); the then stays synthesized (no separate unit).
func TestLowerGuardInSubFormulaLowers(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "greeter", "name", "who"),
		greeterFormula("greeter",
			execNode("b", nil, "echo bv")+","+
				guardNode("g", []string{"b"}, condRefEq("name", "x"), execNode("gthen", nil, "echo t"))),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a guard-in-sub-formula: %v", err)
	}
	g := unitByNode(units, "greeting/g")
	if g == nil || g.kind != unitGuard {
		t.Fatalf("greeting/g = %+v, want a unitGuard; got %v", g, nodeIDs(units))
	}
	if g.ns != "greeting/" {
		t.Errorf("guard ns = %q, want greeting/", g.ns)
	}
	if g.activation != "greeting/g:0" {
		t.Errorf("guard activation = %q, want greeting/g:0", g.activation)
	}
	if g.guard == nil || g.guard.thenNodeID != "greeting/gthen" {
		t.Errorf("guard spec thenNodeID = %+v, want greeting/gthen (qualified)", deref(g).guard)
	}
	if len(g.rawAfter) != 1 || g.rawAfter[0] != "greeting/b" {
		t.Errorf("guard rawAfter = %v, want [greeting/b] (authored after qualified by the prefix)", g.rawAfter)
	}
	if !containsStr(g.afterDeps, "greeting/b:0") {
		t.Errorf("guard afterDeps = %v, want to include greeting/b:0", g.afterDeps)
	}
	// The then is synthesized at run time, not a separate unit.
	if unitByNode(units, "greeting/gthen") != nil {
		t.Errorf("greeting/gthen must NOT be a separate unit; got %v", nodeIDs(units))
	}
}

// TestLowerGuardShapeRefusalMatrix (§2.8) pins the structural refusals with their exact
// messages, at the ROOT and inside a SUB-FORMULA: a missing cond, a missing then, a then
// missing its id, and a non-leaf then all refuse with ErrUnsupportedNode at load.
func TestLowerGuardShapeRefusalMatrix(t *testing.T) {
	cond := condRefEq("name", "x")
	cases := []struct {
		name  string
		guard string
		want  string
	}{
		{
			name:  "missing cond",
			guard: `{"kind":"guard","id":"g","name":"g","after":[],"then":` + execNode("gthen", nil, "echo t") + `}`,
			want:  `guard "g" missing cond`,
		},
		{
			name:  "missing then",
			guard: `{"kind":"guard","id":"g","name":"g","after":[],"cond":` + cond + `}`,
			want:  `guard "g" missing then`,
		},
		{
			name:  "then missing id",
			guard: `{"kind":"guard","id":"g","name":"g","after":[],"cond":` + cond + `,"then":{"kind":"exec","name":"x","after":[],"interpreter":{"program":{"kind":"shell"}},"body":{"raw":"echo t"}}}`,
			want:  `guard "g" then missing id`,
		},
		{
			name:  "non-leaf then",
			guard: `{"kind":"guard","id":"g","name":"g","after":[],"cond":` + cond + `,"then":` + scatterOf("tb", execNode("m", nil, "echo m")) + `}`,
			want:  `guard "g" then kind "scatter" (only exec/do leaf then)`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Root placement.
			_, err := buildUnits(decodeBundle(t, plainDoc(tc.guard)), true, true)
			if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("root: err = %v, want ErrUnsupportedNode containing %q", err, tc.want)
			}
			// Sub-formula placement (the restructured region must refuse identically).
			_, err = buildUnits(decodeBundle(t, runMainDoc(
				runNode("greeting", nil, "greeter", "name", "who"),
				greeterFormula("greeter", tc.guard))), true, true)
			if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), tc.want) {
				t.Errorf("sub-formula: err = %v, want ErrUnsupportedNode containing %q", err, tc.want)
			}
		})
	}
}

// TestLowerGuardSelfRefCondRefusedInSubFormula (§2.8) pins the self-referential-cond
// refusal INSIDE a run sub-formula (the root pin lives in TestLowerGuardSelfRefCondRefused;
// the self-ref sweep compares BARE ids, so it must keep firing at any prefix).
func TestLowerGuardSelfRefCondRefusedInSubFormula(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "greeter", "name", "who"),
		greeterFormula("greeter",
			guardNode("g", nil, condRefEq("gthen", "x"), execNode("gthen", nil, "echo t"))),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "self-referential") {
		t.Fatalf("want a self-referential-cond refusal inside the sub-formula, got %v", err)
	}
}

// TestLowerGuardInSubFormulaCondRefGatesQualified (§3 condRef gate qualification) pins
// that a guard cond reading a sub-SIBLING node gates the guard on that sibling's
// QUALIFIED activation (`greeting/b:0`), so the ns decision is stable across passes —
// the DET cond-ref gate follows the prefix exactly like a leaf's `after`.
func TestLowerGuardInSubFormulaCondRefGatesQualified(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "greeter", "name", "who"),
		greeterFormula("greeter",
			execNode("b", nil, "echo bv")+","+
				guardNode("g", nil, condRefEq("b", "x"), execNode("gthen", nil, "echo t"))),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	g := unitByNode(units, "greeting/g")
	if g == nil {
		t.Fatal("no greeting/g unit")
	}
	if !containsStr(g.afterDeps, "greeting/b:0") {
		t.Errorf("guard afterDeps = %v, want to include greeting/b:0 (qualified cond-ref gate)", g.afterDeps)
	}
}

// TestLowerGuardInSubFormulaSilentRefSubstitutesClosure (§2.5 gate substitution) pins
// that a guard cond reading a SILENT sub-let gates on the let's transitive NON-SILENT
// closure (its real inputs), not on the silent activation itself — a silent unit never
// settles, so gating on it would defer the sub-graph forever on the Advance path. The
// guard reads msg, a silent interp over {{prep}}.
func TestLowerGuardInSubFormulaSilentRefSubstitutesClosure(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "greeter", "name", "who"),
		greeterFormula("greeter",
			execNode("prep", nil, "echo p")+","+
				`{"kind":"interp","id":"msg","name":"msg","after":["prep"],"body":{"raw":"{{ prep }}"}}`+","+
				guardNode("g", nil, condRefEq("msg", "x"), execNode("gthen", nil, "echo t"))),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	g := unitByNode(units, "greeting/g")
	if g == nil {
		t.Fatal("no greeting/g unit")
	}
	if containsStr(g.afterDeps, "greeting/msg:0") {
		t.Errorf("guard gates on SILENT greeting/msg:0 (never settles → Advance wedge); afterDeps=%v", g.afterDeps)
	}
	if !containsStr(g.afterDeps, "greeting/prep:0") {
		t.Errorf("guard must gate on greeting/prep:0 (silent msg's non-silent closure); afterDeps=%v", g.afterDeps)
	}
}

// TestLowerGuardInScatterInSubFormulaLowers (§2.9a) pins that a guard is a legal scatter
// member INSIDE a run sub-formula: it lowers (no ErrUnsupportedNode), is parented to the
// namespaced scatter activation, and is collected as a scatter member alongside a leaf.
func TestLowerGuardInScatterInSubFormulaLowers(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "greeter", "name", "who"),
		greeterFormula("greeter",
			scatterOf("lanes",
				execNode("direct", nil, "echo d"),
				guardNode("g", nil, condRefEq("name", ""), execNode("gthen", nil, "echo t")))),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a guard-in-scatter-in-sub-formula: %v", err)
	}
	g := unitByNode(units, "greeting/g")
	if g == nil || g.kind != unitGuard {
		t.Fatalf("greeting/g = %+v, want a unitGuard", g)
	}
	if g.parent != "greeting/lanes:0" {
		t.Errorf("guard parent = %q, want greeting/lanes:0 (a scatter member)", g.parent)
	}
	agg := unitByNode(units, "greeting/lanes")
	if agg == nil || agg.kind != unitScatterAgg {
		t.Fatalf("greeting/lanes = %+v, want a scatter aggregate", agg)
	}
	if !containsStr(agg.members, "greeting/g:0") {
		t.Errorf("scatter members = %v, want to include the guard greeting/g:0", agg.members)
	}
}

// TestLowerGuardCondRefDelimiterRefused (§2.8 charset ban) pins that a guard cond ref
// carrying '/' or ':' is refused with ErrUnsupportedNode at BOTH the root and inside a
// sub-formula — a delimiter-bearing ref is a forged cross-namespace key (idents carry
// neither delimiter). It mirrors the decodeRunEnv env-ref charset parity.
func TestLowerGuardCondRefDelimiterRefused(t *testing.T) {
	for _, ref := range []string{"a/b", "a:0"} {
		// Root placement.
		rootDoc := decodeBundle(t, plainDoc(
			guardNode("g", nil, condRefEq(ref, "x"), execNode("gthen", nil, "echo t"))))
		_, err := buildUnits(rootDoc, true, true)
		if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("root ref %q: err = %v, want an ErrUnsupportedNode charset refusal", ref, err)
		}
		// Sub-formula placement.
		subDocIR := decodeBundle(t, runMainDoc(
			runNode("greeting", nil, "greeter", "name", "who"),
			greeterFormula("greeter",
				guardNode("g", nil, condRefEq(ref, "x"), execNode("gthen", nil, "echo t")))))
		_, err = buildUnits(subDocIR, true, true)
		if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("sub-formula ref %q: err = %v, want an ErrUnsupportedNode charset refusal", ref, err)
		}
	}
}

// errorsIsUnsupported reports whether err wraps ErrUnsupportedNode (the enqueue-gate
// triage sentinel the charset ban must carry).
func errorsIsUnsupported(err error) bool { return errors.Is(err, ErrUnsupportedNode) }

// TestLowerGuardCondRefSynthBodyRefused (P1) pins the synth-body cond-ref refusal: a
// guard cond ref naming a SIBLING guard's synthesized then id ("b.then" — '.' passes the
// charset ban) is never a lowered unit, so it would take NO gate edge, yet record()
// exposes it in the flat nodeOutputs once the then runs — an ungated read whose decision
// diverges by driver (inline sees the settled then; pool freezes null the same pass).
// Refused loudly at the ROOT and inside a SUB-FORMULA, ErrUnsupportedNode-wrapped.
func TestLowerGuardCondRefSynthBodyRefused(t *testing.T) {
	guards := guardNode("b", nil, condRefEq("mode", "go"), execNode("b.then", nil, "echo bt")) + "," +
		guardNode("a", nil, condRefEq("b.then", "x"), execNode("a.then", nil, "echo at"))
	// Root placement.
	rootDoc := decodeBundle(t, plainDoc(guards))
	_, err := buildUnits(rootDoc, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") || !strings.Contains(err.Error(), "b.then") {
		t.Errorf("root: err = %v, want an ErrUnsupportedNode synth-body refusal naming b.then", err)
	}
	// Sub-formula placement.
	subDocIR := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "greeter", "name", "who"),
		greeterFormula("greeter", guards)))
	_, err = buildUnits(subDocIR, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") || !strings.Contains(err.Error(), "b.then") {
		t.Errorf("sub-formula: err = %v, want an ErrUnsupportedNode synth-body refusal naming b.then", err)
	}
}

// TestLowerGuardCondRefLoopBodyRefused (P1) pins the loop-body variant of the same
// hole: a guard cond reffing a retry/repeat's spec-only BODY id (byNodeID misses the
// runtime-minted attempt activation too, but nodeOutputs resolves it once an attempt
// settles) is refused through the same synth registry (lowerLoop addSynth-registers
// bodyNodeID).
func TestLowerGuardCondRefLoopBodyRefused(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		retryMember("r1", "rbody", "echo a")+","+
			guardNode("g", nil, condRefEq("rbody", "x"), execNode("gthen", nil, "echo t")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") || !strings.Contains(err.Error(), "rbody") {
		t.Errorf("err = %v, want an ErrUnsupportedNode synth-body refusal naming the loop body rbody", err)
	}
}

// plainDoc wraps a node list into a full IR doc (no formulas bundle).
func plainDoc(nodes string) string {
	return `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[]},"nodes":[` + nodes + `]}`
}

// retryMember renders a retry loop (distinct loop + body ids) with a literal
// 2-attempt budget (the budget is irrelevant to the lowering-shape pins that use it).
func retryMember(loopID, bodyID, script string) string {
	return `{"kind":"retry","id":"` + loopID + `","name":"` + loopID + `","after":[],` +
		`"attempts":{"kind":"literal","value":2},` +
		`"body":{"kind":"exec","id":"` + bodyID + `","name":"` + bodyID + `","after":[],` +
		`"interpreter":{"program":{"kind":"shell"}},"body":{"raw":` + jsonStr(script) + `},` +
		`"exitMap":{"pass":[0],"retryable":[]}}}`
}

// repeatMemberForgedCond renders a repeat leaf loop whose cond carries a '/'-FORGED ref
// — a DURABLE un-lowerable body (the reserved-delimiter charset ban is a permanent
// language invariant), used by the ⚑S4 dry-run refusal pins now that a plain nested loop
// lowers inside a sub-formula (LIS).
func repeatMemberForgedCond(loopID, bodyID string) string {
	forged := `{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"forged/ref","field":"outcome"},{"kind":"literal","value":"pass"}]}`
	return `{"kind":"repeat","id":"` + loopID + `","name":"` + loopID + `","after":[],` +
		`"iterationName":"iteration","cond":` + forged + `,` +
		`"body":{"kind":"exec","id":"` + bodyID + `","name":"` + bodyID + `","after":[],` +
		`"interpreter":{"program":{"kind":"shell"}},"body":{"raw":"echo hi"},` +
		`"exitMap":{"pass":[0],"retryable":[]}}}`
}

// scatterOf renders an ungated members-form scatter over the given member node JSONs.
func scatterOf(id string, members ...string) string {
	return `{"kind":"scatter","id":"` + id + `","name":"` + id + `","after":[]` +
		`,"form":"members","on_fail":"continue","members":[` + strings.Join(members, ",") + `]}`
}

// TestLowerRetryInScatterLowers (RN) pins that a retry loop is a legal scatter
// member: the loop lowers (no ErrUnsupportedNode), parented to the scatter, and is
// collected as a scatter member. Before the slice this refused (loops top-level only).
func TestLowerRetryInScatterLowers(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		scatterOf("lanes",
			retryMember("r1", "b1", "echo a"),
			retryMember("r2", "b2", "echo b")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused retry-in-scatter: %v", err)
	}
	r1 := unitByNode(units, "r1")
	r2 := unitByNode(units, "r2")
	if r1 == nil || r1.kind != unitLoop {
		t.Fatalf("r1 = %+v, want a unitLoop", r1)
	}
	if r1.parent != "lanes:0" {
		t.Errorf("r1 parent = %q, want lanes:0 (a scatter member)", r1.parent)
	}
	agg := unitByNode(units, "lanes")
	if agg == nil || agg.kind != unitScatterAgg {
		t.Fatalf("lanes = %+v, want a scatter aggregate", agg)
	}
	if !containsStr(agg.members, "r1:0") || !containsStr(agg.members, "r2:0") {
		t.Errorf("scatter members = %v, want the two retry loops r1:0, r2:0", agg.members)
	}
	if r2 == nil {
		t.Fatal("no r2 loop unit")
	}
}

// TestLowerLoopInSubFormulaLowers pins the LIS flip: a retry/repeat loop INSIDE a run
// sub-formula now LOWERS (the prefix fence is deleted; loopScopeNS makes the decision
// scope namespace-aware). The loop unit is namespaced under the run and carries its
// bare + qualified body ids.
func TestLowerLoopInSubFormulaLowers(t *testing.T) {
	sub := `"greeter":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"greeter","input":{"name":"greeter.input","fields":[]},"nodes":[` +
		retryMember("r1", "b1", "echo hi") + `]}`
	runNoEnv := `{"kind":"run","id":"greeting","name":"greeting","after":[],` +
		`"target":{"kind":"by-name","name":"greeter"},"environment":{"fields":[]},"outcome":"transparent"}`
	doc := decodeBundle(t, runMainDoc(runNoEnv, sub))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a loop inside a sub-formula (LIS should lower it): %v", err)
	}
	loop := unitByNode(units, "greeting/r1")
	if loop == nil || loop.kind != unitLoop {
		t.Fatalf("greeting/r1 = %+v, want a namespaced unitLoop", loop)
	}
	if loop.ns != "greeting/" || loop.loop.bodyNodeID != "greeting/b1" || loop.loop.bodyBareID != "b1" {
		t.Errorf("loop = ns %q bodyNodeID %q bodyBareID %q, want greeting/, greeting/b1, b1", loop.ns, loop.loop.bodyNodeID, loop.loop.bodyBareID)
	}
}

// TestLowerRunInlinesNamespacedSubGraph pins the core lowering shape: a
// top-level run inlines its sub-formula's nodes under a `<runID>/` namespace,
// parented to the run activation, and emits a unitRun aggregate after them.
func TestLowerRunInlinesNamespacedSubGraph(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		execNode("prep", nil, "echo prep")+","+
			runNode("greeting", []string{"prep"}, "greeter", "name", "who")+","+
			execNode("done", []string{"greeting"}, "echo done"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}

	// The sub-node is namespaced greeting/hello and parented to the run activation.
	sub := unitByNode(units, "greeting/hello")
	if sub == nil {
		t.Fatalf("no unit for greeting/hello; got %v", nodeIDs(units))
	}
	if sub.parent != "greeting:0" {
		t.Errorf("greeting/hello parent = %q, want greeting:0", sub.parent)
	}
	if sub.ns != "greeting/" {
		t.Errorf("greeting/hello ns = %q, want greeting/", sub.ns)
	}

	// The run aggregate is a unitRun with the sub-node as its member.
	agg := unitByNode(units, "greeting")
	if agg == nil || agg.kind != unitRun {
		t.Fatalf("greeting unit = %+v, want a unitRun", agg)
	}
	if len(agg.members) != 1 || agg.members[0] != "greeting/hello:0" {
		t.Errorf("greeting members = %v, want [greeting/hello:0]", agg.members)
	}

	// `done` gates on the run aggregate (bare node id greeting).
	done := unitByNode(units, "done")
	if done == nil || len(done.afterDeps) != 1 || done.afterDeps[0] != "greeting:0" {
		t.Errorf("done afterDeps = %v, want [greeting:0]", deref(done).afterDeps)
	}
}

// TestLowerRunGatePropagatesOntoSubGraph pins that the run's own `after` gate is
// propagated onto every sub-unit (a run gated on a failed dep runs no sub-effect).
func TestLowerRunGatePropagatesOntoSubGraph(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		execNode("prep", nil, "echo prep")+","+
			runNode("greeting", []string{"prep"}, "greeter", "name", "who"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	sub := unitByNode(units, "greeting/hello")
	if sub == nil {
		t.Fatal("no greeting/hello unit")
	}
	if len(sub.afterDeps) != 1 || sub.afterDeps[0] != "prep:0" {
		t.Errorf("greeting/hello afterDeps = %v, want [prep:0] (run gate propagated)", sub.afterDeps)
	}
}

// TestLowerRunTwoInvocationsSameTargetNoCollision pins §D: two runs of the same
// target get disjoint namespaces, no duplicate-activation error.
func TestLowerRunTwoInvocationsSameTargetNoCollision(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("g1", nil, "greeter", "name", "who")+","+
			runNode("g2", []string{"g1"}, "greeter", "name", "who"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	if unitByNode(units, "g1/hello") == nil || unitByNode(units, "g2/hello") == nil {
		t.Errorf("want disjoint g1/hello and g2/hello; got %v", nodeIDs(units))
	}
}

// TestLowerRunNestedDepth2 pins nested-run qualification outer/inner/leaf.
func TestLowerRunNestedDepth2(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("outer", nil, "mid", "name", "who"),
		greeterFormula("mid", runNode("inner", nil, "leaf", "name", "name"))+","+
			greeterFormula("leaf", execNode("do", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	if unitByNode(units, "outer/inner/do") == nil {
		t.Errorf("want deeply-qualified outer/inner/do; got %v", nodeIDs(units))
	}
}

// TestLowerRunMissingTargetLoud pins a run targeting an absent formula refuses loudly.
func TestLowerRunMissingTargetLoud(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "nonexistent", "name", "who"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "nonexistent") {
		t.Fatalf("want a missing-target refusal naming nonexistent, got %v", err)
	}
}

// TestLowerRunCycleRefused pins a recursive formula cycle (A->A) refuses loudly.
func TestLowerRunCycleRefused(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("r", nil, "loop", "name", "who"),
		`"loop":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},"name":"loop",`+
			`"input":{"name":"loop.input","fields":[{"name":"name","type":{"kind":"atomic","name":"string"},"required":false,"body":false}]},`+
			`"nodes":[`+runNode("again", nil, "loop", "name", "name")+`]}`,
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "cycle") {
		t.Fatalf("want a recursion-cycle refusal, got %v", err)
	}
}

// TestLowerRunUnderScatterLowers is the placement flip (⚑SF-2/⚑SF-3): a run is now a
// legal scatter member. It pins the member-collection shape — the run aggregate IS the
// member (parent = scatterAct), collected ALONGSIDE a leaf member, while its inlined
// sub-units (parent = runAct) are EXCLUDED from the scatter's member set. It also pins
// ⚑NICE: the run agg folds with memberIndex ABSENT (lowerNode's NodeRun arm never
// threads it), consumed by nothing.
func TestLowerRunUnderScatterLowers(t *testing.T) {
	scatterWithRun := `{"kind":"scatter","id":"s","name":"s","after":[],"form":"members",` +
		`"members":[` + execNode("direct", nil, "echo d") + `,` +
		runNode("extra", nil, "greeter", "name", "who") + `]}`
	doc := decodeBundle(t, runMainDoc(
		scatterWithRun,
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused run-in-scatter: %v", err)
	}

	// The run aggregate is a scatter member: parent = scatterAct, memberIndex ABSENT.
	extra := unitByNode(units, "extra")
	if extra == nil || extra.kind != unitRun {
		t.Fatalf("extra = %+v, want a unitRun", extra)
	}
	if extra.parent != "s:0" {
		t.Errorf("extra parent = %q, want s:0 (a scatter member)", extra.parent)
	}
	if extra.memberIndex != nil {
		t.Errorf("extra memberIndex = %v, want nil (⚑NICE: NodeRun arm threads none)", *extra.memberIndex)
	}

	// The run's inlined sub-unit is parented to the run activation and EXCLUDED from
	// the scatter member set.
	sub := unitByNode(units, "extra/hello")
	if sub == nil || sub.parent != "extra:0" {
		t.Fatalf("extra/hello = %+v, want parent extra:0", sub)
	}

	// The scatter aggregates the leaf AND the run agg — never the run's sub-unit.
	agg := unitByNode(units, "s")
	if agg == nil || agg.kind != unitScatterAgg {
		t.Fatalf("s = %+v, want a scatter aggregate", agg)
	}
	if len(agg.members) != 2 || !containsStr(agg.members, "direct:0") || !containsStr(agg.members, "extra:0") {
		t.Errorf("scatter members = %v, want exactly [direct:0 extra:0] (run agg is the member, sub excluded)", agg.members)
	}
	if containsStr(agg.members, "extra/hello:0") {
		t.Errorf("scatter members = %v, must NOT include the run's sub-unit extra/hello:0", agg.members)
	}
}

// TestLowerRunSlashInNodeIDRefused pins the delimiter-forgery guard.
func TestLowerRunSlashInNodeIDRefused(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		execNode("a/b", nil, "echo x"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "a/b") {
		t.Fatalf("want a '/'-in-id refusal, got %v", err)
	}
}

// TestLowerRunEnvUnknownFieldRefused pins the hand-authored-bundle env guard.
func TestLowerRunEnvUnknownFieldRefused(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		runNode("greeting", nil, "greeter", "nosuchfield", "who"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "nosuchfield") {
		t.Fatalf("want an unknown-env-field refusal, got %v", err)
	}
}

// TestLowerRunEnvRefDelimiterRefused pins the env-ref charset guard: an environment
// binding whose ref NAME carries '/' or ':' is refused at lowering. A legal ref can only
// name a same-namespace node or an input (idents cannot contain those chars); a
// delimiter-bearing ref is a forged cross-namespace key — e.g. "sibling/hello" would
// resolve in byNodeID to a SIBLING run's sub-node, bypassing the sibling-member refusal
// (⚑SF-1) and installing a hidden intra-scatter edge. Refusing the whole malformed-ref
// class here closes the bypass at the source.
func TestLowerRunEnvRefDelimiterRefused(t *testing.T) {
	for _, ref := range []string{"x/y", "x:0"} {
		doc := decodeBundle(t, runMainDoc(
			runNode("greeting", nil, "greeter", "name", ref),
			greeterFormula("greeter", execNode("hello", nil, "echo hi")),
		))
		_, err := buildUnits(doc, true, true)
		if err == nil {
			t.Fatalf("ref %q: want a delimiter-ref refusal, got nil", ref)
		}
		if !strings.Contains(err.Error(), "greeting") || !strings.Contains(err.Error(), ref) {
			t.Errorf("ref %q: error = %v, want it to name the run and the ref", ref, err)
		}
	}
}

// TestLowerRunEnvRefGatesSubGraph pins the DET hardening (seed #3): an env
// binding that refs a parent NODE output gates the sub-graph, so the sub-scope
// is stable before any sub-unit renders. `greeting`'s env binds name<-prep
// (a parent node), so greeting/hello must gate on prep even without a direct
// `after`.
func TestLowerRunEnvRefGatesSubGraph(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		execNode("prep", nil, "echo prep")+","+
			// run has NO `after: prep`, but its env refs prep — the gate must be inferred.
			runNode("greeting", nil, "greeter", "name", "prep"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	sub := unitByNode(units, "greeting/hello")
	if sub == nil {
		t.Fatal("no greeting/hello unit")
	}
	if !containsStr(sub.afterDeps, "prep:0") {
		t.Errorf("greeting/hello afterDeps = %v, want to include prep:0 (env-ref gate)", sub.afterDeps)
	}
}

// TestLowerRunEnvRefSilentNodeSubstitutesClosure pins the red-team fix: an env
// binding that reads a SILENT (lit/interp) parent node must gate on that silent
// node's transitive NON-SILENT closure (its real inputs), NOT on the silent
// activation itself — a silent unit never settles, so gating on it directly would
// defer the sub-graph forever on the Advance path (ErrAdvanceStalled). Here
// greeting binds name<-msg, where msg is a silent interp over {{prep}}.
func TestLowerRunEnvRefSilentNodeSubstitutesClosure(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		execNode("prep", nil, "echo p")+","+
			`{"kind":"interp","id":"msg","name":"msg","after":["prep"],"body":{"raw":"{{ prep }}"}}`+","+
			runNode("greeting", nil, "greeter", "name", "msg"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	sub := unitByNode(units, "greeting/hello")
	if sub == nil {
		t.Fatal("no greeting/hello unit")
	}
	if containsStr(sub.afterDeps, "msg:0") {
		t.Errorf("greeting/hello gates on SILENT msg:0 (never settles → Advance wedge); afterDeps=%v", sub.afterDeps)
	}
	if !containsStr(sub.afterDeps, "prep:0") {
		t.Errorf("greeting/hello must gate on prep:0 (silent msg's non-silent closure); afterDeps=%v", sub.afterDeps)
	}
}

// TestLowerRunEnvRefPureLiteralNoGate pins that an env ref to a pure literal (a
// silent node with NO deps) contributes NO gate — its value is render-stable, so
// there is nothing that must settle first.
func TestLowerRunEnvRefPureLiteralNoGate(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		`{"kind":"lit","id":"word","name":"word","after":[],"value":{"kind":"literal","value":"world"}}`+","+
			runNode("greeting", nil, "greeter", "name", "word"),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	sub := unitByNode(units, "greeting/hello")
	if sub == nil || len(sub.afterDeps) != 0 {
		t.Errorf("greeting/hello afterDeps = %v, want empty (pure-literal env ref = no gate)", deref(sub).afterDeps)
	}
}

// TestNoRunLoweringUnchanged pins byte-identity for a run-free doc: buildUnits
// still lowers a plain exec chain exactly as before the signature change.
func TestNoRunLoweringUnchanged(t *testing.T) {
	doc := decodeBundle(t, `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},`+
		`"name":"m","input":{"name":"m.input","fields":[]},"nodes":[`+
		execNode("a", nil, "echo a")+","+execNode("b", []string{"a"}, "echo b")+`]}`)
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	if len(units) != 2 {
		t.Fatalf("want 2 units, got %d (%v)", len(units), nodeIDs(units))
	}
	b := unitByNode(units, "b")
	if b == nil || len(b.afterDeps) != 1 || b.afterDeps[0] != "a:0" {
		t.Errorf("b afterDeps = %v, want [a:0]", deref(b).afterDeps)
	}
	for i := range units {
		if units[i].ns != "" {
			t.Errorf("unit %q ns = %q, want empty for a run-free doc", units[i].nodeID, units[i].ns)
		}
	}
}

// TestEvalValueExprWrapperOverRef pins the new expr-wrapper arm evalValue needs
// for environment bindings (compiled shape {"kind":"expr","expr":{...}}).
func TestEvalValueExprWrapperOverRef(t *testing.T) {
	raw := json.RawMessage(`{"kind":"expr","expr":{"kind":"ref","name":"who"}}`)
	got, err := evalValue(raw, map[string]string{"who": "world"})
	if err != nil {
		t.Fatalf("evalValue expr wrapper: %v", err)
	}
	if got != "world" {
		t.Errorf("evalValue = %q, want world", got)
	}
}

func nodeIDs(units []planUnit) []string {
	out := make([]string, len(units))
	for i := range units {
		out[i] = units[i].nodeID
	}
	return out
}

func deref(u *planUnit) planUnit {
	if u == nil {
		return planUnit{}
	}
	return *u
}
