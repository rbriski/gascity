package engine

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// --- for-each-body=run (FBR) lowering fixtures ------------------------------

// envF renders one run env binding: <name> <- ref <ref>.
func envF(name, ref string) string {
	return `{"name":"` + name + `","value":{"kind":"expr","expr":{"kind":"ref","name":"` + ref + `"}}}`
}

// fbrRunMember renders the fan's single run-body member (id "lane") targeting a
// sub-formula with the given environment.fields.
func fbrRunMember(target, envFields string) string {
	return `{"kind":"run","id":"lane","name":"lane","after":[],` +
		`"target":{"kind":"by-name","name":"` + target + `"},` +
		`"environment":{"fields":[` + envFields + `]},"outcome":"transparent"}`
}

// fbrFanNode renders a run-bodied for-each (id "fanout", binder "reviewer") over the
// given over-expression, whose single body member is the given run node.
func fbrFanNode(over, runMember string) string {
	return `{"kind":"scatter","id":"fanout","name":"fanout","after":[],` +
		`"form":"each","binder":"reviewer","over":` + over +
		`,"body":{"kind":"block","id":"fanout.body","after":[],"members":[` + runMember + `]},` +
		`"on_fail":"continue"}`
}

// reviewLaneFormula renders the reviewLane sub-formula bundle entry (accepts reviewer +
// an optional target) with the given nodes.
func reviewLaneFormula(name, nodes string) string {
	return `"` + name + `":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"` + name + `","input":{"name":"` + name + `.input","fields":[` +
		`{"name":"reviewer","type":{"kind":"atomic","name":"string"},"required":true,"body":false},` +
		`{"name":"target","type":{"kind":"atomic","name":"string"},"required":false,"body":false}]},` +
		`"nodes":[` + nodes + `]}`
}

// fbrMainDoc wraps top-level nodes + a formulas bundle into a full IR doc with array +
// string inputs (reviewers, target, prep).
func fbrMainDoc(mainNodes, formulas string) string {
	return `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"main","input":{"name":"main.input","fields":[` +
		`{"name":"reviewers","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false},` +
		`{"name":"target","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + mainNodes + `],"formulas":{` + formulas + `}}`
}

// corpusEnv is the marquee env: reviewer <- the binder, target <- the input.
func corpusEnv() string { return envF("reviewer", "reviewer") + "," + envF("target", "target") }

// TestLowerForEachRunBodyStashesSpec (§1.1) pins the FBR lowering shape: a run-bodied
// for-each lowers to a SINGLE unitForEach carrying bodyRun (+ the re-lowering context) and
// bodyIRKind==NodeRun, with NO member units emitted (the sub-graphs are minted per element
// at run time). The run node's authored id "lane" is discarded — the member namespace is
// fanout/<index>/, not lane.
func TestLowerForEachRunBodyStashesSpec(t *testing.T) {
	doc := decodeBundle(t, fbrMainDoc(
		fbrFanNode(refV("reviewers"), fbrRunMember("reviewLane", corpusEnv())),
		reviewLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v (a run-bodied for-each must lower)", err)
	}
	fan := unitByNode(units, "fanout")
	if fan == nil || fan.kind != unitForEach || fan.forEach == nil {
		t.Fatalf("fanout = %+v, want a unitForEach", fan)
	}
	if fan.forEach.bodyRun == nil || fan.forEach.bodyIRKind != ir.NodeRun {
		t.Fatalf("forEach body = {bodyRun:%v kind:%q}, want a run body", fan.forEach.bodyRun, fan.forEach.bodyIRKind)
	}
	if fan.forEach.bodyRun.target != "reviewLane" {
		t.Errorf("bodyRun target = %q, want reviewLane", fan.forEach.bodyRun.target)
	}
	if fan.forEach.bodyFormula == nil || len(fan.forEach.bodyFormula.Nodes) != 1 {
		t.Errorf("bodyFormula = %+v, want the reviewLane sub-formula", fan.forEach.bodyFormula)
	}
	// No member units are lowered; neither is the authored run id "lane".
	if unitByNode(units, "fanout/0") != nil || unitByNode(units, "lane") != nil {
		t.Errorf("member/run units must be runtime-minted; got %v", nodeIDs(units))
	}
}

// TestForEachRunBodyMintCoordinatesAndMemberIndex (§1.2, Q-C) pins the per-member mint: the
// aggregate settles at fanout/<i>:0 (nodeID fanout/<i>), parented under the FAN aggregate,
// carries MemberIndex==i, and its sub-node lives one level deeper at fanout/<i>/review.
func TestForEachRunBodyMintCoordinatesAndMemberIndex(t *testing.T) {
	doc := decodeBundle(t, fbrMainDoc(
		fbrFanNode(refV("reviewers"), fbrRunMember("reviewLane", corpusEnv())),
		reviewLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	fan := unitByNode(units, "fanout")
	for _, i := range []int{0, 1, 3} {
		idx := i
		member := "fanout/" + strconv.Itoa(i)
		sub, agg, err := mintRunBody(fan.forEach.runBodyStash, fan.forEach.bodyRun, member, member+"/",
			activationFor(member), fan.activation, fan.ns, fan.afterDeps, fan.rawAfter, &idx)
		if err != nil {
			t.Fatalf("mint member %d: %v", i, err)
		}
		if agg.activation != member+":0" || agg.nodeID != member || agg.irKind != ir.NodeRun {
			t.Errorf("member %d agg = {%q %q %q}, want {%s:0 %s run}", i, agg.activation, agg.nodeID, agg.irKind, member, member)
		}
		if agg.parent != "fanout:0" {
			t.Errorf("member %d agg parent = %q, want fanout:0 (out of the fan's runOutcome)", i, agg.parent)
		}
		if agg.ns != "" {
			t.Errorf("member %d agg ns = %q, want \"\" (the root fan's namespace)", i, agg.ns)
		}
		if agg.memberIndex == nil || *agg.memberIndex != i {
			t.Errorf("member %d agg memberIndex = %v, want %d (leaf-member projection parity)", i, agg.memberIndex, i)
		}
		wantSub := member + "/review"
		if len(sub) != 1 || sub[0].nodeID != wantSub || sub[0].ns != member+"/" {
			t.Errorf("member %d sub = %v, want [%s] in ns %s/", i, nodeIDs(sub), wantSub, member)
		}
		if len(agg.members) != 1 || agg.members[0] != wantSub+":0" {
			t.Errorf("member %d agg members = %v, want [%s:0]", i, agg.members, wantSub)
		}
	}
}

// TestForEachRunBodyMintMemberSourceOrderParity pins that the mint's aggregate members are
// SOURCE order (lowerRun parity), not topo order: for a sub-formula authored [b after a, a]
// the members must be [fanout/0/b:0, fanout/0/a:0].
func TestForEachRunBodyMintMemberSourceOrderParity(t *testing.T) {
	greeterNodes := execNode("b", []string{"a"}, "echo b") + "," + execNode("a", nil, "echo a")
	doc := decodeBundle(t, fbrMainDoc(
		fbrFanNode(refV("reviewers"), fbrRunMember("reviewLane", corpusEnv())),
		reviewLaneFormula("reviewLane", greeterNodes)))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	fan := unitByNode(units, "fanout")
	idx := 0
	_, agg, err := mintRunBody(fan.forEach.runBodyStash, fan.forEach.bodyRun, "fanout/0", "fanout/0/",
		activationFor("fanout/0"), fan.activation, fan.ns, fan.afterDeps, fan.rawAfter, &idx)
	if err != nil {
		t.Fatalf("mint: %v", err)
	}
	if len(agg.members) != 2 || agg.members[0] != "fanout/0/b:0" || agg.members[1] != "fanout/0/a:0" {
		t.Fatalf("minted agg members = %v, want [fanout/0/b:0 fanout/0/a:0] (source order)", agg.members)
	}
}

// TestLowerForEachRunBodyEnvRefGatesFan (⚑B2) pins that a run-bodied fan gates on the parent
// NODES its body run's environment reads (minus the binder): target<-ref prep (a node) gates
// the fan on prep:0; the binder reviewer<-ref reviewer does NOT gate even when a same-named
// node exists (Q-D); an env ref to an input is no gate.
func TestLowerForEachRunBodyEnvRefGatesFan(t *testing.T) {
	// A parent node `prep` (env-read → gate) AND a parent node named `reviewer` (the binder
	// name → must NOT gate). over is an input (no gate).
	env := envF("reviewer", "reviewer") + "," + envF("target", "prep")
	doc := decodeBundle(t, fbrMainDoc(
		execNode("prep", nil, "echo p")+","+
			execNode("reviewer", nil, "echo r")+","+
			fbrFanNode(refV("reviewers"), fbrRunMember("reviewLane", env)),
		reviewLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v", err)
	}
	fan := unitByNode(units, "fanout")
	if fan == nil || !containsStr(fan.afterDeps, "prep:0") {
		t.Errorf("fan afterDeps = %v, want the env-ref gate prep:0", deref(fan).afterDeps)
	}
	if containsStr(fan.afterDeps, "reviewer:0") {
		t.Errorf("fan afterDeps = %v, want NO gate on the binder-named node reviewer:0 (Q-D binder exclusion)", fan.afterDeps)
	}
}

// TestLowerForEachRunBodyEnvRefSynthBodyExempt (⚑B2 pin a) pins that an env ref naming a
// sibling guard's SYNTHESIZED then id is accepted-UNGATED (static-run parity) — NOT refused
// like the over-ref synth-ban, and NOT gated either: a synth id is never in byNodeID, so
// the ⚑B2 pass contributes no edge for it. The env is ban-EXEMPT (the loop/run precedent).
func TestLowerForEachRunBodyEnvRefSynthBodyExempt(t *testing.T) {
	env := envF("reviewer", "reviewer") + "," + envF("target", "gthen") // gthen = a synth then id
	doc := decodeBundle(t, fbrMainDoc(
		guardNode("g", nil, condRefEq("target", "go"), execNode("gthen", nil, "echo t"))+","+
			fbrFanNode(refV("reviewers"), fbrRunMember("reviewLane", env)),
		reviewLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits refused an env-ref-to-synth-body run body: %v (must be accepted-ungated)", err)
	}
	// The UNGATED half: no gthen-derived gate lands on the fan (accepted-ungated, not
	// accepted-gated — a synth id contributes no edge).
	fan := unitByNode(units, "fanout")
	if fan == nil {
		t.Fatalf("no fanout unit; got %v", nodeIDs(units))
	}
	if containsStr(fan.afterDeps, "gthen:0") {
		t.Errorf("fan afterDeps = %v, want NO gthen-derived gate (synth env refs are gate-exempt)", fan.afterDeps)
	}
}

// TestLowerForEachRunBodyOverRefSynthBanUnchanged (⚑B2 pin c) pins that the OVER ref keeps
// the synth-body ban: a fan whose `over` names a sibling guard's synth then refuses loudly.
func TestLowerForEachRunBodyOverRefSynthBanUnchanged(t *testing.T) {
	doc := decodeBundle(t, fbrMainDoc(
		guardNode("g", nil, condRefEq("target", "go"), execNode("gthen", nil, "echo t"))+","+
			fbrFanNode(refV("gthen"), fbrRunMember("reviewLane", corpusEnv())),
		reviewLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }}"`))))
	_, err := buildUnits(doc, true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") ||
		!strings.Contains(err.Error(), "gthen") || !strings.Contains(err.Error(), "for-each") {
		t.Errorf("err = %v, want a for-each over-ref synth-body refusal naming gthen", err)
	}
}

// TestLowerForEachRunBodyEnvRefSilentClosureSubstituted (⚑B2 pin b) pins that an env ref to a
// SILENT node is closure-substituted (gates on the silent value's non-silent inputs), NOT
// refused: target<-ref msg (a silent interp over {{seed}}) gates the fan on seed:0, not msg:0.
func TestLowerForEachRunBodyEnvRefSilentClosureSubstituted(t *testing.T) {
	silent := `{"kind":"interp","id":"msg","name":"msg","after":["seed"],"body":{"raw":"{{ seed }}"}}`
	env := envF("reviewer", "reviewer") + "," + envF("target", "msg")
	doc := decodeBundle(t, fbrMainDoc(
		execNode("seed", nil, "echo s")+","+silent+","+
			fbrFanNode(refV("reviewers"), fbrRunMember("reviewLane", env)),
		reviewLaneFormula("reviewLane", execNode("review", nil, `echo "{{ reviewer }} {{ target }}"`))))
	units, err := buildUnits(doc, true, true)
	if err != nil {
		t.Fatalf("buildUnits: %v (an env-ref-to-silent-node must closure-substitute, not refuse)", err)
	}
	fan := unitByNode(units, "fanout")
	if fan == nil || !containsStr(fan.afterDeps, "seed:0") {
		t.Errorf("fan afterDeps = %v, want the silent closure seed:0", deref(fan).afterDeps)
	}
	if containsStr(fan.afterDeps, "msg:0") {
		t.Errorf("fan afterDeps = %v, want NO gate on the silent node msg:0 directly", fan.afterDeps)
	}
}

// TestLowerForEachRunBodyDecodeInheritance pins that the FBR arm inherits every decodeRunNode
// refusal (shared path, no drift): the deferred with-agent / runInput / non-transparent
// forms, a missing target, a recursive cycle, and a delimiter-bearing env ref all refuse at
// lowering — the full contract row set.
func TestLowerForEachRunBodyDecodeInheritance(t *testing.T) {
	plainLane := reviewLaneFormula("reviewLane", execNode("review", nil, "echo hi"))
	cases := []struct {
		name   string
		member string
		sub    string
		want   string
	}{
		{
			name: "with-agent override",
			member: `{"kind":"run","id":"lane","name":"lane","after":[],"with":{"kind":"agent","name":"x"},` +
				`"target":{"kind":"by-name","name":"reviewLane"},"environment":{"fields":[` + corpusEnv() + `]},"outcome":"transparent"}`,
			sub:  plainLane,
			want: "with-agent override",
		},
		{
			name: "runInput form",
			member: `{"kind":"run","id":"lane","name":"lane","after":[],"runInput":{},` +
				`"target":{"kind":"by-name","name":"reviewLane"},"environment":{"fields":[` + corpusEnv() + `]},"outcome":"transparent"}`,
			sub:  plainLane,
			want: "runInput form",
		},
		{
			name: "non-transparent outcome",
			member: `{"kind":"run","id":"lane","name":"lane","after":[],` +
				`"target":{"kind":"by-name","name":"reviewLane"},"environment":{"fields":[` + corpusEnv() + `]},"outcome":"detached"}`,
			sub:  plainLane,
			want: "only transparent",
		},
		{
			name:   "missing target",
			member: fbrRunMember("nonexistent", corpusEnv()),
			sub:    plainLane,
			want:   "not present",
		},
		{
			name:   "charset env ref",
			member: fbrRunMember("reviewLane", envF("reviewer", "reviewer")+","+envF("target", "a/b")),
			sub:    plainLane,
			want:   "reserved delimiters",
		},
		{
			name:   "recursive cycle",
			member: fbrRunMember("reviewLane", envF("reviewer", "reviewer")),
			sub:    plainLane, // placeholder, replaced below
			want:   "cycle",
		},
	}
	// The recursive-cycle sub runs reviewLane again inside its own fan.
	cases[len(cases)-1].sub = `"reviewLane":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"reviewLane","input":{"name":"reviewLane.input","fields":[` +
		`{"name":"reviewer","type":{"kind":"atomic","name":"string"},"required":true,"body":false}]},` +
		`"nodes":[` + fbrFanNode(refV("reviewer"), fbrRunMember("reviewLane", envF("reviewer", "reviewer"))) + `]}`
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := decodeBundle(t, fbrMainDoc(fbrFanNode(refV("reviewers"), tc.member), tc.sub))
			_, err := buildUnits(doc, true, true)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want a %q refusal containing %q, got %v", tc.name, tc.want, err)
			}
		})
	}
}

// TestLowerForEachRunBodyDryRunRefusesNestedLoop (§2.10 ⚑S4) pins that a run-bodied fan whose
// member sub-formula contains a LOOP does not lower — the member-prefix mint hits the loop's
// prefix fence ("top-level only") and buildUnits refuses with the for-each run-body
// provenance wrap, so EnqueueRun refuses before seeding a run.
func TestLowerForEachRunBodyDryRunRefusesNestedLoop(t *testing.T) {
	sub := reviewLaneFormula("reviewLane", retryMember("r1", "b1", "echo hi"))
	doc := decodeBundle(t, fbrMainDoc(
		fbrFanNode(refV("reviewers"), fbrRunMember("reviewLane", corpusEnv())), sub))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") || !strings.Contains(err.Error(), "top-level") {
		t.Fatalf("want a for-each run-body dry-run refusal (does not lower / top-level), got %v", err)
	}
	if !strings.Contains(err.Error(), "for-each") {
		t.Errorf("refusal %v should name the for-each provenance", err)
	}
}

// TestLowerForEachRunBodyDryRunProvenanceUnderRepeat (§2.10) pins the COMPOSED provenance: a
// run-bodied fan inside a repeat run body, whose fan member sub contains a loop, refuses with
// the for-each wrap composed UNDER the repeat wrap.
func TestLowerForEachRunBodyDryRunProvenanceUnderRepeat(t *testing.T) {
	// reviewer sub-formula body is a run-bodied fan whose member (innerLane) has a loop.
	inner := reviewLaneFormula("innerLane", retryMember("r1", "b1", "echo hi"))
	reviewer := `"reviewer":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"x"},` +
		`"name":"reviewer","input":{"name":"reviewer.input","fields":[` +
		`{"name":"reviewers","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}]},` +
		`"nodes":[` + fbrFanNode(refV("reviewers"), fbrRunMember("innerLane", envF("reviewer", "reviewer"))) + `]}`
	stage := runNode("stage", nil, "reviewer", "reviewers", "reviewers")
	doc := decodeBundle(t, runMainDoc(
		repeatRunNode(stage, repeatRunCondPassOrIter()),
		reviewer+","+inner))
	_, err := buildUnits(doc, true, true)
	if err == nil {
		t.Fatalf("want a composed dry-run refusal, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "does not lower") {
		t.Fatalf("refusal %v should be a does-not-lower dry-run wrap", err)
	}
	// The NESTING order pin: the repeat wrap is the OUTER frame, the for-each wrap the
	// inner — "repeat" must appear BEFORE "for-each" in the composed message.
	repeatIdx, forEachIdx := strings.Index(msg, "repeat"), strings.Index(msg, "for-each")
	if repeatIdx < 0 || forEachIdx < 0 || repeatIdx >= forEachIdx {
		t.Errorf("refusal %q: want the for-each wrap composed UNDER the repeat wrap (repeat@%d before for-each@%d)", msg, repeatIdx, forEachIdx)
	}
}

// TestForEachRunBodyFixtureLowers guards the hand-authored for-each-run-body dolt-e2e bundle
// fixture: `fanout: scatter reviewer in reviewers { lane: run reviewLane given {…} }` decodes
// and lowers under BOTH the inline and controller-loop pool flag pairs, so a fixture typo
// fails fast here — not 10min into the e2e — and the member mint lands at fanout/0/review.
func TestForEachRunBodyFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "for-each-run-body.lumen.json")
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
			t.Fatalf("lower for-each-run-body (allowCombineDo=%v): %v", combineDo, err)
		}
		fan := unitByNode(units, "fanout")
		if fan == nil || fan.kind != unitForEach || fan.forEach == nil || fan.forEach.bodyRun == nil {
			t.Fatalf("no run-bodied for-each unit; got %v", nodeIDs(units))
		}
		if fan.forEach.binder != "reviewer" || fan.forEach.bodyRun.target != "reviewLane" {
			t.Errorf("fan = {binder:%q target:%q}, want {reviewer reviewLane}", fan.forEach.binder, fan.forEach.bodyRun.target)
		}
		idx := 0
		sub, agg, err := mintRunBody(fan.forEach.runBodyStash, fan.forEach.bodyRun, "fanout/0", "fanout/0/",
			activationFor("fanout/0"), fan.activation, fan.ns, fan.afterDeps, fan.rawAfter, &idx)
		if err != nil {
			t.Fatalf("mint member 0 (allowCombineDo=%v): %v", combineDo, err)
		}
		if unitByNode(sub, "fanout/0/review") == nil || agg.nodeID != "fanout/0" {
			t.Fatalf("minted member = %v (agg %q), want a sub-do at fanout/0/review under agg fanout/0", nodeIDs(sub), agg.nodeID)
		}
	}
}
