package engine

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// eachNode renders a scatter(form:each) node (fixed id "fan", binder "item", on_fail
// "continue", ungated) with the given over-expression and single-leaf body — the FIS
// lowering fixtures vary only the over and the body.
func eachNode(over, body string) string {
	return `{"kind":"scatter","id":"fan","name":"fan","after":[],` +
		`"form":"each","binder":"item","over":` + over +
		`,"body":{"kind":"block","id":"fan.body","after":[],"members":[` + body + `]},` +
		`"on_fail":"continue"}`
}

// memberOverIR renders an `input.<field>` member over-expression (the conformance-golden
// form) for the internal lowering fixtures.
func memberOverIR(field string) string {
	return `{"kind":"member","base":{"kind":"ref","name":"input"},"name":"` + field + `"}`
}

// forEachSubMainDoc wraps a run of a `greeter` sub-formula whose single node is a
// for-each over `over` (the greeter declares a required array input `arr`, env-bound
// from the main `who` — lowering does not type-check the binding).
func forEachSubMainDoc(over string) string {
	sub := `"greeter":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},"name":"greeter",` +
		`"input":{"name":"greeter.input","fields":[{"name":"arr","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}]},` +
		`"nodes":[` + eachNode(over, execNode("mem", nil, `echo "{{ item }}"`)) + `]}`
	run := `{"kind":"run","id":"greeting","name":"greeting","after":[],"target":{"kind":"by-name","name":"greeter"},` +
		`"environment":{"fields":[{"name":"arr","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}}]},"outcome":"transparent"}`
	return runMainDoc(run, sub)
}

// TestLowerForEachInSubFormula (§2.10 positive prefix pin) proves the fence is lifted: a
// for-each inside a run sub-formula lowers to a unitForEach at the qualified id
// greeting/fan with the run namespace, no member units (the count is a runtime property).
func TestLowerForEachInSubFormula(t *testing.T) {
	doc := decodeBundle(t, forEachSubMainDoc(refV("arr")))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v (for-each-in-sub-formula must lower)", err)
	}
	fan := unitByNode(units, "greeting/fan")
	if fan == nil || fan.kind != unitForEach || fan.forEach == nil {
		t.Fatalf("greeting/fan = %+v, want a unitForEach", fan)
	}
	if fan.ns != "greeting/" {
		t.Errorf("for-each ns = %q, want greeting/", fan.ns)
	}
	// The member is runtime-materialized, never a lowered unit.
	if unitByNode(units, "greeting/fan/0") != nil {
		t.Errorf("greeting/fan/0 should not be a lowered unit; got %v", nodeIDs(units))
	}
}

// TestLowerForEachMemberOverBesideInputNodeNoGate (§1.1.2 ⚑S1) pins the head-derived
// overRefs footprint fix: a member-over `input.arr` lowers with NO over-ref candidates,
// so a real node literally named "input" beside it takes NO gate edge and no refusal —
// collectRefs would have returned the base ref "input" and installed a spurious gate.
func TestLowerForEachMemberOverBesideInputNodeNoGate(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		execNode("input", nil, "echo hi")+","+
			eachNode(memberOverIR("arr"), execNode("mem", nil, `echo "{{ item }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v (member-over beside a node named input must lower)", err)
	}
	fan := unitByNode(units, "fan")
	if fan == nil || fan.forEach == nil {
		t.Fatalf("fan = %+v, want a unitForEach", fan)
	}
	if len(fan.forEach.overRefs) != 0 {
		t.Errorf("member-over overRefs = %v, want [] (head-derived: a member form reads no node)", fan.forEach.overRefs)
	}
	if containsStr(fan.afterDeps, "input:0") {
		t.Errorf("for-each afterDeps = %v, want NO gate on the node named input (spurious collectRefs edge)", fan.afterDeps)
	}
}

// TestLowerForEachOverRefDelimiterRefused (§2.10 charset ban) pins that a for-each `over`
// ref carrying '/' or ':' is refused with ErrUnsupportedNode at BOTH the root and inside a
// sub-formula — a delimiter-bearing ref forges a cross-namespace scope key.
func TestLowerForEachOverRefDelimiterRefused(t *testing.T) {
	for _, ref := range []string{"a/b", "a:0"} {
		rootDoc := decodeBundle(t, plainDoc(
			eachNode(refV(ref), execNode("mem", nil, "echo 1"))))
		_, err := buildUnits(rootDoc, true, true)
		if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("root over ref %q: err = %v, want an ErrUnsupportedNode charset refusal", ref, err)
		}
		subDoc := decodeBundle(t, forEachSubMainDoc(refV(ref)))
		_, err = buildUnits(subDoc, true, true)
		if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "must not contain") {
			t.Errorf("sub over ref %q: err = %v, want an ErrUnsupportedNode charset refusal", ref, err)
		}
	}
}

// TestLowerForEachInSubOverSilentSubNodeRefused (§2.10 silent-over inside ns) pins the
// namespace-aware silent-over sweep: a for-each in a run sub-formula whose `over` reads a
// SILENT sub-sibling (a lit) has no settleable gate, so it is refused at load — the sweep
// keys byNodeID on the qualified id greeting/slit.
func TestLowerForEachInSubOverSilentSubNodeRefused(t *testing.T) {
	silent := `{"kind":"lit","id":"slit","name":"slit","after":[],"value":{"kind":"literal","value":"x"}}`
	sub := `"greeter":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},"name":"greeter",` +
		`"input":{"name":"greeter.input","fields":[{"name":"arr","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}]},` +
		`"nodes":[` + silent + `,` + eachNode(refV("slit"), execNode("mem", nil, "echo 1")) + `]}`
	run := `{"kind":"run","id":"greeting","name":"greeting","after":[],"target":{"kind":"by-name","name":"greeter"},` +
		`"environment":{"fields":[{"name":"arr","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}}]},"outcome":"transparent"}`
	doc := decodeBundle(t, runMainDoc(run, sub))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "silent node") || !strings.Contains(err.Error(), "greeting/fan") {
		t.Errorf("err = %v, want an ErrUnsupportedNode silent-over refusal naming greeting/fan inside the ns", err)
	}
}

// TestLowerForEachOverSynthBodyRefused (§2.10 ⚑S2) pins the synth-body ban extended to a
// for-each `over` ref: a ref naming a sibling guard's synthesized `then` id is never a
// lowered unit, yet record() exposes it in the flat nodeOutputs once the then runs — an
// ungated read whose fan diverges by driver. Refused loudly with the for-each-keyed
// message.
func TestLowerForEachOverSynthBodyRefused(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		guardNode("g", nil, condRefEq("mode", "go"), execNode("gthen", nil, "echo t"))+","+
			eachNode(refV("gthen"), execNode("mem", nil, "echo 1"))))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") ||
		!strings.Contains(err.Error(), "gthen") || !strings.Contains(err.Error(), "for-each") {
		t.Errorf("err = %v, want a for-each over-ref synth-body refusal naming gthen", err)
	}
}

// TestLowerDispatchSubjectSynthBodyRefused (§2.10 ⚑S2, Q-D) pins the SAME ban extended to
// a dispatch `subject` ref naming a SIBLING synth body (a guard's then — a dispatch's OWN
// arm ids are already caught by the self-referential check): the live root hole where
// subjectRefs traverse the miss arm ungated while advanceDispatch freezes the chosen arm
// write-once, so arm selection diverges by driver. Refused with the dispatch-keyed message.
func TestLowerDispatchSubjectSynthBodyRefused(t *testing.T) {
	doc := decodeBundle(t, plainDoc(
		guardNode("g", nil, condRefEq("mode", "go"), execNode("gthen", nil, "echo t"))+","+
			dispatchNode("d", nil, "gthen", [2]string{"go", "echo a"})))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") ||
		!strings.Contains(err.Error(), "gthen") || !strings.Contains(err.Error(), "dispatch") {
		t.Errorf("err = %v, want a dispatch subject-ref synth-body refusal naming gthen", err)
	}
}

// TestEvalForEachArrayUnregisteredNamespaceLoud (§2.11 ⚑S3) pins that evalForEachArray
// over an unregistered namespace refuses loudly BEFORE the arm switch — for BOTH the ref
// and member arms (a direct unit test: register-before-drive makes this state unreachable
// on every real path, so a hit is a structural bug, not a silent empty fan). The INNER
// message carries no "lumen:"/for-each-id prefix — the driver call sites wrap it
// (TestRunForEachUnregisteredNamespaceWrap / TestAdvanceForEachUnregisteredNamespaceWrap),
// so the surfaced error names the fan exactly once (the GIS no-doubled-prefix lesson).
func TestEvalForEachArrayUnregisteredNamespaceLoud(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {}})
	for _, tc := range []struct{ name, over string }{
		{"ref arm", refV("items")},
		{"member arm", memberOverIR("items")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, _, err := d.evalForEachArray("ghost/", &forEachSpec{overRaw: json.RawMessage(tc.over)}, map[string]string{})
			if err == nil || !strings.Contains(err.Error(), `namespace "ghost/" has no registered environment`) {
				t.Fatalf("evalForEachArray(unregistered) err = %v, want the exact inner unregistered-namespace message", err)
			}
			if strings.Contains(err.Error(), "lumen:") {
				t.Fatalf("inner err = %v carries a lumen: prefix — the call-site wrap would double it", err)
			}
		})
	}
}

// unregisteredFanUnit is the hand-built unitForEach both driver-wrap tests feed: a fan in
// an unregistered namespace ghost/ (register-before-drive makes this unreachable on real
// paths; the pin is the P1 wrap contract, not the state).
func unregisteredFanUnit() planUnit {
	return planUnit{
		kind:       unitForEach,
		activation: "ghost/fan:0",
		nodeID:     "ghost/fan",
		ns:         "ghost/",
		forEach:    &forEachSpec{overRaw: json.RawMessage(refV("items")), binder: "item"},
	}
}

// assertForEachWrappedNSError asserts the P1 wrap contract on a driver-surfaced
// unregistered-ns error: `lumen: for-each "ghost/fan" over: namespace "ghost/" has no
// registered environment`, with the "lumen:" prefix appearing exactly once (no doubling).
func assertForEachWrappedNSError(t *testing.T, err error) {
	t.Helper()
	if err == nil ||
		!strings.Contains(err.Error(), `lumen: for-each "ghost/fan" over:`) ||
		!strings.Contains(err.Error(), `namespace "ghost/" has no registered environment`) {
		t.Fatalf("err = %v, want the wrapped `lumen: for-each %%q over: namespace %%q has no registered environment` shape", err)
	}
	if strings.Count(err.Error(), "lumen:") != 1 {
		t.Fatalf("err = %v, want exactly ONE lumen: prefix (the call-site wrap, no doubling)", err)
	}
}

// TestRunForEachUnregisteredNamespaceWrap (P1) pins the INLINE driver's wrap: runForEach
// surfaces the unregistered-ns refusal as `lumen: for-each %q over: %w`, naming the
// failing fan exactly once.
func TestRunForEachUnregisteredNamespaceWrap(t *testing.T) {
	d := condScopeDriver(nil, nil, map[string]*runSpec{"stage/": {}})
	err := d.runForEach(unregisteredFanUnit(), map[string]string{}, map[string]string{})
	assertForEachWrappedNSError(t, err)
}

// TestAdvanceForEachUnregisteredNamespaceWrap (P1, driver parity) pins the POOL driver's
// identical wrap. The fan node is pre-seeded activated so ensureDecisionActivated no-ops
// (no store) and the error path is evalForEachArray's, exactly as on a re-Advance pass.
func TestAdvanceForEachUnregisteredNamespaceWrap(t *testing.T) {
	d := condScopeDriver(
		map[string]*nodeState{"ghost/fan:0": {NodeID: "ghost/fan"}},
		nil,
		map[string]*runSpec{"stage/": {}},
	)
	err := d.advanceForEach(unregisteredFanUnit(), map[string]string{}, map[string]string{}, Options{})
	assertForEachWrappedNSError(t, err)
}

// TestLowerForEachInSubOverRefGatesQualified (§2.4 gate + P2 red-team) pins the
// ns-qualified over-ref DET gate at the PLAN level: a fan whose `over` refs a sub-LEAF
// gates on greeting/up:0, and one whose `over` refs a sub-AGGREGATE (static scatter)
// gates on the aggregate activation — the ordering source when no explicit `after` is
// authored (the behavioral twin dropped its after).
func TestLowerForEachInSubOverRefGatesQualified(t *testing.T) {
	formulaDoc := func(subNodes string) *ir.IR {
		sub := `"greeter":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},"name":"greeter",` +
			`"input":{"name":"greeter.input","fields":[{"name":"arr","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}]},` +
			`"nodes":[` + subNodes + `]}`
		run := `{"kind":"run","id":"greeting","name":"greeting","after":[],"target":{"kind":"by-name","name":"greeter"},` +
			`"environment":{"fields":[{"name":"arr","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}}]},"outcome":"transparent"}`
		return decodeBundle(t, runMainDoc(run, sub))
	}
	t.Run("sub-leaf ref", func(t *testing.T) {
		doc := formulaDoc(execNode("up", nil, `printf '["p","q"]'`) + "," +
			eachNode(refV("up"), execNode("mem", nil, `echo "{{ item }}"`)))
		units, err := buildUnits(doc, true, true)
		if err != nil {
			t.Fatalf("buildUnits: %v", err)
		}
		fan := unitByNode(units, "greeting/fan")
		if fan == nil || !containsStr(fan.afterDeps, "greeting/up:0") {
			t.Errorf("fan afterDeps = %v, want the qualified over-ref gate greeting/up:0", deref(fan).afterDeps)
		}
	})
	t.Run("sub-aggregate ref", func(t *testing.T) {
		scatter := `{"kind":"scatter","id":"sc","name":"sc","after":[],"form":"members",` +
			`"members":[` + execNode("m1", nil, "echo one") + `],"on_fail":"continue"}`
		doc := formulaDoc(scatter + "," +
			eachNode(refV("sc"), execNode("mem", nil, `echo "{{ item }}"`)))
		units, err := buildUnits(doc, true, true)
		if err != nil {
			t.Fatalf("buildUnits: %v", err)
		}
		fan := unitByNode(units, "greeting/fan")
		if fan == nil || !containsStr(fan.afterDeps, "greeting/sc:0") {
			t.Errorf("fan afterDeps = %v, want the qualified aggregate gate greeting/sc:0", deref(fan).afterDeps)
		}
	})
}

// TestForEachRunDoFixtureLowers guards the hand-authored for-each-in-run-body dolt-e2e
// bundle fixture: `repeat { run stage -> reviewer{ fanout: scatter item in items { do } } }
// until stage.outcome==pass || iteration>=2` decodes and lowers (allowDo), so a fixture
// typo fails fast here — not 10min into the e2e. It confirms both the inline and the
// controller-loop pool flag pairs lower the fan under the attempt prefix.
func TestForEachRunDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "for-each-run-do.lumen.json")
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
			t.Fatalf("lower for-each-run-do (allowCombineDo=%v): %v", combineDo, err)
		}
		loop := unitByNode(units, "loop")
		if loop == nil || loop.loop == nil || loop.loop.bodyRun == nil {
			t.Fatalf("no repeat-run-body loop unit; got %v", nodeIDs(units))
		}
		if loop.loop.bodyNodeID != "stage" {
			t.Errorf("body node id = %q, want stage", loop.loop.bodyNodeID)
		}
		// The attempt-0 mint lowers the fan under stage/0/fanout in the attempt namespace.
		sub, _, err := loop.loop.mintRunBodyAttempt(0, "loop:0", "", nil, nil)
		if err != nil {
			t.Fatalf("mint attempt 0 (allowCombineDo=%v): %v", combineDo, err)
		}
		fan := unitByNode(sub, "stage/0/fanout")
		if fan == nil || fan.kind != unitForEach || fan.forEach == nil || fan.ns != "stage/0/" {
			t.Fatalf("minted fan = %+v, want a unitForEach at stage/0/fanout in ns stage/0/; got %v", fan, nodeIDs(sub))
		}
		if fan.forEach.binder != "item" {
			t.Errorf("minted fan binder = %q, want item", fan.forEach.binder)
		}
	}
}
