package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// TestRepeatRunDoFixtureLowers guards the hand-authored dolt-e2e bundle fixture:
// `repeat { run stage -> greeter{do hello} } until stage.outcome==pass || iteration>=2`
// decodes and lowers (allowDo), so a fixture typo fails fast here — not 10min into the
// e2e. It also confirms the DRY-RUN mint accepts the pool-mode gate (allowDo=true,
// allowCombineDo=false — the exact controller-loop flags EnqueueRun pre-validates with).
func TestRepeatRunDoFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "repeat-run-do.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	// Both the inline (allowDo,allowCombineDo) and the controller-loop pool
	// (allowDo, !allowCombineDo) flag pairs must lower — the latter is what EnqueueRun
	// pre-validates the run body through.
	for _, combineDo := range []bool{true, false} {
		units, err := buildUnits(doc, true, combineDo)
		if err != nil {
			t.Fatalf("lower repeat-run-do (allowCombineDo=%v): %v", combineDo, err)
		}
		loop := unitByNode(units, "loop")
		if loop == nil || loop.loop == nil || loop.loop.bodyRun == nil {
			t.Fatalf("no repeat-run-body loop unit; got %v", nodeIDs(units))
		}
		if loop.loop.bodyNodeID != "stage" {
			t.Errorf("body node id = %q, want stage", loop.loop.bodyNodeID)
		}
	}
}

// --- repeat-run-body (RBL) lowering fixtures --------------------------------

// repeatRunNode builds an ungated repeat loop (fixed id "loop") whose body is a
// run node (the RBL shape): runBody keeps its own id, cond is the closed exit expr.
func repeatRunNode(runBody, cond string) string {
	return `{"kind":"repeat","id":"loop","name":"loop","after":[]` +
		`,"body":` + runBody + `,"cond":` + cond + `,"iterationName":"iteration"}`
}

// retryRunNode builds a retry loop whose body is a run node (⚑S2 refusal fixture).
func retryRunNode(loopID string, runBody string) string {
	return `{"kind":"retry","id":"` + loopID + `","name":"` + loopID + `","after":[],` +
		`"attempts":{"kind":"literal","value":2},"body":` + runBody + `}`
}

// repeatRunCondPassOrIter is the canonical RBL cond `stage.outcome == "pass" ||
// iteration >= 2` (the pilot/gascity-port main-loop shape over the "stage" body).
func repeatRunCondPassOrIter() string {
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[` +
		`{"kind":"ref","name":"stage","field":"outcome"},` +
		`{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[` +
		`{"kind":"ref","name":"iteration"},{"kind":"literal","value":2}]}]}`
}

// TestLowerRepeatRunBodyStashesSpec pins the core RBL lowering shape: a repeat whose
// body is a run lowers to a single unitLoop carrying bodyRun (+ the re-lowering
// context) and bodyIRKind==NodeRun, and NO sub-units are emitted at the top level
// (the sub-graph is minted per attempt at run time). The body node id is the run
// node's id.
func TestLowerRepeatRunBodyStashesSpec(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode(runNode("stage", nil, "greeter", "name", "who"),
			repeatRunCondPassOrIter()),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	loop := unitByNode(units, "loop")
	if loop == nil || loop.kind != unitLoop {
		t.Fatalf("loop = %+v, want a unitLoop", loop)
	}
	if loop.loop == nil || loop.loop.bodyRun == nil {
		t.Fatalf("loop spec bodyRun = %+v, want the decoded run spec", loop.loop)
	}
	if loop.loop.bodyNodeID != "stage" || loop.loop.bodyIRKind != ir.NodeRun {
		t.Errorf("loop body = {%q, %q}, want {stage, run}", loop.loop.bodyNodeID, loop.loop.bodyIRKind)
	}
	if loop.loop.bodyRun.target != "greeter" {
		t.Errorf("bodyRun target = %q, want greeter", loop.loop.bodyRun.target)
	}
	// The sub-graph is minted at run time, not a top-level unit: no stage/0/hello here.
	if unitByNode(units, "stage/0/hello") != nil {
		t.Errorf("attempt sub-unit stage/0/hello must NOT be a top-level unit; got %v", nodeIDs(units))
	}
	// The re-lowering context is captured (bodyFormula + bundle).
	if loop.loop.bodyFormula == nil || len(loop.loop.bodyFormula.Nodes) != 1 {
		t.Errorf("bodyFormula = %+v, want the greeter sub-formula", loop.loop.bodyFormula)
	}
}

// TestLowerRetryRunBodyRefused pins ⚑S2: a retry whose body is a run is refused loudly
// (a transparent aggregate is never retryable, so it could never re-attempt).
func TestLowerRetryRunBodyRefused(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		retryRunNode("loop", runNode("stage", nil, "greeter", "name", "who")),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "cannot re-attempt") || !strings.Contains(err.Error(), "repeat") {
		t.Fatalf("want a retry+run-body refusal naming repeat, got %v", err)
	}
}

// TestLowerRepeatRunBodyEnvRefIterationRefused pins ⚑S5: an env binding that reads the
// repeat's iteration counter is refused (silent path-dependent render on re-render).
func TestLowerRepeatRunBodyEnvRefIterationRefused(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode( // bind name <- iteration (the loop's own counter) — refused.
			runNode("stage", nil, "greeter", "name", "iteration"),
			repeatRunCondPassOrIter()),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "iteration") {
		t.Fatalf("want an env-ref-to-iteration refusal, got %v", err)
	}
}

// TestLowerRepeatRunBodyInScatterRefused pins the scatter-member refusal: a repeat-run-
// body loop nested as a scatter member is refused (an entry-top-level surface this
// slice; the re-mint arm under an aggregate is untested).
func TestLowerRepeatRunBodyInScatterRefused(t *testing.T) {
	loopMember := repeatRunNode(runNode("stage", nil, "greeter", "name", "who"),
		repeatRunCondPassOrIter())
	doc := decodeBundle(t, runMainDoc(
		scatterOf("lanes", nil, loopMember),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "scatter member") {
		t.Fatalf("want a loop-with-run-body-in-scatter refusal, got %v", err)
	}
}

// TestLowerRepeatRunBodyDecodeInheritance pins that the run-body arm inherits every
// decodeRunNode refusal (shared path, no drift): a missing target, a recursive cycle,
// and a delimiter-bearing env ref all refuse at lowering.
func TestLowerRepeatRunBodyDecodeInheritance(t *testing.T) {
	cond := repeatRunCondPassOrIter()
	cases := []struct {
		name    string
		runBody string
		sub     string
		want    string
	}{
		{
			name:    "missing target",
			runBody: runNode("stage", nil, "nonexistent", "name", "who"),
			sub:     greeterFormula("greeter", execNode("hello", nil, "echo hi")),
			want:    "not present",
		},
		{
			name:    "charset env ref",
			runBody: runNode("stage", nil, "greeter", "name", "a/b"),
			sub:     greeterFormula("greeter", execNode("hello", nil, "echo hi")),
			want:    "reserved delimiters",
		},
		{
			name: "recursive cycle",
			// greeter's own body runs greeter again -> cycle refused at the DRY-RUN mint.
			runBody: runNode("stage", nil, "greeter", "name", "who"),
			sub: `"greeter":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
				`"name":"greeter","input":{"name":"greeter.input","fields":[{"name":"name","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
				`"nodes":[` + runNode("again", nil, "greeter", "name", "name") + `]}`,
			want: "cycle",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeBundle(t, runMainDoc(repeatRunNode(tc.runBody, cond), tc.sub))
			_, err := buildUnits(doc, true, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a %q refusal containing %q, got %v", tc.name, tc.want, err)
			}
		})
	}
}

// TestLowerRepeatRunBodyNestedLoopRefusedAtDryRun pins the ⚑S4 dry-run: a run body whose
// sub-formula contains a LOOP does not lower — the attempt-prefix mint hits the
// prefix-fence in lowerLoop ("top-level only") and buildUnits refuses, so EnqueueRun
// refuses before seeding a run.
func TestLowerRepeatRunBodyNestedLoopRefusedAtDryRun(t *testing.T) {
	sub := `"greeter":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"greeter","input":{"name":"greeter.input","fields":[]},"nodes":[` +
		retryMember("r1", "b1", "echo hi") + `]}`
	runNoEnv := `{"kind":"run","id":"stage","name":"stage","after":[],` +
		`"target":{"kind":"by-name","name":"greeter"},"environment":{"fields":[]},"outcome":"transparent"}`
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode(runNoEnv, repeatRunCondPassOrIter()),
		sub,
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "does not lower") || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("want a dry-run refusal (does not lower / top-level), got %v", err)
	}
}

// TestLowerRepeatRunBodyEnvRefGatesLoop pins ⚑S6: a repeat run body whose environment
// reads a parent NODE gates the LOOP on that node (the sub-scope is frozen before the
// first attempt mints). An env ref to an INPUT is not a gate.
func TestLowerRepeatRunBodyEnvRefGatesLoop(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		execNode("prep", nil, "echo p")+","+
			repeatRunNode(runNode("stage", nil, "greeter", "name", "prep"), // env reads node prep
				repeatRunCondPassOrIter()),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	loop := unitByNode(units, "loop")
	if loop == nil || !containsStr(loop.afterDeps, "prep:0") {
		t.Errorf("loop afterDeps = %v, want to include prep:0 (env-ref gate)", deref(loop).afterDeps)
	}
}

// TestMintRunBodyAttemptAttemptInvariant pins that the per-attempt mint is attempt-
// invariant modulo the prefix (⚑S4/⚑S1): attempts 0 and 1 produce the same unit shapes
// under distinct `<bodyNodeID>/<N>/` namespaces, and the attempt aggregate settles at
// bodyNodeID:N, parented under the loop activation, with the sub-node as its member.
func TestMintRunBodyAttemptAttemptInvariant(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode(runNode("stage", nil, "greeter", "name", "who"),
			repeatRunCondPassOrIter()),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	spec := unitByNode(units, "loop").loop

	for _, attempt := range []int{0, 1, 7} {
		sub, agg, err := spec.mintRunBodyAttempt(attempt, "loop:0", "", nil, nil)
		if err != nil {
			t.Fatalf("mint attempt %d: %v", attempt, err)
		}
		wantSubNode := "stage/" + itoa(attempt) + "/hello"
		if len(sub) != 1 || sub[0].nodeID != wantSubNode {
			t.Fatalf("attempt %d sub-units = %v, want [%s]", attempt, nodeIDs(sub), wantSubNode)
		}
		if sub[0].ns != "stage/"+itoa(attempt)+"/" {
			t.Errorf("attempt %d sub ns = %q, want stage/%d/", attempt, sub[0].ns, attempt)
		}
		wantAgg := "stage:" + itoa(attempt)
		if agg.activation != wantAgg || agg.nodeID != "stage" || agg.irKind != ir.NodeRun {
			t.Errorf("attempt %d agg = {%q %q %q}, want {%s stage run}", attempt, agg.activation, agg.nodeID, agg.irKind, wantAgg)
		}
		if agg.parent != "loop:0" {
			t.Errorf("attempt %d agg parent = %q, want loop:0 (out of runOutcome)", attempt, agg.parent)
		}
		if len(agg.members) != 1 || agg.members[0] != wantSubNode+":0" {
			t.Errorf("attempt %d agg members = %v, want [%s:0]", attempt, agg.members, wantSubNode)
		}
		// The sub-node parents the attempt aggregate.
		if sub[0].parent != wantAgg {
			t.Errorf("attempt %d sub parent = %q, want %s", attempt, sub[0].parent, wantAgg)
		}
	}
}

// TestLowerRepeatRunBodyCondExternalNodeRefRefused pins the re-decide-flip fix (C1): a
// run-body repeat cond that reads an EXTERNAL node (`other.outcome`) is refused at
// lowering. advanceRunBodyLoop re-runs loopDecide on EVERY tick (the agg activates
// last per ⚑S1, so liveAttempt cannot park it), and loopDecide's scope resolves any
// settled bare node id — so an external ref settling BETWEEN ticks would flip an
// already-acted-on continue decision into a stale settle over a live minted attempt.
// The freeze makes decide(N) a pure function of (bn(N), N) — tick-stable.
func TestLowerRepeatRunBodyCondExternalNodeRefRefused(t *testing.T) {
	cond := `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"stage","field":"outcome"},{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"other","field":"outcome"},{"kind":"literal","value":"pass"}]}]}`
	doc := decodeBundle(t, runMainDoc(
		execNode("other", nil, "echo o")+","+
			repeatRunNode(runNode("stage", nil, "greeter", "name", "who"), cond),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "other") || !strings.Contains(err.Error(), "cond") {
		t.Fatalf("want a run-body cond external-node-ref refusal naming `other`, got %v", err)
	}
}

// TestLowerRepeatRunBodyCondAllowedRefsLower pins the freeze's ALLOWED set: the corpus
// shape (`<body>.outcome == "pass" || iteration >= cap`) must still lower, and so must a
// cond reading an INPUT field (inputs are immutable — tick-stable by construction).
func TestLowerRepeatRunBodyCondAllowedRefsLower(t *testing.T) {
	// body ref + iteration + the input field `who` — the full allowed set in one cond.
	cond := `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"stage","field":"outcome"},{"kind":"literal","value":"pass"}]},` +
		`{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"ref","name":"who"}]}]}`
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode(runNode("stage", nil, "greeter", "name", "who"), cond),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	if _, err := buildUnits(doc, true, true); err != nil {
		t.Fatalf("buildUnits refused an allowed cond (body + iteration + input refs): %v", err)
	}
}

// TestLowerRepeatRunBodyEnvSelfRefRefused pins the env self-ref refusal (the ⚑S5
// family): `given { name: stage }` — an env binding reading the body's OWN id — is
// refused. byNodeID misses the spec-only body id, so it would gate nothing and render
// "" on attempt 0 but attempt N-1's transparent output on attempt N ≥ 1 (the bare
// scope[bodyID] last-wins key) — a silently attempt-VARYING mint.
func TestLowerRepeatRunBodyEnvSelfRefRefused(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode(runNode("stage", nil, "greeter", "name", "stage"), // env reads the body's own id
			repeatRunCondPassOrIter()),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "own") || !strings.Contains(err.Error(), "stage") {
		t.Fatalf("want an env self-ref refusal naming the body id, got %v", err)
	}
}

// TestMintRunBodyAttemptMemberSourceOrderParity pins the member-collection order: for a
// sub-formula authored [b after a, a] (source ≠ topo), the mint's aggregate members must
// match lowerRun's SOURCE-order rule for the same formula — the Members payload and the
// "returns lastResult" output selection (last source-order ran member) must not diverge
// between a static inlined run and a re-minted attempt.
func TestMintRunBodyAttemptMemberSourceOrderParity(t *testing.T) {
	// greeter: [b after a, a] — source order b,a; topo order a,b.
	greeterNodes := execNode("b", []string{"a"}, "echo b") + "," + execNode("a", nil, "echo a")
	doc := decodeBundle(t, runMainDoc(
		runNode("sr", nil, "greeter", "name", "who")+","+
			repeatRunNode(runNode("stage", nil, "greeter", "name", "who"),
				repeatRunCondPassOrIter()),
		greeterFormula("greeter", greeterNodes),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	// The static run's members are SOURCE order (the lowerRun rule).
	sr := unitByNode(units, "sr")
	if sr == nil || len(sr.members) != 2 || sr.members[0] != "sr/b:0" || sr.members[1] != "sr/a:0" {
		t.Fatalf("static run members = %v, want [sr/b:0 sr/a:0] (source order)", deref(sr).members)
	}
	// The mint's members must match — source order, not topo order.
	spec := unitByNode(units, "loop").loop
	_, agg, err := spec.mintRunBodyAttempt(0, "loop:0", "", nil, nil)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(agg.members) != 2 || agg.members[0] != "stage/0/b:0" || agg.members[1] != "stage/0/a:0" {
		t.Fatalf("minted agg members = %v, want [stage/0/b:0 stage/0/a:0] (source order, lowerRun parity)", agg.members)
	}
}

// TestMintRunBodyAttemptPropagatesLoopGate pins ⚑S6 at mint time: the loop's afterDeps
// propagate onto every minted sub-unit AND the aggregate (fold-edge honesty).
func TestMintRunBodyAttemptPropagatesLoopGate(t *testing.T) {
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode(runNode("stage", nil, "greeter", "name", "who"),
			repeatRunCondPassOrIter()),
		greeterFormula("greeter", execNode("hello", nil, "echo hi")),
	))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	spec := unitByNode(units, "loop").loop
	sub, agg, err := spec.mintRunBodyAttempt(2, "loop:0", "", []string{"gate:0"}, []string{"gate"})
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if !containsStr(sub[0].afterDeps, "gate:0") {
		t.Errorf("sub afterDeps = %v, want the loop gate gate:0 propagated", sub[0].afterDeps)
	}
	if !containsStr(agg.afterDeps, "gate:0") {
		t.Errorf("agg afterDeps = %v, want the loop gate gate:0 propagated", agg.afterDeps)
	}
}

// itoa is a tiny int->string for building expected qualified ids in tests.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
