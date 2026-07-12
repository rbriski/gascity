package engine_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// insOptStrField renders one OPTIONAL, undefaulted string input field (the declared-null
// shape ga-wvqsay resolves to present-null; strField renders a REQUIRED field).
func insOptStrField(name string) string {
	return `{"name":"` + name + `","type":{"kind":"atomic","name":"string"},"required":false,"body":false}`
}

// insShadowCond builds `x == "done" || iteration >= 3`: pre-fix the same-named sibling node
// shadows `x` with its "done" output and the loop exits at attempt 0; post-fix `x` resolves
// present-null (null == "done" ⇒ NaN ⇒ FALSE), so only `iteration >= 3` exits — three attempts.
func insShadowCond() string {
	return `{"kind":"operator","op":"||","operands":[` +
		`{"kind":"operator","op":"==","operands":[{"kind":"ref","name":"x"},{"kind":"literal","value":"done"}]},` +
		`{"kind":"operator","op":">=","operands":[{"kind":"ref","name":"iteration"},{"kind":"literal","value":3}]}]}`
}

// insShadowDoc is the shared child-shadow fixture: a wrapper sub-formula declaring an
// OPTIONAL-unbound input `x`, a sibling node `x` that settles "done", and a run-body repeat
// loop (gated after `x` so the sibling has settled before the first decide) whose cond reads
// `x`. The run body targets an exec-only leaf, so both drivers seal without dispatches.
func insShadowDoc(t *testing.T) *ir.IR {
	t.Helper()
	return decodeIR(t, bundleDoc(
		"",
		runNodeRawEnv("wrap", nil, "wrapper", "[]"),
		subDoc("wrapper", insOptStrField("x"),
			execNode("x", `echo done`, nil)+","+
				repeatRunLoop([]string{"x"}, runNodeJSON("stage", nil, "leaf", "", ""), insShadowCond()))+","+
			subDoc("leaf", "", execNode("hi", `echo hi`, nil)),
	))
}

// TestRunOptionalUnboundInputClosesChildShadow is the INLINE-driver child-shadow mutant killer
// (ga-wvqsay): the run-body loop's cond reads an optional-unbound declared input `x` while a
// same-named sibling node settles "done". Post-fix `x` resolves present-null across every
// decide tick, so the loop runs its full three attempts (exit via `iteration >= 3`). Pre-fix
// (typedSubInput omitted the key) `x` fell through to the sibling's "done" and the loop exited
// after a single attempt.
func TestRunOptionalUnboundInputClosesChildShadow(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	res, err := engine.Run(ctx, store, insShadowDoc(t), nil)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", res.Outcome)
	}
	if n := countAttemptMinted(res.Events); n != 3 {
		t.Fatalf("attempt.minted = %d, want 3 — optional-unbound x resolves present-null, so the loop exits via iteration>=3, NOT the child-shadowed x==\"done\" at attempt 1", n)
	}
}

// TestAdvanceOptionalUnboundInputClosesChildShadow is the POOL-driver twin: the same fixture
// driven through Advance re-decides the run-body loop over the last settled attempt on every
// tick, so the child-shadow window is the wider hazard (⚑S6 freeze). Post-fix the loop runs
// three attempts and seals pass with zero dispatches (exec-only body). Pre-fix it exits after
// one.
func TestAdvanceOptionalUnboundInputClosesChildShadow(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID := "gcg-ins-shadow-pool"
	fake := newFakeWorkStore()
	doc := insShadowDoc(t)

	r, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	for i := 0; i < 40 && !r.Sealed; i++ {
		r, err = engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i+2, err)
		}
	}
	if !r.Sealed {
		t.Fatalf("run did not seal across repeated Advances: %+v", r)
	}
	if r.Run.Outcome != engine.OutcomePass {
		t.Errorf("run outcome = %q, want pass", r.Run.Outcome)
	}
	if fake.dispatchCount() != 0 {
		t.Errorf("DispatchWork calls = %d, want 0 (exec-only body)", fake.dispatchCount())
	}
	if n := countAttemptMinted(r.Run.Events); n != 3 {
		t.Fatalf("pool attempt.minted = %d, want 3 — the child must not shadow the present-null input between decide ticks", n)
	}
}

// --- Step 2 (ga-ospbql): genesis seeding, required-unbound refusal, null renders --------------

// insRequiredDoc declares a REQUIRED, undefaulted root input `token` (strField) with a trivial exec
// body that renders it — the genesis required-unbound refusal shape (⚑B2). It is reused as the
// pre-INS journal fixture for the rebuild-side no-refusal pin, so plant + resume share one ir_hash.
func insRequiredDoc(t *testing.T) *ir.IR {
	t.Helper()
	return decodeIR(t, bundleDoc(strField("token"),
		execNode("greet", `echo "token=[{{ token }}]"`, nil), ""))
}

// TestRunRefusesRequiredUnboundInput pins ⚑B2 surface 1: Run refuses a required-unbound root input
// with ErrRequiredInputUnbound BEFORE any journal append. The sentinel is deliberately DISTINCT
// from ErrInputHashMismatch — an input can hash-match a journal and still be refused at genesis.
func TestRunRefusesRequiredUnboundInput(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	_, err := engine.Run(ctx, store, insRequiredDoc(t), nil) // token omitted
	if !errors.Is(err, engine.ErrRequiredInputUnbound) {
		t.Fatalf("run err = %v, want ErrRequiredInputUnbound", err)
	}
	if errors.Is(err, engine.ErrInputHashMismatch) {
		t.Errorf("required-unbound refusal must NOT be an input-hash mismatch (⚑B2)")
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %v, want it to name the unbound `token`", err)
	}
}

// TestAdvanceFreshRefusesRequiredUnboundInput pins ⚑B2 surface 2: a FRESH Advance (head==0) refuses
// a required-unbound root input pre-run.started, leaving the journal at head 0 (no litter). Then
// supplying the field drives the SAME pristine stream to a clean genesis seal — proving the refusal
// wrote nothing.
func TestAdvanceFreshRefusesRequiredUnboundInput(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	streamID := "gcg-ins-req-fresh"
	doc := insRequiredDoc(t)

	_, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()) // token omitted
	if !errors.Is(err, engine.ErrRequiredInputUnbound) {
		t.Fatalf("advance err = %v, want ErrRequiredInputUnbound", err)
	}
	head, herr := store.Head(ctx, streamID)
	if herr != nil {
		t.Fatalf("head: %v", herr)
	}
	if head != 0 {
		t.Errorf("journal head = %d after refusal, want 0 (no run.started appended)", head)
	}

	r, err := engine.Advance(ctx, store, doc, streamID, map[string]any{"token": "v"}, fake.opts())
	if err != nil {
		t.Fatalf("advance with token: %v", err)
	}
	if !r.Sealed || r.Run.Outcome != engine.OutcomePass {
		t.Errorf("advance with token = %+v, want a sealed pass (fresh genesis after a clean refusal)", r)
	}
	if got := r.Run.NodeOutputs["greet"]; got != "token=[v]" {
		t.Errorf("greet = %q, want token=[v] (the now-bound input rendered)", got)
	}
}

// TestEnqueueRunRefusesRequiredUnboundInput pins ⚑B2 surface 3: EnqueueRun refuses a
// required-unbound input LOUDLY beside its lowering gate — no discoverable run minted (an empty
// stream id), so the controller never re-logs a dead run.
func TestEnqueueRunRefusesRequiredUnboundInput(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	streamID, err := engine.EnqueueRun(ctx, store, insRequiredDoc(t), nil, "packs/x@v1", "workers")
	if !errors.Is(err, engine.ErrRequiredInputUnbound) {
		t.Fatalf("enqueue err = %v, want ErrRequiredInputUnbound (streamID=%q)", err, streamID)
	}
	if streamID != "" {
		t.Errorf("enqueue minted streamID %q, want empty (no discoverable run on refusal)", streamID)
	}
	if !strings.Contains(err.Error(), "token") {
		t.Errorf("err = %v, want it to name the unbound `token`", err)
	}
}

// TestRebuildDoesNotRefuseRequiredUnboundOldJournal is the ⚑B2 rebuild-side MUTANT KILLER: a
// genesis-era (pre-INS) in-flight journal whose run.started declared a now-required root input that
// never bound RESUMES GREEN under the new binary. rebuildDriver DISCARDS the advisory
// required-unbound error (seeding tolerant declared-null), never refuses, and the unbound input
// renders "". A rebuild-side refusal would permanently STRAND every such journal (the enqueue-wedge
// class); this fixture pins its ABSENCE. Because the pre-INS run took no input, its inputHash is ""
// (unpinned), so resume imposes no input constraint and the refusal is never an input-hash mismatch.
func TestRebuildDoesNotRefuseRequiredUnboundOldJournal(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := insRequiredDoc(t)
	streamID := "gcg-ins-oldjournal"
	engine.PlantPreINSJournalForTest(t, store, doc, streamID, nil) // pre-INS: run.started only, unpinned input

	res, err := engine.Resume(ctx, store, doc, streamID, nil, engine.Options{}) // token still unbound
	if err != nil {
		t.Fatalf("resume of a pre-INS required-unbound journal errored (rebuild must NOT refuse): %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("resumed outcome = %q, want pass", res.Outcome)
	}
	if got := res.NodeOutputs["greet"]; got != "token=[]" {
		t.Errorf("greet = %q, want %q (unbound required input renders empty, not stranded)", got, "token=[]")
	}
}

// TestRenderOptionalUnboundRootInputEmpty pins the §4 render/exec consumer row: an exec {{x}} over a
// declared OPTIONAL-unbound root input renders "" (declared-null seeded → baseScope ""), NOT the
// verbatim {{x}} token an undeclared miss would leave. This is the ga-ospbql null-render flip at the
// root; reverting the baseScope nil arm regresses `x` to "null".
func TestRenderOptionalUnboundRootInputEmpty(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(insOptStrField("x"),
		execNode("say", `echo "x=[{{ x }}]"`, nil), ""))
	res, err := engine.Run(ctx, store, doc, nil) // x omitted → declared-null → ""
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["say"]; got != "x=[]" {
		t.Errorf("say = %q, want %q (optional-unbound input renders empty, not the verbatim {{x}} or \"null\")", got, "x=[]")
	}
}

// TestDispatchSubjectDeclaredNullMatchesEmptyArm pins the §4 dispatch-subject consumer row: the
// subject of a declared-null (optional-unbound) input renders "" — an arm matching "" is chosen, an
// arm matching the literal "null" is NOT (the match literal keeps its text). This is the
// accepted-asymmetry row: a declared-null subject is "" at the dispatch, never "null".
func TestDispatchSubjectDeclaredNullMatchesEmptyArm(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	arms := `{"match":{"kind":"literal","value":"null"},"body":` + execNode("armNull", `echo NULLARM`, nil) + `},` +
		`{"match":{"kind":"literal","value":""},"body":` + execNode("armEmpty", `echo EMPTYARM`, nil) + `}`
	dispatch := `{"kind":"dispatch","id":"d","name":"d","after":[],"subject":{"kind":"ref","name":"policy"},"arms":[` + arms + `]}`
	doc := decodeIR(t, bundleDoc(insOptStrField("policy"),
		dispatch+","+execNode("after", `echo "d=[{{ d }}]"`, []string{"d"}), ""))
	res, err := engine.Run(ctx, store, doc, nil) // policy omitted → declared-null → ""
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["armEmpty"] != engine.OutcomePass {
		t.Errorf("armEmpty = %q, want pass (declared-null subject \"\" matched the empty arm)", settled["armEmpty"])
	}
	if _, ok := settled["armNull"]; ok {
		t.Errorf(`the "null"-literal arm ran, want ZERO mints (declared-null renders "", never "null")`)
	}
	if got := res.NodeOutputs["after"]; got != "d=[EMPTYARM]" {
		t.Errorf("after = %q, want d=[EMPTYARM] (dispatch transparent from the empty arm)", got)
	}
}

// insDefaultedArrayField declares an OPTIONAL array input `items` defaulted to ["a","b"] — the
// omitted-defaulted-array shape whose for-each fans the default post-seeding (ga-ospbql).
func insDefaultedArrayField() string {
	return `{"name":"items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":false,"default":["a","b"],"body":false}`
}

// insOptArrayField declares an OPTIONAL, undefaulted array input `items` — the declared-null array
// shape whose for-each stays vacuous.
func insOptArrayField() string {
	return `{"name":"items","type":{"kind":"array","element":{"kind":"atomic","name":"string"}},"required":false,"body":false}`
}

// TestForEachDefaultedArrayInputFansDefault pins the §4 for-each consumer FLIP: a for-each over a
// declared array input with a DEFAULT that is OMITTED now FANS the default's elements — genesis
// seeds the default into d.input (member-over reads d.input) and baseScope (ref-over reads scope).
// Pre-INS the omitted default was unseeded → a vacuous PASS with zero members. Pinned for BOTH the
// bare-ref and the input.<field> member over-forms.
func TestForEachDefaultedArrayInputFansDefault(t *testing.T) {
	for _, tc := range []struct {
		name string
		over string
	}{
		{"ref over-form", refOver("items")},
		{"member over-form", memberOver("items")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			store := newStore(t)
			doc := decodeIR(t, bundleDoc(insDefaultedArrayField(),
				forEachNode(nil, "item", "continue", tc.over,
					execNode("mem", `echo "x={{ item }}"`, nil)), ""))
			res, err := engine.Run(ctx, store, doc, nil) // items omitted → seeded default ["a","b"]
			if err != nil {
				t.Fatalf("run: %v", err)
			}
			if res.Outcome != engine.OutcomePass {
				t.Fatalf("run outcome = %q, want pass", res.Outcome)
			}
			if got := res.NodeOutputs["fan/0"]; got != "x=a" {
				t.Errorf("member 0 = %q, want x=a (defaulted array fanned its elements)", got)
			}
			if got := res.NodeOutputs["fan/1"]; got != "x=b" {
				t.Errorf("member 1 = %q, want x=b", got)
			}
		})
	}
}

// TestForEachDefaultedArrayInputFansDefaultPool is the both-drivers twin: the SAME defaulted-array
// fan driven through Advance (the pool driver's fresh genesis also seeds the default) fans two
// members and seals — an exec-bodied fan runs inline in-pass, so it seals without pool work.
func TestForEachDefaultedArrayInputFansDefaultPool(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	doc := decodeIR(t, bundleDoc(insDefaultedArrayField(),
		forEachNode(nil, "item", "continue", refOver("items"),
			execNode("mem", `echo "x={{ item }}"`, nil)), ""))
	streamID := "gcg-ins-fan-pool"
	r, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	for i := 0; i < 40 && !r.Sealed; i++ {
		r, err = engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
		if err != nil {
			t.Fatalf("advance %d: %v", i+2, err)
		}
	}
	if !r.Sealed || r.Run.Outcome != engine.OutcomePass {
		t.Fatalf("advance = %+v, want a sealed pass", r)
	}
	if got := r.Run.NodeOutputs["fan/0"]; got != "x=a" {
		t.Errorf("pool member 0 = %q, want x=a (Advance genesis seeded the default)", got)
	}
	if got := r.Run.NodeOutputs["fan/1"]; got != "x=b" {
		t.Errorf("pool member 1 = %q, want x=b", got)
	}
}

// TestForEachDeclaredNullInputVacuous pins the §4 companion: a for-each over a declared array input
// that is OPTIONAL-unbound-undefaulted stays a vacuous PASS (declared-null → baseScope "" →
// decodeArrayString empty; member-over → arrayFromInputValue nil arm). Declared-null is NOT a
// defaulted fan, and the downstream consumer runs (no skip-cascade).
func TestForEachDeclaredNullInputVacuous(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(insOptArrayField(),
		forEachNode(nil, "item", "continue", refOver("items"),
			execNode("mem", `echo "x={{ item }}"`, nil))+","+
			execNode("after", `echo done`, []string{"fan"}), ""))
	res, err := engine.Run(ctx, store, doc, nil) // items omitted → declared-null → vacuous
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if res.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass (declared-null over → vacuous)", res.Outcome)
	}
	settled := settledOutcomeByID(t, res.Events)
	if settled["fan"] != engine.OutcomePass {
		t.Errorf("fan = %q, want pass (vacuous)", settled["fan"])
	}
	if _, ok := settled["fan/0"]; ok {
		t.Errorf("fan/0 settled, want ZERO members (declared-null array fans nothing)")
	}
	if settled["after"] != engine.OutcomePass {
		t.Errorf("after = %q, want pass (downstream ran, not skip-cascaded)", settled["after"])
	}
}

// TestEnvBoundNullCrossesRunBoundaryAsEmpty is the e2e null-hop row (ga-ospbql): a null does NOT
// survive a hop. A declared optional-unbound root input `x` (present-null → "") is env-bound into a
// sub-input `y`; the binding renders "" and the child sees a BOUND, PRESENT empty string, so it
// renders "y=[]" — never the verbatim {{y}} of a miss, never "null". Cataloged so nobody "fixes"
// this into null propagation across hops.
func TestEnvBoundNullCrossesRunBoundaryAsEmpty(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	doc := decodeIR(t, bundleDoc(insOptStrField("x"),
		runNodeJSON("hop", nil, "child", "y", "x"),
		subDoc("child", insOptStrField("y"),
			execNode("say", `echo "y=[{{ y }}]"`, nil))))
	res, err := engine.Run(ctx, store, doc, nil) // x omitted → declared-null → "" → bound into y
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := res.NodeOutputs["hop/say"]; got != "y=[]" {
		t.Errorf("child say = %q, want y=[] (a bound null crosses as present-empty, not verbatim or \"null\")", got)
	}
}

// TestAdvancePoolDoReusesSeededPromptNoIdemReuse pins §5 compat (HIGH-1 × seeding): a pool `do`
// whose prompt renders a DEFAULTED root input dispatches with the SEEDED value baked into its
// prompt, and a re-Advance with no new settlement reuses the FOLDED prompt verbatim (never
// re-renders) — so seeding can never trip ErrIdemTokenReuse on the write-once pool node.activated
// (the folded prompt carries input-derived bytes; the re-render no-op is what keeps it stable).
func TestAdvancePoolDoReusesSeededPromptNoIdemReuse(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	defField := `{"name":"who","type":{"kind":"atomic","name":"string"},"required":false,"default":"ada","body":false}`
	doc := decodeIR(t, bundleDoc(defField, doNode("greet", "hello {{ who }}", nil), ""))
	streamID := "gcg-ins-pooldo"

	r1, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts()) // who omitted → seeded "ada"
	if err != nil {
		t.Fatalf("advance 1: %v", err)
	}
	if !r1.Parked || len(r1.InFlight) != 1 {
		t.Fatalf("advance 1 = %+v, want parked with 1 in-flight pool do", r1)
	}
	if got := r1.InFlight[0].Prompt; got != "hello ada" {
		t.Errorf("dispatched prompt = %q, want 'hello ada' (seeded default baked into the prompt)", got)
	}
	// Re-Advance with nothing settled: the folded prompt is reused, no re-render, no idem-reuse.
	r2, err := engine.Advance(ctx, store, doc, streamID, nil, fake.opts())
	if err != nil {
		t.Fatalf("re-advance re-rendered / tripped idem reuse over a seeded prompt: %v", err)
	}
	if !r2.Parked || len(r2.InFlight) != 1 || r2.InFlight[0].Prompt != "hello ada" {
		t.Errorf("re-advance = %+v, want the same parked in-flight prompt (folded, not re-rendered)", r2)
	}
	if fake.dispatchCount() != 1 {
		t.Errorf("DispatchWork calls = %d, want 1 (an in-flight do must be a HIGH-1 no-op)", fake.dispatchCount())
	}
}
