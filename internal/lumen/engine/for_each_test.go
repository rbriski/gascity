package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// refOver renders a bare `ref` over-expression (a node output / flattened input
// holding a JSON array) — the gascity-authored convention (cf. dispatch subject).
func refOver(name string) string {
	return `{"kind":"ref","name":"` + name + `","origin":{"uri":"t","line":1,"col":0}}`
}

// memberOver renders the upstream conformance-golden `input.<field>` member
// over-expression (for-each-array.ir.json).
func memberOver(field string) string {
	return `{"kind":"member","base":{"kind":"ref","name":"input","origin":{"uri":"t","line":1,"col":0}},"name":"` + field + `"}`
}

// forEachNode renders a scatter(form:each) node with id "fan": binder over `over`,
// body a block wrapping the given member node(s), with the given on_fail.
func forEachNode(after []string, binder, onFail, over string, bodyMembers ...string) string {
	afterJSON := "[]"
	if len(after) > 0 {
		afterJSON = `["` + strings.Join(after, `","`) + `"]`
	}
	return `{
      "kind":"scatter","id":"fan","name":"fan","after":` + afterJSON + `,
      "origin":{"uri":"t","line":1,"col":0},
      "form":"each","binder":"` + binder + `",
      "over":` + over + `,
      "body":{"kind":"block","id":"fan.body","after":[],"origin":{"uri":"t","line":1,"col":0},
              "members":[` + strings.Join(bodyMembers, ",") + `]},
      "on_fail":"` + onFail + `"}`
}

// TestForEachRunsElementsBinderBound proves a for-each over a 2-element array runs
// one member per element (namespaced forEachID/<i>), with the binder bound into each
// member's render, and the aggregate settles pass.
func TestForEachRunsElementsBinderBound(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("fe1",
		forEachNode(nil, "item", "continue", refOver("items"),
			execNode("mem", `echo "x={{ item }}"`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["fan/0"]; got != "x=a" {
		t.Errorf("member 0 output = %q, want %q (binder bound to element 0)", got, "x=a")
	}
	if got := res.NodeOutputs["fan/1"]; got != "x=b" {
		t.Errorf("member 1 output = %q, want %q (binder bound to element 1)", got, "x=b")
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "fan", engine.OutcomePass)
}

// TestForEachMemberInputOverForm proves the upstream `input.<field>` member
// over-form resolves the array identically to the bare-ref form.
func TestForEachMemberInputOverForm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("fem",
		forEachNode(nil, "item", "continue", memberOver("items"),
			execNode("mem", `echo "{{ item }}"`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["fan/0"]; got != "a" {
		t.Errorf("member 0 = %q, want a (input.items member-over)", got)
	}
	if got := res.NodeOutputs["fan/1"]; got != "b" {
		t.Errorf("member 1 = %q, want b (input.items member-over)", got)
	}
}

// TestForEachEmptyArrayIsPass proves an empty `over` array settles the aggregate PASS
// (vacuous success — not skipped, not degraded) with no members.
func TestForEachEmptyArrayIsPass(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("fe0",
		forEachNode(nil, "item", "continue", refOver("items"),
			execNode("mem", `echo "{{ item }}"`, nil)),
		execNode("after", `echo "done: {{ fan }}"`, []string{"fan"}),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass (empty for-each)", res.Outcome)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "fan", engine.OutcomePass)
	assertSettled(t, settled, "after", engine.OutcomePass) // downstream ran (not skip-cascaded)
	if got := res.NodeOutputs["fan/0"]; got != "" {
		t.Errorf("member 0 = %q, want empty (no members for empty array)", got)
	}
}

// TestForEachMemberFailDegradesContinue proves on_fail:continue drains a failed
// member into the aggregate as degraded (scatter parity), and the run does NOT flip
// to failed (members are parented under the aggregate, out of the run outcome).
func TestForEachMemberFailDegradesContinue(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fail := `if [ "{{ item }}" = "bad" ]; then exit 1; fi; echo ok`
	doc := decodeIR(t, blockDoc("fed",
		forEachNode(nil, "item", "continue", refOver("items"),
			execNode("mem", fail, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"ok", "bad"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "fan", engine.OutcomeDegraded)
}

// TestForEachMemberFailStop proves on_fail:stop fails the aggregate when any member
// fails.
func TestForEachMemberFailStop(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fail := `if [ "{{ item }}" = "bad" ]; then exit 1; fi; echo ok`
	doc := decodeIR(t, blockDoc("fes",
		forEachNode(nil, "item", "stop", refOver("items"),
			execNode("mem", fail, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"ok", "bad"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "fan", engine.OutcomeFailed)
}

// TestForEachSkipCascade proves a for-each gated on a failed `after` dep settles
// skipped and mints NO members (over is never evaluated).
func TestForEachSkipCascade(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("fesc",
		execNode("gate", `exit 1`, nil),
		forEachNode([]string{"gate"}, "item", "continue", refOver("items"),
			execNode("mem", `echo "{{ item }}"`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "fan", engine.OutcomeSkipped)
	if got := res.NodeOutputs["fan/0"]; got != "" {
		t.Errorf("member 0 = %q, want empty (skip-cascaded for-each mints nothing)", got)
	}
}

// TestForEachOverNodeOutputGated proves a for-each whose `over` is a bare ref to an
// UPSTREAM node's output (a JSON array) gates on that node — the array is fixed
// before fan-out — and fans correctly once it settles.
func TestForEachOverNodeOutputGated(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("feg",
		execNode("up", `printf '["p","q"]'`, nil),
		forEachNode(nil, "item", "continue", refOver("up"),
			execNode("mem", `echo "{{ item }}"`, []string{"up"})),
	))
	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["fan/0"]; got != "p" {
		t.Errorf("member 0 = %q, want p (over a node output)", got)
	}
	if got := res.NodeOutputs["fan/1"]; got != "q" {
		t.Errorf("member 1 = %q, want q (over a node output)", got)
	}
}

// TestForEachDropRefoldByteIdentity pins DET: the dynamically-materialized member
// rows refold byte-identically.
func TestForEachDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, blockDoc("fedr",
		forEachNode(nil, "item", "continue", refOver("items"),
			execNode("mem", `echo "{{ item }}"`, nil)),
	))
	res, err := engine.Run(ctx, store, doc, map[string]any{"items": []any{"a", "b", "c"}})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
}

// litArrayNode renders a silent lit node holding an array literal (an authored
// `let`-bound array), for the silent-over-ref refusal.
func litArrayNode(id string, elems ...string) string {
	quoted := make([]string, len(elems))
	for i, e := range elems {
		quoted[i] = `{"kind":"literal","value":"` + e + `"}`
	}
	return `{"kind":"lit","id":"` + id + `","name":"` + id + `","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},` +
		`"value":{"kind":"array","elements":[` + strings.Join(quoted, ",") + `]}}`
}

// TestForEachLoweringRefusals pins the refused shapes (loud at load, never a runtime
// surprise): the body shape (multi-member / non-leaf), a missing binder/over, an
// unsupported over kind, a binder with a reserved delimiter, a for-each nested in an
// aggregate, and a for-each over a silent (lit/interp) node.
func TestForEachLoweringRefusals(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name  string
		nodes []string
	}{
		{
			name: "multi-member body",
			nodes: []string{forEachNode(nil, "item", "continue", refOver("items"),
				execNode("a", `echo 1`, nil), execNode("b", `echo 2`, nil))},
		},
		{
			name: "non-leaf body member",
			nodes: []string{forEachNode(nil, "item", "continue", refOver("items"),
				scatterNode("inner", nil, "continue", execNode("x", `echo 1`, nil)))},
		},
		{
			name: "missing binder",
			nodes: []string{forEachNode(nil, "", "continue", refOver("items"),
				execNode("mem", `echo 1`, nil))},
		},
		{
			name: "binder with reserved delimiter",
			nodes: []string{forEachNode(nil, "a/b", "continue", refOver("items"),
				execNode("mem", `echo 1`, nil))},
		},
		{
			name: "missing over",
			nodes: []string{`{"kind":"scatter","id":"fan","name":"fan","after":[],` +
				`"origin":{"uri":"t","line":1,"col":0},"form":"each","binder":"item",` +
				`"body":{"kind":"block","id":"fan.body","after":[],"origin":{"uri":"t","line":1,"col":0},` +
				`"members":[` + execNode("mem", `echo 1`, nil) + `]},"on_fail":"continue"}`},
		},
		{
			name: "unsupported over kind",
			nodes: []string{forEachNode(nil, "item", "continue",
				`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"a"},{"kind":"literal","value":1}]}`,
				execNode("mem", `echo 1`, nil))},
		},
		{
			name: "nested in an aggregate",
			nodes: []string{scatterNode("outer", nil, "continue",
				forEachNode(nil, "item", "continue", refOver("items"), execNode("mem", `echo 1`, nil)))},
		},
		{
			name: "over a silent node",
			nodes: []string{
				litArrayNode("items", "a", "b"),
				forEachNode(nil, "item", "continue", refOver("items"), execNode("mem", `echo 1`, nil)),
			},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeIR(t, blockDoc("ferf", tc.nodes...))
			_, err := engine.Run(ctx, newStore(t), doc, map[string]any{"items": []any{"a"}})
			if !errors.Is(err, engine.ErrUnsupportedNode) {
				t.Fatalf("run err = %v, want ErrUnsupportedNode", err)
			}
		})
	}
}

// --- Advance (pool) ---

// TestAdvanceForEachFansOutAndParks proves a pool-do for-each dispatches ONE work
// bead per array element concurrently and parks on them.
func TestAdvanceForEachFansOutAndParks(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("afe",
		forEachNode(nil, "item", "continue", refOver("items"),
			doNode("mem", "review {{ item }}", nil)),
	))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-foreach", map[string]any{"items": []any{"a", "b"}}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || !res.Parked {
		t.Fatalf("advance = %+v, want Parked (both members dispatched, awaited)", res)
	}
	if len(res.InFlight) != 2 {
		t.Fatalf("InFlight = %+v, want 2 members fanned out", res.InFlight)
	}
	if fake.dispatchCount() != 2 {
		t.Fatalf("DispatchWork called %d times, want 2 (one per element)", fake.dispatchCount())
	}
}

// TestAdvanceForEachSealsWhenMembersClose proves the aggregate seals pass once every
// fanned member's work bead closes.
func TestAdvanceForEachSealsWhenMembersClose(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("afs",
		forEachNode(nil, "item", "continue", refOver("items"),
			doNode("mem", "review {{ item }}", nil)),
	))
	opts := fake.opts()
	if _, err := engine.Advance(ctx, store, doc, "gcg-run-fes", map[string]any{"items": []any{"a", "b"}}, opts); err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	fake.settleAllDispatchedPass()
	res, err := engine.Advance(ctx, store, doc, "gcg-run-fes", map[string]any{"items": []any{"a", "b"}}, opts)
	if err != nil {
		t.Fatalf("advance 2: %v", err)
	}
	if !res.Sealed {
		t.Fatalf("advance to seal = %+v, want Sealed", res)
	}
	settled := settledIDs(t, res.Run.Events)
	assertSettled(t, settled, "fan", engine.OutcomePass)
}

// TestAdvanceForEachEmptyArraySeals proves an empty pool for-each seals pass in one
// pass with zero dispatches.
func TestAdvanceForEachEmptyArraySeals(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, blockDoc("afe0",
		forEachNode(nil, "item", "continue", refOver("items"),
			doNode("mem", "review {{ item }}", nil)),
	))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-fe0", map[string]any{"items": []any{}}, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Sealed {
		t.Fatalf("advance = %+v, want Sealed (empty for-each seals immediately)", res)
	}
	if fake.dispatchCount() != 0 {
		t.Fatalf("DispatchWork called %d times, want 0", fake.dispatchCount())
	}
}
