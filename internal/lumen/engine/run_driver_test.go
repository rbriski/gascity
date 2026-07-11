package engine_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/enginehost"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// injectCrashThenResumeInput is injectCrashThenResume with a run input threaded
// through both the crashed run and the resume (the base helper passes nil input).
func injectCrashThenResumeInput(t *testing.T, doc *ir.IR, host enginehost.AgentHost, boundary, activation string, input map[string]any, snapEvery int) (engine.RunResult, *graphstore.Store, string) {
	t.Helper()
	ctx := context.Background()
	store := newStore(t)

	errCrash := errors.New("crash injected at " + boundary + " @ " + activation)
	var crashedStream string
	fired := false
	restore := engine.SetCrashHookForTest(func(b, streamID, act string) error {
		if b == boundary && (activation == "" || act == activation) && !fired {
			fired = true
			crashedStream = streamID
			return errCrash
		}
		return nil
	})

	_, runErr := engine.RunWithOptions(ctx, store, doc, input, engine.Options{Host: host, SnapshotEvery: snapEvery})
	restore()
	if !errors.Is(runErr, errCrash) {
		t.Fatalf("crash at (%s,%s): run returned %v, want the injected sentinel", boundary, activation, runErr)
	}
	if crashedStream == "" {
		t.Fatalf("crash at (%s,%s): no stream id captured", boundary, activation)
	}
	resumed, err := engine.Resume(ctx, store, doc, crashedStream, input, engine.Options{Host: host, SnapshotEvery: snapEvery})
	if err != nil {
		t.Fatalf("crash at (%s,%s): resume: %v", boundary, activation, err)
	}
	return resumed, store, crashedStream
}

// --- bundle-doc builders (exec-only sub-formulas; no host needed) ---

// strField renders one required string input field for a sub-formula's accepts block.
func strField(name string) string {
	return `{"name":"` + name + `","type":{"kind":"atomic","name":"string"},"required":true,"body":false}`
}

// runNodeJSON renders a run node binding one sub-input field from a parent ref.
func runNodeJSON(id string, after []string, target, subField, parentRef string) string {
	env := ""
	if subField != "" {
		env = `{"name":"` + subField + `","value":{"kind":"expr","expr":{"kind":"ref","name":"` + parentRef + `"}}}`
	}
	return runNodeRawEnv(id, after, target, `[`+env+`]`)
}

// runNodeRawEnv renders a run node with an explicit environment.fields JSON array.
func runNodeRawEnv(id string, after []string, target, fieldsJSON string) string {
	a, _ := json.Marshal(after)
	return `{"kind":"run","id":"` + id + `","name":"` + id + `","after":` + string(a) +
		`,"target":{"kind":"by-name","name":"` + target + `"},` +
		`"environment":{"fields":` + fieldsJSON + `},"outcome":"transparent"}`
}

// subDoc renders a full sub-IR doc entry for the formulas bundle.
func subDoc(name, inputFields string, nodes ...string) string {
	return `"` + name + `":{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"test"},` +
		`"name":"` + name + `","input":{"name":"` + name + `.input","fields":[` + inputFields + `]},` +
		`"nodes":[` + strings.Join(nodes, ",") + `]}`
}

// bundleDoc assembles a main formula (input fields + nodes) plus a formulas bundle.
func bundleDoc(inputFields, mainNodes, formulas string) string {
	return `{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"test"},` +
		`"name":"main","input":{"name":"main.input","fields":[` + inputFields + `]},` +
		`"nodes":[` + mainNodes + `],"formulas":{` + formulas + `}}`
}

// TestRunTransparentPassAndValuePlumbing proves a top-level run of an exec
// sub-formula passes transparently, seeds the sub-scope from the environment, and
// records the sub-formula's result into the parent scope for a downstream {{ref}}.
func TestRunTransparentPassAndValuePlumbing(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, bundleDoc(
		strField("who"),
		execNode("prep", `echo prepping`, nil)+","+
			runNodeJSON("greeting", []string{"prep"}, "greeter", "name", "who")+","+
			execNode("done", `echo "got: {{ greeting }}"`, []string{"greeting"}),
		subDoc("greeter", strField("name"),
			execNode("hello", `echo "hi {{ name }}"`, nil)),
	))

	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["greeting/hello"]; got != "hi world" {
		t.Errorf("sub output greeting/hello = %q, want %q (env seeded)", got, "hi world")
	}
	if got := res.NodeOutputs["greeting"]; got != "hi world" {
		t.Errorf("run output greeting = %q, want %q (transparent output)", got, "hi world")
	}
	if got := res.NodeOutputs["done"]; got != "got: hi world" {
		t.Errorf("downstream done = %q, want %q (value plumbing through run)", got, "got: hi world")
	}
}

// TestRunScopeIsolation proves the sub-scope sees ONLY what environment binds
// (plus defaults) — a parent input not bound through environment is invisible,
// and a sub node's output does not leak to the parent as a bare name.
func TestRunScopeIsolation(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("greeting", nil, "greeter", "name", "who")+","+
			// `hello` is a sub node id; the parent must NOT see it as a bare name.
			execNode("leak", `echo "hello={{ hello }}"`, []string{"greeting"}),
		subDoc("greeter", strField("name"),
			// The sub sees `name` (bound) but NOT `who` (parent input, unbound here).
			execNode("hello", `echo "name={{ name }} who={{ who }}"`, nil)),
	))

	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	// interpolate leaves an unknown ref as its literal `{{ ref }}` placeholder (the
	// existing engine behavior), so isolation shows as the ref STAYING unresolved:
	// `who` (a parent input, not bound into the sub) never becomes "world" in the
	// sub, and `hello` (a sub node id) never resolves to the sub's output in the parent.
	if got := res.NodeOutputs["greeting/hello"]; got != "name=world who={{ who }}" {
		t.Errorf("sub output = %q, want parent input `who` to stay unresolved (isolation)", got)
	}
	if got := res.NodeOutputs["leak"]; got != "hello={{ hello }}" {
		t.Errorf("parent leak output = %q, want sub node `hello` to stay unresolved (isolation)", got)
	}
}

// TestRunTransparentFailedSkipCascades proves a sub-formula failure makes the run
// settle failed, and a downstream parent unit skip-cascades off it.
func TestRunTransparentFailedSkipCascades(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, bundleDoc(
		"",
		runNodeJSON("greeting", nil, "greeter", "", "")+","+
			execNode("after", `echo ran`, []string{"greeting"}),
		subDoc("greeter", "",
			execNode("boom", `exit 3`, nil)),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Fatalf("outcome = %q, want failed (transparent)", res.Outcome)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "greeting", engine.OutcomeFailed)
	assertSettled(t, settled, "after", engine.OutcomeSkipped)
}

// TestRunTransparentIgnoresSubSkips proves the transparent outcome mirrors a
// standalone run's runOutcome (skipped members IGNORED), NOT a scatter's
// skipped->degraded mapping: a sub-formula whose first step fails and whose tail
// skip-cascades settles the run `failed`, not `degraded`.
func TestRunTransparentIgnoresSubSkips(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, bundleDoc(
		"",
		runNodeJSON("greeting", nil, "greeter", "", ""),
		subDoc("greeter", "",
			execNode("boom", `exit 1`, nil)+","+
				execNode("tail", `echo unreached`, []string{"boom"})),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomeFailed {
		t.Errorf("outcome = %q, want failed (skipped tail ignored, not degraded)", res.Outcome)
	}
}

// TestRunSkippedWhenGateFails proves a run gated on a failed dependency runs NO
// sub-effect: every sub-activation and the aggregate settle skipped.
func TestRunSkippedWhenGateFails(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, bundleDoc(
		"",
		execNode("gate", `exit 1`, nil)+","+
			runNodeJSON("greeting", []string{"gate"}, "greeter", "", ""),
		subDoc("greeter", "",
			execNode("hello", `echo hi`, nil)),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledIDs(t, res.Events)
	assertSettled(t, settled, "greeting", engine.OutcomeSkipped)
	assertSettled(t, settled, "greeting/hello", engine.OutcomeSkipped)
	// The sub exec must not have RUN — its output is empty (skip records nothing).
	if got := res.NodeOutputs["greeting/hello"]; got != "" {
		t.Errorf("skipped sub ran (output %q); want no effect", got)
	}
}

// TestRunDefaultsApplied proves an unbound sub-input field with a declared default
// renders from that default.
func TestRunDefaultsApplied(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	defField := `{"name":"greeting_word","type":{"kind":"atomic","name":"string"},"required":false,"default":"hello","body":false}`
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("greeting", nil, "greeter", `[]`), // bind nothing -> default applies
		subDoc("greeter", defField,
			execNode("say", `echo "{{ greeting_word }} world"`, nil)),
	))

	res, err := engine.Run(ctx, store, doc, nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["greeting/say"]; got != "hello world" {
		t.Errorf("defaulted sub output = %q, want %q", got, "hello world")
	}
}

// TestRunNestedDepth2ValueFlow proves environment chains across two run
// boundaries (parent -> mid -> leaf) and the result bubbles back up.
func TestRunNestedDepth2ValueFlow(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	doc := decodeIR(t, bundleDoc(
		strField("who"),
		runNodeJSON("outer", nil, "mid", "person", "who")+","+
			execNode("done", `echo "final: {{ outer }}"`, []string{"outer"}),
		subDoc("mid", strField("person"),
			runNodeJSON("inner", nil, "leaf", "name", "person"))+","+
			subDoc("leaf", strField("name"),
				execNode("greet", `echo "hey {{ name }}"`, nil)),
	))

	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "sam"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["outer/inner/greet"]; got != "hey sam" {
		t.Errorf("grandchild output = %q, want %q (env chained 2 levels)", got, "hey sam")
	}
	if got := res.NodeOutputs["done"]; got != "final: hey sam" {
		t.Errorf("top output = %q, want %q (result bubbled up)", got, "final: hey sam")
	}
}

// runValueDoc is the shared prep -> run greeter -> done exec fixture (value
// plumbing through the run boundary), reused by the DET and resume proofs.
func runValueDoc(t *testing.T) string {
	t.Helper()
	return bundleDoc(
		strField("who"),
		execNode("prep", `echo prepping`, nil)+","+
			runNodeJSON("greeting", []string{"prep"}, "greeter", "name", "who")+","+
			execNode("done", `echo "got: {{ greeting }}"`, []string{"greeting"}),
		subDoc("greeter", strField("name"),
			execNode("hello", `echo "hi {{ name }}"`, nil)),
	)
}

// TestRunDropRefoldByteIdentity proves a run-bearing journal's live Tier-A
// projection equals a from-scratch drop+refold — the reducer folds no hidden state
// from the inlined sub-graph or the transparent aggregate (DET-T-17), so
// reducerVersion stays 3.
func TestRunDropRefoldByteIdentity(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, runValueDoc(t))

	res, err := engine.Run(ctx, store, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	assertProjectionEqualsRefold(t, store, res.StreamID)
	if v := engine.Reducer().ReducerVersion(); v != 4 {
		t.Fatalf("reducerVersion = %d, want 4 (v4 folds nodeState.Detail for the recover error binding)", v)
	}
}

// TestResumeMidSubGraphSealsIdentically proves a crash after a sub-node settles
// (mid-sub-graph) resumes to the SAME sealed outcome and downstream value, with a
// projection that drop+refolds identically — resume threads the bundle + sub-scope
// through the run boundary.
func TestResumeMidSubGraphSealsIdentically(t *testing.T) {
	doc := decodeIR(t, runValueDoc(t))

	// Uninterrupted baseline.
	base := newStore(t)
	want, err := engine.Run(context.Background(), base, doc, map[string]any{"who": "world"})
	if err != nil {
		t.Fatalf("baseline run: %v", err)
	}

	// Crash right after the sub-node greeting/hello settles, then resume.
	resumed, store, stream := injectCrashThenResumeInput(t, doc, nil,
		engine.CrashAfterSettle, "greeting/hello:0", map[string]any{"who": "world"}, 0)

	if resumed.Outcome != want.Outcome {
		t.Errorf("resumed outcome = %q, want %q", resumed.Outcome, want.Outcome)
	}
	if resumed.NodeOutputs["done"] != "got: hi world" {
		t.Errorf("resumed done = %q, want value plumbing intact through the run boundary", resumed.NodeOutputs["done"])
	}
	assertProjectionEqualsRefold(t, store, stream)
}

// TestEnqueueRunPrevalidatesUnlowerableBundle proves EnqueueRun refuses a bundle
// that cannot lower (here: a run targeting a formula absent from the bundle) LOUDLY
// at enqueue, rather than appending run.started and wedging the run open forever.
func TestEnqueueRunPrevalidatesUnlowerableBundle(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(
		"",
		runNodeJSON("greeting", nil, "missing", "", ""),
		subDoc("greeter", "", execNode("hello", `echo hi`, nil)),
	))

	streamID, err := engine.EnqueueRun(ctx, store, doc, nil, "packs/x@v1", "workers")
	if err == nil {
		t.Fatalf("enqueue accepted an un-lowerable bundle (streamID=%q); want a loud refusal", streamID)
	}
	if !strings.Contains(err.Error(), "does not lower") || !strings.Contains(err.Error(), "missing") {
		t.Errorf("enqueue error = %v, want it to name the lowering failure + the missing target", err)
	}
}

// TestAdvanceRunEnvRefSilentNodeSealsNoStall is the end-to-end symptom proof for
// the red-team HIGH: a run whose environment reads a SILENT (interp) parent node
// must SEAL under Advance, not wedge with ErrAdvanceStalled. `msg` is a silent
// interp over `{{seed}}` (a real exec); `greeting` binds name<-msg. The env-ref
// gate must substitute msg's non-silent closure (seed), which settles, so the
// sub-graph advances. Before the fix, greeting/hello gated on the never-settling
// msg:0 and every Advance pass deferred it forever.
func TestAdvanceRunEnvRefSilentNodeSealsNoStall(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()

	doc := decodeIR(t, bundleDoc(
		strField("ignored"),
		execNode("seed", `echo seedval`, nil)+","+
			`{"kind":"interp","id":"msg","name":"msg","after":["seed"],"body":{"raw":"{{ seed }}"}}`+","+
			runNodeJSON("greeting", nil, "greeter", "name", "msg"),
		subDoc("greeter", strField("name"),
			execNode("hello", `echo "hi {{ name }}"`, nil)),
	))

	streamID := "gcg-run-silentenvtest"
	res, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"ignored": "x"}, fake.opts())
	if err != nil {
		t.Fatalf("advance stalled/errored (the silent-env-ref wedge): %v", err)
	}
	if !res.Sealed {
		t.Fatalf("advance = %+v, want Sealed (exec-only sub-graph seals in one pass)", res)
	}
	if res.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Run.Outcome)
	}
	if got := res.Run.NodeOutputs["greeting/hello"]; got != "hi seedval" {
		t.Errorf("sub output = %q, want %q (silent msg resolved to seed's output through the run boundary)", got, "hi seedval")
	}
}

func assertSettled(t *testing.T, settled [][2]string, nodeID, want string) {
	t.Helper()
	for _, s := range settled {
		if s[0] == nodeID {
			if s[1] != want {
				t.Errorf("%q settled %q, want %q", nodeID, s[1], want)
			}
			return
		}
	}
	t.Errorf("no outcome.settled for %q (want %q); settled=%v", nodeID, want, settled)
}
