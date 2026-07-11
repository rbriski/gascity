package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// TestScatterRunDoFixtureLowers guards the scatter-run dolt-e2e bundle fixture: a
// scatter { direct(do), extra(run -> subwork{inner(do)}) } then wrap(do) decodes and
// lowers (allowDo) so a fixture typo fails fast here, not 300s into the e2e. It pins the
// member-collection shape the e2e asserts: the scatter member set is {direct, extra}
// (the transparent run agg is the member; its namespaced sub-do extra/inner is excluded).
func TestScatterRunDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "scatter-run-do.lumen.json")
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
		t.Fatalf("lower scatter-run-do: %v", err)
	}
	if unitByNode(units, "extra/inner") == nil {
		t.Errorf("no namespaced sub-do extra/inner; got %v", nodeIDs(units))
	}
	agg := unitByNode(units, "lanes")
	if agg == nil || agg.kind != unitScatterAgg {
		t.Fatalf("no scatter aggregate lanes; got %v", nodeIDs(units))
	}
	if len(agg.members) != 2 || !containsStr(agg.members, "direct:0") || !containsStr(agg.members, "extra:0") {
		t.Errorf("scatter members = %v, want [direct:0 extra:0] (run agg is the member, sub-do excluded)", agg.members)
	}
	if containsStr(agg.members, "extra/inner:0") {
		t.Errorf("scatter members = %v must NOT include the run's sub-do extra/inner:0", agg.members)
	}
	if wrap := unitByNode(units, "wrap"); wrap == nil || !containsStr(wrap.afterDeps, "lanes:0") {
		t.Errorf("wrap afterDeps = %v, want to gate on lanes:0 (downstream of the scatter)", deref(wrap).afterDeps)
	}
}

// gatherOverCombine renders a gather(authored) over `over` with a single combine member
// (a raw node JSON) — the internal-package peer of the external gatherNode helper, for
// the run-in-combine refusal pins.
func gatherOverCombine(id, over, combineMember string) string {
	return `{"kind":"gather","id":"` + id + `","name":"` + id + `","after":[],` +
		`"over":{"kind":"ref","name":"` + over + `"},` +
		`"combine":{"kind":"authored","block":{"kind":"block","id":"` + id + `.body","after":[],` +
		`"members":[` + combineMember + `]}}}`
}

// TestLowerScatterRunMemberH1GatePropagates pins that the scatter's own `after` gate is
// propagated onto the run member's INLINED sub-units (H1): a gated-off scatter must
// skip-cascade the whole member run, not just the run aggregate. The scatter gates on
// `prep`, so the run's sub-unit extra/hello inherits prep:0 in its afterDeps.
func TestLowerScatterRunMemberH1GatePropagates(t *testing.T) {
	scatterWithRun := `{"kind":"scatter","id":"s","name":"s","after":["prep"],"form":"members",` +
		`"members":[` + runNode("extra", nil, "greeter", "name", "who") + `]}`
	doc := decodeBundle(t, runMainDoc(
		execNode("prep", nil, "echo p")+","+scatterWithRun,
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	sub := unitByNode(units, "extra/hello")
	if sub == nil {
		t.Fatalf("no extra/hello unit; got %v", nodeIDs(units))
	}
	if !containsStr(sub.afterDeps, "prep:0") {
		t.Errorf("extra/hello afterDeps = %v, want to include prep:0 (scatter H1 gate propagated onto the run's sub-graph)", sub.afterDeps)
	}
}

// TestLowerScatterMemberRunResetAllowsNestedRun pins the inAggregate reset (§1): a
// scatter-member run whose sub-formula's OWN top-level statement is another run lowers.
// The reset (inAggregate=false around the inlined sub-graph) keeps the sub-formula's own
// nested run/loop legal — the sub-graph statements are not aggregate members.
func TestLowerScatterMemberRunResetAllowsNestedRun(t *testing.T) {
	scatterWithRun := `{"kind":"scatter","id":"s","name":"s","after":[],"form":"members",` +
		`"members":[` + runNode("outer", nil, "mid", "name", "who") + `]}`
	doc := decodeBundle(t, runMainDoc(
		scatterWithRun,
		greeterFormula("mid", runNode("inner", nil, "leaf", "name", "name"))+","+
			greeterFormula("leaf", execNode("do", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a nested run at a scatter-member run's sub-formula top level: %v", err)
	}
	if unitByNode(units, "outer/inner/do") == nil {
		t.Errorf("want the doubly-nested outer/inner/do; got %v", nodeIDs(units))
	}
}

// TestLowerScatterRunMemberInSubFormulaLowers pins ⚑SF-5: a scatter WITH a run member,
// itself INSIDE a run sub-formula, lowers — the exact shape the pilot enters after RBL
// (lowerScatter at prefix!="" sets inAggregate=true, then lowerRun is entered with BOTH
// prefix!="" AND inAggregate=true, which ⚑SF-2 makes legal). Before the slice, lowerRun's
// inAggregate check refused this.
func TestLowerScatterRunMemberInSubFormulaLowers(t *testing.T) {
	// main: run R -> formula F. F: scatter { do d, run M -> formula G }.
	subScatter := `{"kind":"scatter","id":"sc","name":"sc","after":[],"form":"members",` +
		`"members":[` + execNode("d", nil, "echo d") + `,` +
		runNode("m", nil, "leaf", "name", "name") + `]}`
	doc := decodeBundle(t, runMainDoc(
		runNode("R", nil, "mid", "name", "who"),
		`"mid":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},`+
			`"name":"mid","input":{"name":"mid.input","fields":[{"name":"name","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},`+
			`"nodes":[`+subScatter+`]}`+","+
			greeterFormula("leaf", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a scatter-with-a-run-member inside a sub-formula: %v", err)
	}
	// The doubly-namespaced run member's sub-unit lowered: R/m/hello.
	if unitByNode(units, "R/m/hello") == nil {
		t.Errorf("want R/m/hello (run member inside a sub-formula scatter); got %v", nodeIDs(units))
	}
	// The sub-scatter aggregates the run member R/m (parent = the sub-scatter act).
	agg := unitByNode(units, "R/sc")
	if agg == nil || agg.kind != unitScatterAgg {
		t.Fatalf("R/sc = %+v, want a scatter aggregate", agg)
	}
	if !containsStr(agg.members, "R/m:0") || !containsStr(agg.members, "R/d:0") {
		t.Errorf("sub-scatter members = %v, want R/d:0 and R/m:0", agg.members)
	}
}

// TestLowerLoopInScatterInSubFormulaRefused pins the double-fence proof: a LOOP nested
// under a scatter that is itself inside a run sub-formula still REFUSES — via the prefix
// fence in lowerLoop (loops are top-level or a top-level scatter member only). The
// inAggregate reset does not un-fence it; the prefix fence holds.
func TestLowerLoopInScatterInSubFormulaRefused(t *testing.T) {
	subScatter := `{"kind":"scatter","id":"sc","name":"sc","after":[],"form":"members",` +
		`"members":[` + retryMember("r1", "b1", "echo hi") + `]}`
	doc := decodeBundle(t, runMainDoc(
		runNode("R", nil, "mid", "name", "who"),
		`"mid":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},`+
			`"name":"mid","input":{"name":"mid.input","fields":[{"name":"name","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},`+
			`"nodes":[`+subScatter+`]}`,
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("want a loop-in-sub-formula prefix-fence refusal, got %v", err)
	}
}

// TestLowerRunInCombineRefusedLeafOnly pins that a run placed in a gather COMBINE is
// still refused — now SOLELY by lowerCombine's leaf-only sweep (⚑SF-2 deleted lowerRun's
// placement check). The combine run FULLY inlines (valid target, env validated) and is
// then rejected as a non-leaf combine member. Pins the exact leaf-only message.
func TestLowerRunInCombineRefusedLeafOnly(t *testing.T) {
	combineRun := runNode("cr", nil, "greeter", "name", "who")
	doc := decodeBundle(t, runMainDoc(
		scatterOf("lanes", execNode("m1", nil, "echo m"))+","+
			gatherOverCombine("G", "lanes", combineRun),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil {
		t.Fatal("want a run-in-combine refusal, got nil")
	}
	if !strings.Contains(err.Error(), "not executable in a combine block") {
		t.Fatalf("run-in-combine error = %v, want the leaf-only sweep message", err)
	}
	// The message names the run member and its kind.
	if !strings.Contains(err.Error(), "cr") || !strings.Contains(err.Error(), "run") {
		t.Errorf("run-in-combine error = %v, want it to name member cr and kind run", err)
	}
}

// TestLowerRunInCombineMissingTargetError pins ⚑SF-2 consequence (a): a combine run with
// a MISSING target now errors "targets formula … not present" (loud, but a DIFFERENT
// shape than a placement refusal) — the run inlines BEFORE the leaf-only sweep, so the
// missing-target check inside lowerRun fires first.
func TestLowerRunInCombineMissingTargetError(t *testing.T) {
	combineRun := runNode("cr", nil, "nonexistent", "name", "who")
	doc := decodeBundle(t, runMainDoc(
		scatterOf("lanes", execNode("m1", nil, "echo m"))+","+
			gatherOverCombine("G", "lanes", combineRun),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil {
		t.Fatal("want a missing-target refusal, got nil")
	}
	if !strings.Contains(err.Error(), "targets formula") || !strings.Contains(err.Error(), "not present") {
		t.Fatalf("combine missing-target error = %v, want the \"targets formula … not present\" shape", err)
	}
}

// TestLowerScatterRunSiblingEnvRefRefused pins ⚑SF-1: a run scatter member whose
// environment reads a SIBLING scatter member is refused loudly. Silently accepting it
// creates an intra-scatter ordering edge (members stop being concurrent; a failed
// sibling skip-cascades the run so on_fail=stop reports DEGRADED — a "stop" scatter that
// doesn't fail). The refusal names BOTH members.
func TestLowerScatterRunSiblingEnvRefRefused(t *testing.T) {
	// scatter { exec sibling, run extra (env name<-sibling) } — extra reads its sibling.
	scatterWithSiblingRef := `{"kind":"scatter","id":"s","name":"s","after":[],"form":"members",` +
		`"members":[` + execNode("sibling", nil, "echo s") + `,` +
		runNode("extra", nil, "greeter", "name", "sibling") + `]}`
	doc := decodeBundle(t, runMainDoc(
		scatterWithSiblingRef,
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil {
		t.Fatal("want a sibling-env-ref refusal, got nil")
	}
	if !strings.Contains(err.Error(), "extra") || !strings.Contains(err.Error(), "sibling") {
		t.Fatalf("sibling-env-ref error = %v, want it to name BOTH members (extra and sibling)", err)
	}
}

// TestLowerScatterRunSilentSiblingClosureRefused closes the ⚑SF-1 closure-hop bypass: a
// run member whose environment reads a SILENT (lit/interp) scatter sibling — which is
// excluded from the drained member set, so the pre-closure check misses — must still be
// refused when the silent sibling's non-silent closure lands on a REAL sibling member.
// Here `quiet` (a silent lit, after `sibling`) is the env ref; its closure substitutes
// sibling:0, which IS a member — exactly the intra-scatter ordering edge SF-1 refuses.
func TestLowerScatterRunSilentSiblingClosureRefused(t *testing.T) {
	scatterWithSilentHop := `{"kind":"scatter","id":"s","name":"s","after":[],"form":"members",` +
		`"members":[` + execNode("sibling", nil, "echo s") + `,` +
		`{"kind":"lit","id":"quiet","name":"quiet","after":["sibling"],"value":{"kind":"literal","value":"w"}},` +
		runNode("extra", nil, "greeter", "name", "quiet") + `]}`
	doc := decodeBundle(t, runMainDoc(
		scatterWithSilentHop,
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil {
		t.Fatal("want a sibling-env-ref refusal through the silent closure hop, got nil")
	}
	if !strings.Contains(err.Error(), "extra") || !strings.Contains(err.Error(), "sibling") {
		t.Fatalf("closure-hop sibling error = %v, want it to name BOTH members (extra and sibling)", err)
	}
}

// TestLowerScatterRunSiblingEnvRefRefusedInSubFormula pins ⚑SF-1 at DEPTH: the sibling
// refusal fires for a scatter INSIDE a run sub-formula too — the scatterMembers
// registration (l.qid(n.ID) = "R/sc") and the lookup (activationNodeID(u.parent) from
// parent "R/sc:0") round-trip on the QUALIFIED key. A future de-qualification of either
// side would silently disable the depth case with nothing red; this keeps it pinned.
func TestLowerScatterRunSiblingEnvRefRefusedInSubFormula(t *testing.T) {
	subScatter := `{"kind":"scatter","id":"sc","name":"sc","after":[],"form":"members",` +
		`"members":[` + execNode("sibling", nil, "echo s") + `,` +
		runNode("m", nil, "leaf", "name", "sibling") + `]}`
	doc := decodeBundle(t, runMainDoc(
		runNode("R", nil, "mid", "name", "who"),
		`"mid":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},`+
			`"name":"mid","input":{"name":"mid.input","fields":[{"name":"name","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},`+
			`"nodes":[`+subScatter+`]}`+","+
			greeterFormula("leaf", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil {
		t.Fatal("want a sibling-env-ref refusal inside the sub-formula, got nil")
	}
	if !strings.Contains(err.Error(), "R/m") || !strings.Contains(err.Error(), "R/sibling") {
		t.Fatalf("depth sibling error = %v, want it to name the QUALIFIED members (R/m and R/sibling)", err)
	}
}

// TestLowerScatterRunOutsideNodeEnvRefGates pins the ⚑SF-1 counter-case: a run scatter
// member whose environment reads an OUTSIDE node (not a sibling member) is ALLOWED and
// creates a legitimate env-ref gate on that node. This is the shape the skipped-member
// degraded runtime test relies on (an env gate on a failed node OUTSIDE the scatter).
func TestLowerScatterRunOutsideNodeEnvRefGates(t *testing.T) {
	scatterWithRun := `{"kind":"scatter","id":"s","name":"s","after":[],"form":"members",` +
		`"members":[` + runNode("extra", nil, "greeter", "name", "outside") + `]}`
	doc := decodeBundle(t, runMainDoc(
		execNode("outside", nil, "echo o")+","+scatterWithRun,
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused a run reading an OUTSIDE node (should gate, not refuse): %v", err)
	}
	// The run agg and its sub-unit gate on the outside node.
	extra := unitByNode(units, "extra")
	if extra == nil || !containsStr(extra.afterDeps, "outside:0") {
		t.Errorf("extra afterDeps = %v, want to include outside:0 (env-ref gate)", deref(extra).afterDeps)
	}
	sub := unitByNode(units, "extra/hello")
	if sub == nil || !containsStr(sub.afterDeps, "outside:0") {
		t.Errorf("extra/hello afterDeps = %v, want to include outside:0 (env-ref gate on the whole sub-graph)", deref(sub).afterDeps)
	}
}
