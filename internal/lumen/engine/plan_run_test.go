package engine

import (
	"encoding/json"
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

// TestLowerRunUnderScatterRefused pins §E: a run nested under a scatter is refused.
func TestLowerRunUnderScatterRefused(t *testing.T) {
	scatterWithRun := `{"kind":"scatter","id":"s","name":"s","after":[],"form":"members",` +
		`"members":[` + runNode("r", nil, "greeter", "name", "who") + `]}`
	doc := decodeBundle(t, runMainDoc(
		scatterWithRun,
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("want a run-under-aggregate refusal, got %v", err)
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
