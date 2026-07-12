package engine

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// --- shared-lane (SLX) lowering fixtures -----------------------------------

// slxDoIdx renders a do node whose template parts carry an index interpolation
// {{ <indexSrc> }} (the shape the sweep inspects).
func slxDoIdx(id, indexSrc string) string {
	src := jsonStr(indexSrc)
	raw := jsonStr("x {{ " + indexSrc + " }}")
	return `{"kind":"do","id":"` + id + `","name":"` + id + `","after":[],` +
		`"origin":{"uri":"t","line":1,"col":0},"source":{"kind":"prompt"},` +
		`"interpreter":{"kind":"agent","mode":{"kind":"do"},"origin":{"uri":"t","line":1,"col":0}},` +
		`"body":{"raw":` + raw + `,"template":{"parts":[` +
		`{"kind":"interp","expr":{"kind":"literal","value":` + src + `}}]},` +
		`"source":{"kind":"inline"},"templated":true,"language":"markdown","syntax":"bare","origin":{"uri":"t","line":1,"col":0}}}`
}

// slxExecIdx renders an exec node whose template parts carry an index interpolation (the
// exec render path ignores template.parts, but the sweep still refuses ANY hit).
func slxExecIdx(id, indexSrc string) string {
	src := jsonStr(indexSrc)
	raw := jsonStr("echo {{ " + indexSrc + " }}")
	return `{"kind":"exec","id":"` + id + `","name":"` + id + `","after":[],` +
		`"interpreter":{"program":{"kind":"shell"}},` +
		`"body":{"raw":` + raw + `,"template":{"parts":[{"kind":"interp","expr":{"kind":"literal","value":` + src + `}}]}}}`
}

// slxLitIdx renders a silent lit leaf whose literal value is an index expression.
func slxLitIdx(id, indexSrc string) string {
	return `{"kind":"lit","id":"` + id + `","name":"` + id + `","after":[],"value":{"kind":"literal","value":` + jsonStr(indexSrc) + `}}`
}

// slxInterpPartsIdx renders a silent interp leaf carrying TOP-LEVEL parts (the direct
// raw["parts"] shape — no template wrapper, no body) with one literal interp part.
func slxInterpPartsIdx(id, indexSrc string) string {
	return `{"kind":"interp","id":"` + id + `","name":"` + id + `","after":[],` +
		`"parts":[{"kind":"interp","expr":{"kind":"literal","value":` + jsonStr(indexSrc) + `}}]}`
}

// slxLengthCond renders a repeat cond that is `length(<argRef>)` (a numeric truthiness).
func slxLengthCond(argRef string) string {
	return `{"kind":"call","name":"length","args":[{"kind":"ref","name":"` + argRef + `"}]}`
}

// TestSharedLaneTemplateSweepRefusals pins §1.2.5: the loud-wall sweep at LOWER. A do with a
// STRICT-FAILING index is refused; a do with a STRICT-PASSING index LOWERS; an exec and a
// silent lit/interp leaf refuse ANY pre-grammar hit (they cannot index); a non-pre-grammar
// literal survives verbatim.
func TestSharedLaneTemplateSweepRefusals(t *testing.T) {
	for _, tc := range []struct {
		name    string
		node    string
		wantErr bool
	}{
		{"do-strict-fail-plus", slxDoIdx("d", "items[i + 1]"), true},
		{"do-strict-fail-empty", slxDoIdx("d", "items[]"), true},
		{"do-strict-fail-nested", slxDoIdx("d", "items[i][j]"), true},
		{"do-strict-pass-int", slxDoIdx("d", "items[0]"), false},
		{"do-strict-pass-sub", slxDoIdx("d", "items[iteration - 1]"), false},
		{"do-verbatim-survives", slxDoIdx("d", "see [docs]"), false},
		{"exec-any-hit-refused", slxExecIdx("e", "items[0]"), true},
		{"exec-verbatim-survives", slxExecIdx("e", "see [docs]"), false},
		{"lit-any-hit-refused", slxLitIdx("l", "items[0]"), true},
		{"lit-verbatim-survives", slxLitIdx("l", "see [docs]"), false},
		{"interp-toplevel-parts-refused", slxInterpPartsIdx("i", "items[0]"), true},
		{"interp-toplevel-parts-verbatim-survives", slxInterpPartsIdx("i", "see [docs]"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := buildUnits(decodeBundle(t, plainDoc(tc.node)), true, true)
			if tc.wantErr {
				if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "interp index expr") {
					t.Fatalf("err = %v, want an ErrUnsupportedNode `interp index expr` refusal", err)
				}
			} else if err != nil {
				t.Fatalf("err = %v, want the node to lower", err)
			}
		})
	}
}

// TestSharedLaneEnqueueProvenance pins §2.6: a STRICT-FAILING index do inside a repeat RUN
// body refuses at buildUnits wrapped in the run-body dry-run provenance (`run body does not
// lower`), so a bad index refuses before any effect.
func TestSharedLaneEnqueueProvenance(t *testing.T) {
	inner := lisSubFormula("inner", "", slxDoIdx("bad", "items[i + 1]"))
	runBody := `{"kind":"run","id":"round","name":"round","after":[],` +
		`"target":{"kind":"by-name","name":"inner"},"environment":{"fields":[]},"outcome":"transparent"}`
	loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],"iterationName":"iteration",` +
		`"cond":` + lisIterGE1() + `,"body":` + runBody + `}`
	wrapper := lisSubFormula("wrapper", "", loop)
	doc := decodeBundle(t, runMainDoc(lisWrapperRunNoEnv(), wrapper+","+inner))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") ||
		!strings.Contains(err.Error(), "interp index expr") {
		t.Fatalf("err = %v, want the index-sweep refusal wrapped in the run-body dry-run provenance", err)
	}
}

// TestSharedLaneDispatchArmDryRunProvenance pins §2.6 (the pilot-build path verbatim): a
// STRICT-FAILING index do inside a dispatch SAME-SESSION arm's target sub-formula refuses
// through the arm dry-run mint with the arm provenance — a bad index in the DAR path refuses
// at buildUnits.
func TestSharedLaneDispatchArmDryRunProvenance(t *testing.T) {
	badLane := darLaneFormula("badLane", slxDoIdx("bad", "items[i + 1]"))
	doc := decodeBundle(t, darMainDoc(
		darDispatch("policy", darRunArm("same-session", "sharedLane", "badLane", darCorpusEnv("s"))),
		badLane))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") ||
		!strings.Contains(err.Error(), "interp index expr") {
		t.Fatalf("err = %v, want the index-sweep refusal wrapped in the dispatch-arm dry-run provenance", err)
	}
	if !strings.Contains(err.Error(), "same-session") {
		t.Errorf("refusal %v should name the same-session arm provenance", err)
	}
}

// TestSharedLaneGuardCallRefusedInSubFormula pins the amended §1.1.7: a guard cond carrying
// a call expr INSIDE a run sub-formula is refused at buildUnits — condScope's ns arm is the
// GIS-pinned string-typed child-wins view, so `length(<array binding>)` would count the JSON
// render TEXT (an empty bound array goes truthy). Root guards keep call support (typed
// d.input). Pinned both directions.
func TestSharedLaneGuardCallRefusedInSubFormula(t *testing.T) {
	cond := `{"kind":"operator","op":">","operands":[` + slxLengthCond("items") + `,{"kind":"literal","value":0}]}`
	guard := guardNode("g", nil, cond, execNode("gthen", nil, "echo t"))

	// Root placement: lowers (typed d.input — call support stands).
	if _, err := buildUnits(decodeBundle(t, plainDoc(guard)), true, true); err != nil {
		t.Fatalf("root guard with a length() cond refused: %v (root keeps call support)", err)
	}

	// Sub-formula placement: refused LOUD with the string-typed-scope message.
	wrapper := lisSubFormula("wrapper", slxArrInputField("items"), guard)
	_, err := buildUnits(decodeBundle(t, runMainDoc(slxWrapRunItems(), wrapper)), true, true)
	if err == nil || !errorsIsUnsupported(err) ||
		!strings.Contains(err.Error(), "call expressions unsupported in a sub-formula (string-typed decision scope)") {
		t.Fatalf("ns guard call cond = %v, want the string-typed-scope refusal", err)
	}
}

// TestSharedLaneGuardCallRefusalDryRunProvenance pins the amended §1.1.7 through a repeat
// RUN-BODY dry-run mint: a call-bearing guard cond inside the body's target sub-formula
// refuses at buildUnits wrapped in the run-body provenance.
func TestSharedLaneGuardCallRefusalDryRunProvenance(t *testing.T) {
	cond := `{"kind":"operator","op":">","operands":[` + slxLengthCond("items") + `,{"kind":"literal","value":0}]}`
	// items is DEFAULTED-unbound so the run body's env stays empty and the dry-run mint
	// reaches the guard (a required-unbound field would refuse earlier, masking the pin).
	defaultedItems := `{"name":"items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":false,"default":["a"],"body":false}`
	inner := lisSubFormula("inner", defaultedItems,
		`{"kind":"guard","id":"g","name":"g","after":[],"cond":`+cond+`,"then":`+execNode("gthen", nil, "echo t")+`}`)
	runBody := `{"kind":"run","id":"round","name":"round","after":[],` +
		`"target":{"kind":"by-name","name":"inner"},"environment":{"fields":[]},"outcome":"transparent"}`
	loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],"iterationName":"iteration",` +
		`"cond":` + lisIterGE1() + `,"body":` + runBody + `}`
	doc := decodeBundle(t, runMainDoc(lisWrapperRunNoEnv(), lisSubFormula("wrapper", "", loop)+","+inner))
	_, err := buildUnits(doc, true, true)
	if err == nil || !strings.Contains(err.Error(), "run body does not lower") ||
		!strings.Contains(err.Error(), "call expressions unsupported in a sub-formula") {
		t.Fatalf("err = %v, want the ns guard-call refusal wrapped in the run-body dry-run provenance", err)
	}
}

// TestSharedLaneCallArgCharsetBan pins §2.6: a loop cond `length(<ref>)` whose call ARG ref
// carries a reserved delimiter is refused by the existing charset sweep (collectRefs picks
// the call arg's ref with NO new plumbing).
func TestSharedLaneCallArgCharsetBan(t *testing.T) {
	loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],"iterationName":"iteration",` +
		`"cond":` + slxLengthCond("a/b") + `,"body":` + execNode("body", nil, "echo hi") + `}`
	_, err := buildUnits(decodeBundle(t, plainDoc(loop)), true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "reserved delimiters") {
		t.Fatalf("err = %v, want a charset refusal on the call-arg ref a/b", err)
	}
}

// TestSharedLaneCallArgSynthBan pins §2.6: a loop cond `length(<synth-body>)` whose call ARG
// ref names a sibling guard's synthesized then body is refused by the ban-only synth sweep —
// with NO new fold edges (collectRefs feeds condRefs; the sweep appends zero gates).
func TestSharedLaneCallArgSynthBan(t *testing.T) {
	guard := guardNode("g", nil, condRefEq("who", "x"), execNode("gthen", nil, "echo t"))
	loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],"iterationName":"iteration",` +
		`"cond":` + slxLengthCond("gthen") + `,"body":` + execNode("body", nil, "echo hi") + `}`
	_, err := buildUnits(decodeBundle(t, plainDoc(guard+","+loop)), true, true)
	if err == nil || !errorsIsUnsupported(err) || !strings.Contains(err.Error(), "synthesized decision body") {
		t.Fatalf("err = %v, want a ban-only synth-body refusal naming gthen", err)
	}
}

// TestSharedLaneLengthCondNoNewFoldEdges is the MUTATION PIN (§1.1.6): a leaf loop whose cond
// carries a `length(items)` call gains NO fold edge (the call args feed condRefs but the
// ban-only sweep appends ZERO gates) — root AND inside a sub-formula.
func TestSharedLaneLengthCondNoNewFoldEdges(t *testing.T) {
	cond := `{"kind":"operator","op":">","operands":[` + slxLengthCond("items") + `,{"kind":"literal","value":0}]}`
	loop := `{"kind":"repeat","id":"loop","name":"loop","after":[],"iterationName":"iteration",` +
		`"cond":` + cond + `,"body":` + execNode("body", nil, "echo hi") + `}`

	rootUnits, err := buildUnits(decodeBundle(t, plainDoc(loop)), true, true)
	if err != nil {
		t.Fatalf("root buildUnits: %v", err)
	}
	if lu := unitByNode(rootUnits, "loop"); lu == nil || len(lu.afterDeps) != 0 {
		t.Fatalf("root loop afterDeps = %v, want none (a length call arg adds ZERO gates)", deref(lu).afterDeps)
	}

	wrapper := lisSubFormula("wrapper", slxArrInputField("items"), loop)
	nsUnits, err := buildUnits(decodeBundle(t, runMainDoc(slxWrapRunItems(), wrapper)), true, true)
	if err != nil {
		t.Fatalf("ns buildUnits: %v", err)
	}
	lu := unitByNode(nsUnits, "wrap/loop")
	if lu == nil {
		t.Fatal("no wrap/loop unit")
	}
	if len(lu.afterDeps) != 0 {
		t.Errorf("ns loop afterDeps = %v, want none (a length call arg adds ZERO gates at depth)", lu.afterDeps)
	}
}

// TestSharedLaneFixtureLowers guards the hand-authored shared-lane dolt-e2e bundle fixture
// (§2.10): the dispatch same-session arm → run doWorkShared → repeat leaf-loop with an
// indexed render + length cond decodes and lowers under BOTH pool flag pairs, so a fixture
// typo fails fast HERE, not 350s into the e2e.
func TestSharedLaneFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "shared-lane.lumen.json")
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
			t.Fatalf("buildUnits(allowCombineDo=%v) refused the shared-lane fixture: %v", combineDo, err)
		}
		if d := unitByNode(units, "route"); d == nil || d.kind != unitDispatch {
			t.Fatalf("route = %+v, want a unitDispatch", d)
		}
	}
}

// lisIterGE1 is a `iteration >= 1` cond (a run-body loop cond reading only the counter).
func lisIterGE1() string {
	return `{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":1}]}`
}

// slxArrInputField renders a required array-of-string input field.
func slxArrInputField(name string) string {
	return `{"name":"` + name + `","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":true,"body":false}`
}

// slxWrapRunItems renders the main `run wrap -> wrapper given {items <- who}` node (runMainDoc
// declares the string input `who`; the binding type is irrelevant to the afterDeps mutation
// pin, which only checks that the loop's cond call adds no gate).
func slxWrapRunItems() string {
	return `{"kind":"run","id":"wrap","name":"wrap","after":[],"target":{"kind":"by-name","name":"wrapper"},` +
		`"environment":{"fields":[{"name":"items","value":{"kind":"expr","expr":{"kind":"ref","name":"who"}}}]},` +
		`"outcome":"transparent"}`
}
