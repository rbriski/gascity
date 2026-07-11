package engine_test

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
)

// --- for-each-in-sub-formula (FIS) black-box behavioral fixtures -------------

// envField renders one run environment binding: a sub-input field name bound to a
// bare parent-scope ref (the shape scopeFor evaluates against the parent view).
func envField(name, ref string) string {
	return `{"name":"` + name + `","value":{"kind":"expr","expr":{"kind":"ref","name":"` + ref + `"}}}`
}

// arrField renders a REQUIRED array-of-string input field — the shape a for-each `over`
// resolves to a runtime array.
func arrField(name string) string {
	return `{"name":"` + name + `","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}`
}

// TestForEachInSubFormulaRefFormFans (§2.1) proves the corpus shape: a run of a
// sub-formula whose body is `fan: scatter item in arr { exec }` with arr env-bound from
// a parent array fans one member per element (namespaced stage/fan/<i>), each rendered
// with the binder AND a sub-scope value (label), the aggregate passes, and the run seals.
// Member 0 carries elem0 and NOT elem1 (the mutation-catching render pin).
func TestForEachInSubFormulaRefFormFans(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	env := `[` + envField("arr", "items") + `,` + envField("label", "tag") + `]`
	sub := subDoc("reviewer", arrField("arr")+","+strField("label"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "item={{ item }} label={{ label }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items")+","+strField("tag"),
		runNodeRawEnv("stage", nil, "reviewer", env),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}, "tag": "release"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "item=a label=release" {
		t.Errorf("member 0 = %q, want %q (binder + sub-scope value inside ns)", got, "item=a label=release")
	}
	if got := res.NodeOutputs["stage/fan/1"]; got != "item=b label=release" {
		t.Errorf("member 1 = %q, want %q", got, "item=b label=release")
	}
	if strings.Contains(res.NodeOutputs["stage/fan/0"], "item=b") {
		t.Errorf("member 0 = %q leaked element b (per-member binding not isolated)", res.NodeOutputs["stage/fan/0"])
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomePass {
		t.Errorf("aggregate stage/fan = %q, want pass", settled["stage/fan"])
	}
	if settled["stage"] != engine.OutcomePass {
		t.Errorf("run stage = %q, want pass (transparent from the fan)", settled["stage"])
	}
}

// TestForEachInSubFormulaMemberFormEnvBound (§2.2 ⚑B1) proves the `input.<field>`
// member-over inside a namespace reads the run INPUT LAYER (the env binding), not the
// flat root scope — it fans from the SUB binding.
func TestForEachInSubFormulaMemberFormEnvBound(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", memberOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "a" {
		t.Errorf("member 0 = %q, want a (input.arr member-over via the input layer)", got)
	}
	if got := res.NodeOutputs["stage/fan/1"]; got != "b" {
		t.Errorf("member 1 = %q, want b", got)
	}
}

// TestForEachInSubFormulaMemberFormAbsentNoLeak (§2.2 ⚑B1) proves a member-over
// `input.items` naming an OPTIONAL, UNBOUND, no-default sub-input fans ZERO members and
// passes — and a same-named MAIN input (present, 2 elements) does NOT leak in. The
// observable is the member COUNT (0), not the root array's length.
func TestForEachInSubFormulaMemberFormAbsentNoLeak(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// An OPTIONAL, unbound, no-default sub-input named like the MAIN input — its member-over
	// must read absent inside the ns (never leaking the same-named root array).
	optItems := `{"name":"items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":false,"body":false}`
	sub := subDoc("reviewer", optItems+","+strField("note"),
		forEachNode(nil, "item", "continue", memberOver("items"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		strField("who")+","+arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("note", "who")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "x", "items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass (unbound member-over is a vacuous fan)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomePass {
		t.Errorf("aggregate stage/fan = %q, want pass (empty fan)", settled["stage/fan"])
	}
	// The no-leak observable is the SETTLED member count (0), not an output-map absence:
	// a leak of the 2-element root array would mint 2 member settles.
	members := 0
	for id := range settled {
		if strings.HasPrefix(id, "stage/fan/") {
			members++
		}
	}
	if members != 0 {
		t.Errorf("settled ns fan members = %d, want 0 (a same-named MAIN input must NOT leak into the ns)", members)
	}
}

// TestForEachInSubFormulaRefIsolation (§2.3 ⚑B1) proves a ref-over to a MAIN-only input
// from inside a namespace fans ZERO members and passes — no flat-scope leak (the root
// input is invisible in the sub view).
func TestForEachInSubFormulaRefIsolation(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", strField("note"),
		forEachNode(nil, "item", "continue", refOver("rootonly"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		strField("who")+","+arrField("rootonly"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("note", "who")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "x", "rootonly": []any{"p", "q"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomePass {
		t.Errorf("aggregate stage/fan = %q, want pass (root-only ref is invisible in ns → empty fan)", settled["stage/fan"])
	}
	// Settled member count 0 = no flat-scope leak (a leak would mint 2 member settles).
	members := 0
	for id := range settled {
		if strings.HasPrefix(id, "stage/fan/") {
			members++
		}
	}
	if members != 0 {
		t.Errorf("settled ns fan members = %d, want 0 (no flat-scope leak of the MAIN input rootonly)", members)
	}
}

// TestForEachInSubFormulaNonArrayInvalid (§2.3) proves a ref-over resolving to a PRESENT
// non-array binding (a scalar) inside a namespace settles the aggregate
// failed{invalid_input} — the only invalid route (miss is a vacuous pass).
func TestForEachInSubFormulaNonArrayInvalid(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", strField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "who")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomeFailed {
		t.Errorf("aggregate stage/fan = %q, want failed (present non-array binding is invalid_input)", settled["stage/fan"])
	}
	// The settle must carry the invalid_input DETAIL (root parity), not merely failed.
	var detail string
	for _, e := range res.Events {
		if e.Type != engine.EventOutcomeSettled {
			continue
		}
		var p struct {
			Activation string `json:"activation"`
			Detail     string `json:"detail"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode outcome.settled: %v", err)
		}
		if p.Activation == "stage/fan:0" {
			detail = p.Detail
		}
	}
	if detail != "invalid_input" {
		t.Errorf("stage/fan:0 settle detail = %q, want invalid_input", detail)
	}
}

// TestForEachInSubFormulaOverSubSibling (§2.4) proves a bare-ref over to a SUB-SIBLING
// node's output fans from that output — with NO authored `after` (root-twin parity), so
// the ns-qualified over-ref DET gate is the ONLY ordering source (the plan-level twin
// TestLowerForEachInSubOverRefGatesQualified pins the gate edge itself).
func TestForEachInSubFormulaOverSubSibling(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", strField("note"),
		execNode("up", `printf '["p","q"]'`, nil)+","+
			forEachNode(nil, "item", "continue", refOver("up"),
				execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("note", "who")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "x"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "p" {
		t.Errorf("member 0 = %q, want p (over a sub-sibling node output)", got)
	}
	if got := res.NodeOutputs["stage/fan/1"]; got != "q" {
		t.Errorf("member 1 = %q, want q", got)
	}
}

// TestForEachInSubFormulaMemberOverNodeCollisionBindingWins (§1.2 collision, member arm —
// the scopeFor-member-arm mutant killer) proves a member-over `input.arr` fans from the
// env BINDING even when a same-named sub-NODE `arr` has already settled a DIFFERENT
// 3-element array before the fan (explicit after gate). The member arm reads the run
// input LAYER only — with a scopeFor-based member arm the settled child would shadow the
// binding and fan 3 members, failing both asserts. This pins the deliberate asymmetry vs
// condScope's ⚑B1 overlaid view: a future "DRY unification" onto scopeFor must go red here.
func TestForEachInSubFormulaMemberOverNodeCollisionBindingWins(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		execNode("arr", `printf '["x","y","z"]'`, nil)+","+
			forEachNode([]string{"arr"}, "item", "continue", memberOver("arr"),
				execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "a" {
		t.Errorf("member 0 = %q, want a (member arm reads the BINDING, not the settled sub-node)", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	members := 0
	for id := range settled {
		if strings.HasPrefix(id, "stage/fan/") {
			members++
		}
	}
	if members != 2 {
		t.Errorf("settled ns fan members = %d, want 2 from the binding (3 = the sub-node array leaked via a scopeFor member arm)", members)
	}
}

// TestForEachInSubFormulaRefOverLeafShadowsBinding (§1.2 collision, ref arm / leaf class)
// proves a ref-over resolves the scopeFor view's child-shadows-binding order: a settled
// sub-LEAF `arr` (a scope-recording kind) shadows the same-named env binding, so the fan
// comes from the NODE's array (root child-wins parity; the over-ref gate froze it).
func TestForEachInSubFormulaRefOverLeafShadowsBinding(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		execNode("arr", `printf '["x","y"]'`, nil)+","+
			forEachNode(nil, "item", "continue", refOver("arr"),
				execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "x" {
		t.Errorf("member 0 = %q, want x (settled sub-leaf shadows the same-named binding)", got)
	}
	if got := res.NodeOutputs["stage/fan/1"]; got != "y" {
		t.Errorf("member 1 = %q, want y", got)
	}
}

// TestForEachInSubFormulaRefOverAggregateBindingWins (§1.2 collision, ref arm / aggregate
// class) proves a ref-over naming a sub-AGGREGATE (static scatter sibling) that shares an
// env-binding name fans from the BINDING: an aggregate records nodeOutputs-ONLY (never
// scope), so it is invisible to scopeFor's child overlay — while the over-ref gate still
// defers the fan on the aggregate (plan twin: TestLowerForEachInSubOverRefGatesQualified).
func TestForEachInSubFormulaRefOverAggregateBindingWins(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("sc"),
		scatterNode("sc", nil, "continue", execNode("m1", `echo one`, nil))+","+
			forEachNode(nil, "item", "continue", refOver("sc"),
				execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("sc", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/sc"] != engine.OutcomePass {
		t.Fatalf("sub-scatter stage/sc = %q, want pass (the gate target ran)", settled["stage/sc"])
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "a" {
		t.Errorf("member 0 = %q, want a (aggregate is scope-invisible; the binding wins)", got)
	}
	if got := res.NodeOutputs["stage/fan/1"]; got != "b" {
		t.Errorf("member 1 = %q, want b", got)
	}
}

// TestForEachInSubFormulaBinderShadowsSubInput (§2.5) proves the binder shadows a
// same-named sub-input during the member render (the element wins), and the sub-input
// value is restored for a downstream sub-sibling after the fan.
func TestForEachInSubFormulaBinderShadowsSubInput(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", strField("item")+","+arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "m={{ item }}"`, nil))+","+
			execNode("after", `echo "a={{ item }}"`, []string{"fan"}))
	env := `[` + envField("item", "who") + `,` + envField("arr", "items") + `]`
	doc := decodeIR(t, bundleDoc(
		strField("who")+","+arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", env),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "SUBVAL", "items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "m=a" {
		t.Errorf("member 0 = %q, want m=a (binder element shadows the sub-input during render)", got)
	}
	if got := res.NodeOutputs["stage/after"]; got != "a=SUBVAL" {
		t.Errorf("downstream stage/after = %q, want a=SUBVAL (sub-input restored after the fan)", got)
	}
}

// TestForEachInSubFormulaEmptyIsPass (§2.6) proves an empty `over` array inside a
// namespace settles the aggregate PASS with no members, and a downstream sub-sibling
// gated on it still runs (no skip-cascade).
func TestForEachInSubFormulaEmptyIsPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil))+","+
			execNode("after", `echo done`, []string{"fan"}))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomePass {
		t.Errorf("aggregate stage/fan = %q, want pass (empty fan)", settled["stage/fan"])
	}
	if settled["stage/after"] != engine.OutcomePass {
		t.Errorf("downstream stage/after = %q, want pass (empty fan does not skip-cascade)", settled["stage/after"])
	}
}

// TestForEachInSubFormulaOnFailStop (§2.6) proves on_fail:stop fails the aggregate when a
// member fails inside a namespace.
func TestForEachInSubFormulaOnFailStop(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fail := `if [ "{{ item }}" = "bad" ]; then exit 1; fi; echo ok`
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "stop", refOver("arr"),
			execNode("mem", fail, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"ok", "bad"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomeFailed {
		t.Errorf("aggregate stage/fan = %q, want failed (on_fail:stop with a failed member)", settled["stage/fan"])
	}
}

// TestForEachInSubFormulaOnFailContinueDegrades (§2.6) proves on_fail:continue drains a
// failed member into the aggregate as DEGRADED inside a namespace (scatter parity), and
// the run reports the degraded transparently.
func TestForEachInSubFormulaOnFailContinueDegrades(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fail := `if [ "{{ item }}" = "bad" ]; then exit 1; fi; echo ok`
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", fail, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"ok", "bad"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomeDegraded {
		t.Errorf("aggregate stage/fan = %q, want degraded (on_fail:continue, mixed pass/fail)", settled["stage/fan"])
	}
	if settled["stage"] != engine.OutcomeDegraded {
		t.Errorf("run stage = %q, want degraded (transparent from the drained fan)", settled["stage"])
	}
}

// TestForEachInSubFormulaAggregateVisibility (§2.7) proves ns aggregate visibility: a
// sub-sibling gated `after:[fan]` runs; a sibling guard reading `fan.outcome == "pass"`
// (the ⚑B1 flat-nodeOutputs overlay inside condScope) runs its then; and a TEMPLATE-form
// `{{fan}}` renders "" (a silent interp's ref part — the aggregate output is
// nodeOutputs-only, never in the render view; the mutation-catcher against a future
// overlay leak into render scopes). Root parity: raw-form leaves the token verbatim.
func TestForEachInSubFormulaAggregateVisibility(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	// A silent interp whose TEMPLATE parts embed `{{fan}}`: text "v=" + ref fan.
	interpV := `{"kind":"interp","id":"v","name":"v","after":["fan"],` +
		`"parts":[{"kind":"text","value":"v="},{"kind":"interp","expr":{"kind":"ref","name":"fan"}}]}`
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo x`, nil))+","+
			guardExecAfter("g", []string{"fan"}, condOutcomePass("fan"), "gthen", `echo "guard ran"`)+","+
			execNode("after", `echo done`, []string{"fan"})+","+
			interpV+","+
			execNode("probe", `echo "res={{ v }}"`, []string{"v"}))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/after"] != engine.OutcomePass {
		t.Errorf("downstream stage/after = %q, want pass (after:[fan] gate settled)", settled["stage/after"])
	}
	if got := res.NodeOutputs["stage/gthen"]; got != "guard ran" {
		t.Errorf("guard then stage/gthen = %q, want %q (fan.outcome==pass via ⚑B1 overlay inside ns)", got, "guard ran")
	}
	// TEMPLATE-form {{fan}} renders "" inside ns (root parity): the probe echoes the
	// silent interp's value, which must be exactly "v=" — not the aggregate's outcome, a
	// member output, or a verbatim token.
	if got := res.NodeOutputs["stage/probe"]; got != "res=v=" {
		t.Errorf("template-form render probe = %q, want %q ({{fan}} renders empty inside ns)", got, "res=v=")
	}
}

// TestForEachInRunInScatterDrives (§2.9 ⚑S4) proves a for-each lowers and drives inside a
// run sub-formula that is itself a SCATTER member: the fan settles inside the member
// run's sub-graph, and the member run drains transparently into the outer scatter.
func TestForEachInRunInScatterDrives(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	member := runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`)
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		scatterNode("lanes", nil, "continue", member),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["stage/fan/0"]; got != "a" {
		t.Errorf("member 0 = %q, want a (fan inside a run-in-scatter)", got)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["stage/fan"] != engine.OutcomePass {
		t.Errorf("aggregate stage/fan = %q, want pass", settled["stage/fan"])
	}
	if settled["stage"] != engine.OutcomePass {
		t.Errorf("member run stage = %q, want pass", settled["stage"])
	}
	if settled["lanes"] != engine.OutcomePass {
		t.Errorf("outer scatter lanes = %q, want pass (member run drained transparently)", settled["lanes"])
	}
}

// TestForEachInSubFormulaDropRefoldByteIdentity (§2.12 DET) pins that the
// dynamically-materialized ns member rows refold byte-identically.
func TestForEachInSubFormulaDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b", "c"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// TestForEachInSubFormulaCrashMidFanConverges (§2.12) proves a crash right after member 0
// settles resumes convergently inside a namespace: member 1 is re-minted (never member 0,
// reloaded via resumeMemoized), the fan re-materializes the identical array, and the
// projection drop+refolds byte-identically. The array comes from a ROOT NODE (so the run
// takes no input), and the members are pool-dos so the crash boundary has an effect.
func TestForEachInSubFormulaCrashMidFanConverges(t *testing.T) {
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			doNode("mem", "review {{ item }}", nil)))
	doc := decodeIR(t, bundleDoc(
		"",
		execNode("src", `printf '["a","b"]'`, nil)+","+
			runNodeRawEnv("stage", []string{"src"}, "reviewer", `[`+envField("arr", "src")+`]`),
		sub))
	host := &enginehost.StubHost{Results: map[string]enginehost.DoResult{
		"stage/fan/0": {Outcome: enginehost.OutcomePass, Output: "r0"},
		"stage/fan/1": {Outcome: enginehost.OutcomePass, Output: "r1"},
	}}
	resumed, store, stream := injectCrashThenResume(t, doc, host, engine.CrashAfterSettle, "stage/fan/0:0", 0)
	if resumed.Outcome != engine.OutcomePass {
		t.Errorf("resumed outcome = %q, want pass", resumed.Outcome)
	}
	if resumed.NodeOutputs["stage/fan/0"] != "r0" || resumed.NodeOutputs["stage/fan/1"] != "r1" {
		t.Errorf("resumed members = {%q,%q}, want {r0,r1}", resumed.NodeOutputs["stage/fan/0"], resumed.NodeOutputs["stage/fan/1"])
	}
	// member 0 reloaded (never re-invoked), member 1 minted once: exactly 2 host calls.
	if calls := host.Calls(); len(calls) != 2 {
		ids := make([]string, len(calls))
		for i, c := range calls {
			ids[i] = c.NodeID
		}
		t.Errorf("host calls = %d (%v), want 2 (member 0 reloaded, member 1 minted once)", len(calls), ids)
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestForEachInSubFormulaEmptyPoolInlineJournalParity (§2.12 parity) proves an EMPTY
// ns for-each driven inline (Run + Host, never invoked) and pooled (Advance +
// PoolRouter, never dispatching) journals BYTE-IDENTICALLY after run.started.
func TestForEachInSubFormulaEmptyPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			doNode("mem", "review {{ item }}", nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))

	inStore := newStore(t)
	inRes, err := engine.RunWithOptions(ctx, inStore, doc, map[string]any{"items": []any{}}, engine.Options{Host: passDoStub()})
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if n := countJournalType(t, inStore, inRes.StreamID, engine.EventEffectScheduled); n != 0 {
		t.Fatalf("inline effect.scheduled = %d, want 0 (empty fan invokes no host)", n)
	}

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-fis-parempty", map[string]any{"items": []any{}}, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (empty fan dispatches nothing)", n)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestForEachInSubFormulaExecFanPoolInlineJournalParity (§2.12 parity, FAN branch) proves
// a 2-member EXEC-bodied ns fan driven inline (Run) and pooled (Advance — an exec fan
// falls through to runUnit → runForEach and seals in one pass, never dispatching)
// journals BYTE-IDENTICALLY after run.started: same member activations, same settles,
// same order.
func TestForEachInSubFormulaExecFanPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			execNode("mem", `echo "{{ item }}"`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
		sub))
	input := map[string]any{"items": []any{"a", "b"}}

	inStore := newStore(t)
	inRes, err := engine.Run(ctx, inStore, doc, input)
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if inRes.Outcome != engine.OutcomePass {
		t.Fatalf("inline outcome = %q, want pass", inRes.Outcome)
	}

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-fis-parfan", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (exec fan runs inline)", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (an exec fan never dispatches)", n)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestForEachInSubFormulaInvalidPoolInlineJournalParity (§2.12 parity, FALSE branch)
// proves the ns non-array invalid_input settle journals identically on both drivers: the
// inline host is never invoked, the pool driver dispatches nothing and seals failed in
// one pass, and the journals byte-compare after run.started.
func TestForEachInSubFormulaInvalidPoolInlineJournalParity(t *testing.T) {
	ctx := context.Background()
	sub := subDoc("reviewer", strField("arr"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			doNode("mem", "review {{ item }}", nil)))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "who")+`]`),
		sub))
	input := map[string]any{"who": "world"}

	inStore := newStore(t)
	inRes, err := engine.RunWithOptions(ctx, inStore, doc, input, engine.Options{Host: passDoStub()})
	if err != nil {
		t.Fatalf("inline run: %v", err)
	}
	if n := countJournalType(t, inStore, inRes.StreamID, engine.EventEffectScheduled); n != 0 {
		t.Fatalf("inline effect.scheduled = %d, want 0 (an invalid fan invokes no host)", n)
	}

	poolStore := newStore(t)
	fake := newFakeWorkStore()
	r, err := engine.Advance(ctx, poolStore, doc, "gcg-fis-parinvalid", input, fake.opts())
	if err != nil || !r.Sealed {
		t.Fatalf("pool advance = %+v err %v, want Sealed in one pass (invalid fan settles immediately)", r, err)
	}
	if n := fake.dispatchCount(); n != 0 {
		t.Fatalf("pool dispatch count = %d, want 0 (an invalid fan dispatches nothing)", n)
	}
	assertJournalPairsEqual(t, inRes.Events, r.Run.Events)
}

// TestForEachInRunBodyForgedOverRefusesWithProvenance (§2.13 enqueue gate) proves a
// for-each-in-run-body with a FORGED (delimiter-bearing) `over` ref refuses at the
// lower-time dry run, wrapped in the repeat run-body provenance — the same pre-effect
// gate the enqueue path (buildUnits) fails loudly on.
func TestForEachInRunBodyForgedOverRefusesWithProvenance(t *testing.T) {
	sub := subDoc("reviewer", arrField("arr"),
		forEachNode(nil, "item", "continue", refOver("a/b"),
			execNode("mem", `echo 1`, nil)))
	doc := decodeIR(t, bundleDoc(
		arrField("items"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "reviewer", `[`+envField("arr", "items")+`]`),
			runCondPassOrIter()),
		sub))
	_, err := engine.Run(context.Background(), newStore(t), doc, map[string]any{"items": []any{"a"}})
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") || !strings.Contains(err.Error(), "must not contain") {
		t.Fatalf("err = %v, want a repeat run-body dry-run refusal (does not lower / over-ref must not contain)", err)
	}
}

// TestForEachInRunBodySynthOverRefusesWithProvenance (§2.13 enqueue gate, synth variant)
// proves a for-each-in-run-body whose `over` refs a sibling guard's SYNTHESIZED then id
// refuses at the lower-time dry run with the repeat provenance wrap — the synth-body ban
// fires inside mintRunBodyAttempt's resolveDeps, wrapped by lowerLoop.
func TestForEachInRunBodySynthOverRefusesWithProvenance(t *testing.T) {
	sub := subDoc("reviewer", strField("note"),
		guardExecAfter("g", nil, condEqualRaw("note", `"x"`), "gthen", `echo t`)+","+
			forEachNode(nil, "item", "continue", refOver("gthen"),
				execNode("mem", `echo 1`, nil)))
	doc := decodeIR(t, bundleDoc(
		strField("who"),
		repeatRunLoop(nil,
			runNodeRawEnv("stage", nil, "reviewer", `[`+envField("note", "who")+`]`),
			runCondPassOrIter()),
		sub))
	_, err := engine.Run(context.Background(), newStore(t), doc, map[string]any{"who": "x"})
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") ||
		!strings.Contains(err.Error(), "synthesized decision body") || !strings.Contains(err.Error(), "gthen") {
		t.Fatalf("err = %v, want a repeat run-body dry-run refusal (does not lower / synth over ref gthen)", err)
	}
}

// TestAdvanceForEachInSubFormulaFansAndParks (§2.1 advance/pool) proves a pool-do
// for-each inside a run sub-formula dispatches one work bead per element at the
// namespaced member activations and parks, each prompt rendered against the sub-scope.
func TestAdvanceForEachInSubFormulaFansAndParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	sub := subDoc("reviewer", arrField("arr")+","+strField("label"),
		forEachNode(nil, "item", "continue", refOver("arr"),
			doNode("mem", "review {{ item }} for {{ label }}", nil)))
	env := `[` + envField("arr", "items") + `,` + envField("label", "tag") + `]`
	doc := decodeIR(t, bundleDoc(
		arrField("items")+","+strField("tag"),
		runNodeRawEnv("stage", nil, "reviewer", env),
		sub))
	res, err := engine.Advance(ctx, store, doc, "gcg-fis-fan", map[string]any{"items": []any{"a", "b"}, "tag": "release"}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || !res.Parked {
		t.Fatalf("advance = %+v, want Parked (both ns members dispatched)", res)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("dispatch count = %d, want 2 (one per element)", fake.dispatchCount())
	}
	if got := fake.dispatchPromptFor(t, "stage/fan/0:0"); got != "review a for release" {
		t.Errorf("member 0 prompt = %q, want %q (rendered against the sub-scope)", got, "review a for release")
	}
	if got := fake.dispatchPromptFor(t, "stage/fan/1:0"); got != "review b for release" {
		t.Errorf("member 1 prompt = %q, want %q", got, "review b for release")
	}
}
