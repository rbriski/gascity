package dispatch

import (
	"slices"
	"strconv"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/formula"
)

// total returns the number of settlement emits the spy recorded across all three
// buckets. Coarse means coarse, so a single settle path should add exactly one.
func (s *spySettlementEmitter) total() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.root) + len(s.attempt) + len(s.workflow)
}

// assertAllEnginesV2 fails if any recorded emit rode an engine other than v2.
// Dispatch processes only graph.v2 control beads, so every coarse emit from
// ProcessControl is definitionally v2 (settlement.go).
func assertAllEnginesV2(t *testing.T, spy *spySettlementEmitter) {
	t.Helper()
	spy.mu.Lock()
	defer spy.mu.Unlock()
	for _, e := range spy.engines {
		if e != beads.SettlementEngineV2 {
			t.Fatalf("emit engine = %q, want v2", e)
		}
	}
}

// --- self-eval retry (processRetryControl / KindRetry) anchor tests ----------

// retrySelfEvalFixture builds a self-evaluating retry control bead (the control
// bead IS the logical step) plus one closed attempt carrying the given outcome
// metadata, wired so processRetryControl evaluates it inline.
func retrySelfEvalFixture(t *testing.T, attemptMeta map[string]string, maxAttempts int) (store beads.Store, root, control beads.Bead) {
	t.Helper()
	store = beads.NewMemStore()
	spec := &formula.Step{ID: "review", Title: "Review", Type: "task", Retry: &formula.RetrySpec{MaxAttempts: maxAttempts}}
	root, control = makeRetryControl(t, store, "mol-test.review", spec, maxAttempts)
	attempt1 := makeAttemptBead(t, store, root.ID, "mol-test.review.attempt.1", 1, attemptMeta)
	mustDep(t, store, control.ID, attempt1.ID, "blocks")
	return store, root, control
}

func TestRetryControlEmitsAttemptOnPass(t *testing.T) {
	t.Parallel()
	store, root, control := retrySelfEvalFixture(t, map[string]string{"gc.outcome": "pass"}, 3)
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(retry pass): %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("action = %q, want pass", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != control.ID || got.Outcome != beadmeta.OutcomePass || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=pass attempt=1}", got, root.ID, control.ID)
	}
	assertAllEnginesV2(t, spy)
	assertRetryControlNilInert(t, map[string]string{"gc.outcome": "pass"}, 3, "pass", beadmeta.OutcomePass)
}

func TestRetryControlEmitsAttemptOnHardFail(t *testing.T) {
	t.Parallel()
	store, root, control := retrySelfEvalFixture(t, map[string]string{
		"gc.outcome": "fail", "gc.failure_class": "hard", "gc.failure_reason": "boom",
	}, 3)
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(retry hard): %v", err)
	}
	if result.Action != "hard-fail" {
		t.Fatalf("action = %q, want hard-fail", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != control.ID || got.Outcome != beadmeta.OutcomeFail || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=fail attempt=1}", got, root.ID, control.ID)
	}
	assertAllEnginesV2(t, spy)
	assertRetryControlNilInert(t, map[string]string{"gc.outcome": "fail", "gc.failure_class": "hard"}, 3, "hard-fail", beadmeta.OutcomeFail)
}

func TestRetryControlEmitsAttemptOnExhaustion(t *testing.T) {
	t.Parallel()
	// maxAttempts=1 with a transient failure exhausts immediately → handleRetryExhaustion.
	store, root, control := retrySelfEvalFixture(t, map[string]string{
		"gc.outcome": "fail", "gc.failure_class": "transient", "gc.failure_reason": "timeout",
	}, 1)
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(retry exhausted): %v", err)
	}
	if result.Action != "fail" {
		t.Fatalf("action = %q, want fail (exhausted)", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != control.ID || got.Outcome != beadmeta.OutcomeFail || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=fail attempt=1}", got, root.ID, control.ID)
	}
	assertAllEnginesV2(t, spy)
	assertRetryControlNilInert(t, map[string]string{"gc.outcome": "fail", "gc.failure_class": "transient"}, 1, "fail", beadmeta.OutcomeFail)
}

// assertRetryControlNilInert proves the anchor is strictly after-the-fact: with a
// nil emitter the control result and the control bead's terminal outcome are
// unchanged from the spy run.
func assertRetryControlNilInert(t *testing.T, attemptMeta map[string]string, maxAttempts int, wantAction, wantOutcome string) {
	t.Helper()
	store, _, control := retrySelfEvalFixture(t, attemptMeta, maxAttempts)
	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{Settlements: nil})
	if err != nil {
		t.Fatalf("ProcessControl(nil emitter): %v", err)
	}
	if result.Action != wantAction {
		t.Fatalf("nil-emitter action = %q, want %q", result.Action, wantAction)
	}
	after := mustGet(t, store, control.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != wantOutcome {
		t.Fatalf("nil-emitter control = %s/%s, want closed/%s", after.Status, after.Metadata["gc.outcome"], wantOutcome)
	}
}

// --- self-eval ralph (processRalphControl / KindRalph) anchor tests ----------

// ralphSelfEvalPassFixture builds a ralph control whose latest iteration passes
// its exec check (a real exit-0 script), so processRalphControl settles it pass.
func ralphSelfEvalPassFixture(t *testing.T) (store beads.Store, root, control beads.Bead, opts ProcessOptions) {
	t.Helper()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "pass-check.sh", "#!/bin/sh\nexit 0\n")
	store = beads.NewMemStore()
	root = mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control = mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop",
			"gc.step_id":      "review-loop",
			"gc.check_path":   checkPath,
			"gc.max_attempts": "3",
		},
	})
	iteration := mustCreate(t, store, beads.Bead{
		Title: "review loop iteration 1",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.1",
			"gc.attempt":      "1",
		},
	})
	mustClose(t, store, iteration.ID)
	mustDep(t, store, control.ID, iteration.ID, "blocks")
	return store, root, control, ProcessOptions{CityPath: cityPath}
}

// ralphSelfEvalExhaustedFixture builds a ralph control at its last attempt whose
// iteration already failed, so processRalphControl settles it fail (exhausted)
// without needing an exec check script.
func ralphSelfEvalExhaustedFixture(t *testing.T) (store beads.Store, root, control beads.Bead) {
	t.Helper()
	store = beads.NewMemStore()
	root = mustCreate(t, store, beads.Bead{Title: "workflow", Metadata: map[string]string{"gc.kind": "workflow"}})
	control = mustCreate(t, store, beads.Bead{
		Title: "review loop",
		Metadata: map[string]string{
			"gc.kind":         "ralph",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop",
			"gc.step_id":      "review-loop",
			"gc.max_attempts": "1",
		},
	})
	iteration := mustCreate(t, store, beads.Bead{
		Title: "review loop iteration 1",
		Metadata: map[string]string{
			"gc.kind":         "scope",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "mol-test.review-loop.iteration.1",
			"gc.attempt":      "1",
			"gc.outcome":      "fail",
		},
	})
	mustClose(t, store, iteration.ID)
	mustDep(t, store, control.ID, iteration.ID, "blocks")
	return store, root, control
}

func TestRalphControlEmitsAttemptOnPass(t *testing.T) {
	t.Parallel()
	store, root, control, opts := ralphSelfEvalPassFixture(t)
	spy := &spySettlementEmitter{}
	opts.Settlements = spy

	result, err := ProcessControl(store, mustGet(t, store, control.ID), opts)
	if err != nil {
		t.Fatalf("ProcessControl(ralph pass): %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("action = %q, want pass", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != control.ID || got.Outcome != beadmeta.OutcomePass || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=pass attempt=1}", got, root.ID, control.ID)
	}
	assertAllEnginesV2(t, spy)

	// nil-emitter inert: same terminal pass without an emitter.
	nilStore, _, nilControl, nilOpts := ralphSelfEvalPassFixture(t)
	nilResult, err := ProcessControl(nilStore, mustGet(t, nilStore, nilControl.ID), nilOpts)
	if err != nil {
		t.Fatalf("ProcessControl(nil emitter): %v", err)
	}
	if nilResult.Action != "pass" {
		t.Fatalf("nil-emitter action = %q, want pass", nilResult.Action)
	}
	after := mustGet(t, nilStore, nilControl.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != beadmeta.OutcomePass {
		t.Fatalf("nil-emitter control = %s/%s, want closed/pass", after.Status, after.Metadata["gc.outcome"])
	}
}

func TestRalphControlEmitsAttemptOnExhaustedFail(t *testing.T) {
	t.Parallel()
	store, root, control := ralphSelfEvalExhaustedFixture(t)
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, mustGet(t, store, control.ID), ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(ralph exhausted): %v", err)
	}
	if result.Action != "fail" {
		t.Fatalf("action = %q, want fail", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != control.ID || got.Outcome != beadmeta.OutcomeFail || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=fail attempt=1}", got, root.ID, control.ID)
	}
	assertAllEnginesV2(t, spy)

	// nil-emitter inert.
	nilStore, _, nilControl := ralphSelfEvalExhaustedFixture(t)
	nilResult, err := ProcessControl(nilStore, mustGet(t, nilStore, nilControl.ID), ProcessOptions{Settlements: nil})
	if err != nil {
		t.Fatalf("ProcessControl(nil emitter): %v", err)
	}
	if nilResult.Action != "fail" {
		t.Fatalf("nil-emitter action = %q, want fail", nilResult.Action)
	}
	after := mustGet(t, nilStore, nilControl.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != beadmeta.OutcomeFail {
		t.Fatalf("nil-emitter control = %s/%s, want closed/fail", after.Status, after.Metadata["gc.outcome"])
	}
}

// --- separate-eval retry (processRetryEval / KindRetryEval) anchor tests -----

// retryEvalFixture builds the separate-eval retry topology: a logical retry bead
// blocked by a retry-eval control bead, itself blocked by a closed retry-run
// subject carrying the given outcome metadata.
func retryEvalFixture(t *testing.T, runMeta map[string]string, maxAttempts int, onExhausted string) (store beads.Store, root, logical, eval beads.Bead) {
	t.Helper()
	store = newStrictCloseStore()
	root = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "workflow", Type: "task",
		Metadata: map[string]string{"gc.kind": "workflow", "gc.formula_contract": "graph.v2"},
	})
	logical = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review", Type: "task",
		Metadata: map[string]string{
			"gc.kind":         "retry",
			"gc.root_bead_id": root.ID,
			"gc.step_ref":     "demo.review",
			"gc.max_attempts": strconv.Itoa(maxAttempts),
			"gc.on_exhausted": onExhausted,
		},
	})
	runBase := map[string]string{
		"gc.kind":            "retry-run",
		"gc.root_bead_id":    root.ID,
		"gc.logical_bead_id": logical.ID,
		"gc.attempt":         "1",
		"gc.max_attempts":    strconv.Itoa(maxAttempts),
		"gc.on_exhausted":    onExhausted,
	}
	for k, v := range runMeta {
		runBase[k] = v
	}
	run1 := mustCreateWorkflowBead(t, store, beads.Bead{Title: "review attempt 1", Type: "task", Status: "closed", Metadata: runBase})
	eval = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "review eval 1", Type: "task",
		Metadata: map[string]string{
			"gc.kind":            "retry-eval",
			"gc.root_bead_id":    root.ID,
			"gc.logical_bead_id": logical.ID,
			"gc.attempt":         "1",
			"gc.max_attempts":    strconv.Itoa(maxAttempts),
			"gc.on_exhausted":    onExhausted,
		},
	})
	mustDepAdd(t, store, logical.ID, eval.ID, "blocks")
	mustDepAdd(t, store, eval.ID, run1.ID, "blocks")
	return store, root, logical, eval
}

func TestRetryEvalEmitsAttemptOnHardFail(t *testing.T) {
	t.Parallel()
	store, root, logical, eval := retryEvalFixture(t, map[string]string{
		"gc.outcome": "fail", "gc.failure_class": "hard", "gc.failure_reason": "boom",
	}, 3, "hard_fail")
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, mustGet(t, store, eval.ID), ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval hard): %v", err)
	}
	if result.Action != "hard-fail" {
		t.Fatalf("action = %q, want hard-fail", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != logical.ID || got.Outcome != beadmeta.OutcomeFail || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=fail attempt=1}", got, root.ID, logical.ID)
	}
	assertAllEnginesV2(t, spy)
	assertRetryEvalNilInert(t, map[string]string{"gc.outcome": "fail", "gc.failure_class": "hard"}, 3, "hard_fail", "hard-fail", beadmeta.OutcomeFail)
}

func TestRetryEvalEmitsAttemptOnSoftFailExhaustion(t *testing.T) {
	t.Parallel()
	store, root, logical, eval := retryEvalFixture(t, map[string]string{
		"gc.outcome": "fail", "gc.failure_class": "transient", "gc.failure_reason": "flaky",
	}, 1, "soft_fail")
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, mustGet(t, store, eval.ID), ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval soft): %v", err)
	}
	if result.Action != "soft-fail" {
		t.Fatalf("action = %q, want soft-fail", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	// A soft-fail resolves the logical step to gc.outcome=pass.
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != logical.ID || got.Outcome != beadmeta.OutcomePass || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=pass attempt=1}", got, root.ID, logical.ID)
	}
	assertAllEnginesV2(t, spy)
	assertRetryEvalNilInert(t, map[string]string{"gc.outcome": "fail", "gc.failure_class": "transient"}, 1, "soft_fail", "soft-fail", beadmeta.OutcomePass)
}

func TestRetryEvalEmitsAttemptOnHardExhaustion(t *testing.T) {
	t.Parallel()
	store, root, logical, eval := retryEvalFixture(t, map[string]string{
		"gc.outcome": "fail", "gc.failure_class": "transient", "gc.failure_reason": "flaky",
	}, 1, "hard_fail")
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, mustGet(t, store, eval.ID), ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(retry-eval exhausted hard): %v", err)
	}
	if result.Action != "fail" {
		t.Fatalf("action = %q, want fail", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Root != root.ID || got.Bead != logical.ID || got.Outcome != beadmeta.OutcomeFail || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {root=%s bead=%s outcome=fail attempt=1}", got, root.ID, logical.ID)
	}
	assertAllEnginesV2(t, spy)
	assertRetryEvalNilInert(t, map[string]string{"gc.outcome": "fail", "gc.failure_class": "transient"}, 1, "hard_fail", "fail", beadmeta.OutcomeFail)
}

func assertRetryEvalNilInert(t *testing.T, runMeta map[string]string, maxAttempts int, onExhausted, wantAction, wantOutcome string) {
	t.Helper()
	store, _, logical, eval := retryEvalFixture(t, runMeta, maxAttempts, onExhausted)
	result, err := ProcessControl(store, mustGet(t, store, eval.ID), ProcessOptions{Settlements: nil})
	if err != nil {
		t.Fatalf("ProcessControl(nil emitter): %v", err)
	}
	if result.Action != wantAction {
		t.Fatalf("nil-emitter action = %q, want %q", result.Action, wantAction)
	}
	after := mustGet(t, store, logical.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != wantOutcome {
		t.Fatalf("nil-emitter logical = %s/%s, want closed/%s", after.Status, after.Metadata["gc.outcome"], wantOutcome)
	}
}

// --- separate check (processRalphCheck / KindCheck) anchor tests -------------

func TestCheckEmitsAttemptOnPass(t *testing.T) {
	t.Parallel()
	cityPath := t.TempDir()
	checkPath := writeCheckScript(t, cityPath, "pass-check.sh", "#!/bin/sh\nexit 0\n")
	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
	if err := store.SetMetadata(run1.ID, "gc.outcome", "pass"); err != nil {
		t.Fatalf("set run outcome: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, check1, ProcessOptions{CityPath: cityPath, Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(check pass): %v", err)
	}
	if result.Action != "pass" {
		t.Fatalf("action = %q, want pass", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Bead != logical.ID || got.Outcome != beadmeta.OutcomePass || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {bead=%s outcome=pass attempt=1}", got, logical.ID)
	}
	assertAllEnginesV2(t, spy)
}

func TestCheckEmitsAttemptOnExhaustedFail(t *testing.T) {
	t.Parallel()
	// maxAttempts=1 and a subject that already failed → runRalphCheck returns
	// GateFail without an exec script, and attempt 1 == maxAttempts exhausts.
	store, logical, run1, check1 := newSimpleRalphLoop(t, "implement", "unused.sh", 1)
	if err := store.SetMetadata(run1.ID, "gc.outcome", "fail"); err != nil {
		t.Fatalf("set run outcome: %v", err)
	}
	if err := store.Close(run1.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, check1, ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(check exhausted): %v", err)
	}
	if result.Action != "fail" {
		t.Fatalf("action = %q, want fail", result.Action)
	}
	if spy.total() != 1 || len(spy.attempt) != 1 {
		t.Fatalf("emits = %d (attempt=%d), want exactly one attempt settlement", spy.total(), len(spy.attempt))
	}
	got := spy.attempt[0]
	if got.Bead != logical.ID || got.Outcome != beadmeta.OutcomeFail || got.Attempt != 1 {
		t.Fatalf("attempt = %+v, want {bead=%s outcome=fail attempt=1}", got, logical.ID)
	}
	assertAllEnginesV2(t, spy)

	// nil-emitter inert on the check fail path.
	nilStore, nilLogical, nilRun, nilCheck := newSimpleRalphLoop(t, "implement", "unused.sh", 1)
	if err := nilStore.SetMetadata(nilRun.ID, "gc.outcome", "fail"); err != nil {
		t.Fatalf("set run outcome: %v", err)
	}
	if err := nilStore.Close(nilRun.ID); err != nil {
		t.Fatalf("close run1: %v", err)
	}
	nilResult, err := ProcessControl(nilStore, nilCheck, ProcessOptions{Settlements: nil})
	if err != nil {
		t.Fatalf("ProcessControl(nil emitter): %v", err)
	}
	if nilResult.Action != "fail" {
		t.Fatalf("nil-emitter action = %q, want fail", nilResult.Action)
	}
	after := mustGetBead(t, nilStore, nilLogical.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != beadmeta.OutcomeFail {
		t.Fatalf("nil-emitter logical = %s/%s, want closed/fail", after.Status, after.Metadata["gc.outcome"])
	}
}

// --- workflow-finalize missing_root arm --------------------------------------

// finalizeMissingRootFixture builds a finalizer whose gc.root_bead_id points at a
// bead that does not exist, so the root close fails ErrNotFound and the finalizer
// takes the missing_root arm.
func finalizeMissingRootFixture(t *testing.T, store beads.Store) (ghostRoot string, finalizer beads.Bead) {
	t.Helper()
	ghostRoot = "gcg-ghost-root"
	cleanup := mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "cleanup", Type: "task", Status: "closed",
		Metadata: map[string]string{"gc.outcome": "fail"},
	})
	finalizer = mustCreateWorkflowBead(t, store, beads.Bead{
		Title: "Finalize workflow", Type: "task",
		Metadata: map[string]string{
			"gc.kind":         "workflow-finalize",
			"gc.root_bead_id": ghostRoot,
		},
	})
	mustDepAdd(t, store, finalizer.ID, cleanup.ID, "blocks")
	return ghostRoot, finalizer
}

func TestFinalizeMissingRootEmitsWorkflow(t *testing.T) {
	t.Parallel()
	store := beads.NewMemStore()
	ghostRoot, finalizer := finalizeMissingRootFixture(t, store)
	spy := &spySettlementEmitter{}

	result, err := ProcessControl(store, finalizer, ProcessOptions{Settlements: spy})
	if err != nil {
		t.Fatalf("ProcessControl(finalize missing_root): %v", err)
	}
	if result.Action != "workflow-missing_root" {
		t.Fatalf("action = %q, want workflow-missing_root", result.Action)
	}
	// Coarse: exactly one workflow.finalized settlement, no root settlement (the
	// root never reached a terminal column write — it does not exist).
	if spy.total() != 1 || len(spy.workflow) != 1 || len(spy.root) != 0 {
		t.Fatalf("emits total=%d workflow=%d root=%d, want one workflow.finalized only", spy.total(), len(spy.workflow), len(spy.root))
	}
	got := spy.workflow[0]
	if got.Root != ghostRoot || got.Bead != finalizer.ID || got.Outcome != beadmeta.OutcomeMissingRoot {
		t.Fatalf("workflow = %+v, want {root=%s bead=%s outcome=missing_root}", got, ghostRoot, finalizer.ID)
	}
	assertAllEnginesV2(t, spy)

	// nil-emitter inert on the missing_root path.
	nilStore := beads.NewMemStore()
	_, nilFinal := finalizeMissingRootFixture(t, nilStore)
	nilResult, err := ProcessControl(nilStore, nilFinal, ProcessOptions{Settlements: nil})
	if err != nil {
		t.Fatalf("ProcessControl(nil emitter): %v", err)
	}
	if nilResult.Action != "workflow-missing_root" {
		t.Fatalf("nil-emitter action = %q, want workflow-missing_root", nilResult.Action)
	}
	after := mustGetBead(t, nilStore, nilFinal.ID)
	if after.Status != "closed" || after.Metadata["gc.outcome"] != beadmeta.OutcomeMissingRoot {
		t.Fatalf("nil-emitter finalizer = %s/%s, want closed/missing_root", after.Status, after.Metadata["gc.outcome"])
	}
}

// --- LOCKSTEP: emitting handlers must equal the cmd-wired kind set -----------

// settlementLockstepCase drives one control kind to a genuine terminal
// settlement through ProcessControl, so the lockstep test can assert the kind
// actually emits.
type settlementLockstepCase struct {
	build     func(t *testing.T, spy *spySettlementEmitter) (beads.Store, beads.Bead, ProcessOptions)
	wantEmits int
}

// settlementLockstepCases maps every kind in dispatch.SettlementEmittingKinds to
// a fixture that reaches a real settlement. TestSettlementEmittingKindsTrackHandlers
// asserts this table's key set EXACTLY equals SettlementEmittingKinds and that
// each fixture emits, so the HIGH-1 class fails the build both ways: a kind wired
// for an emitter whose handler never emits (its fixture would record zero emits),
// and an anchor added to a handler without listing its kind (its key would be
// absent from SettlementEmittingKinds).
func settlementLockstepCases() map[string]settlementLockstepCase {
	return map[string]settlementLockstepCase{
		beadmeta.KindCheck: {wantEmits: 1, build: func(t *testing.T, spy *spySettlementEmitter) (beads.Store, beads.Bead, ProcessOptions) {
			cityPath := t.TempDir()
			checkPath := writeCheckScript(t, cityPath, "pass-check.sh", "#!/bin/sh\nexit 0\n")
			store, _, run1, check1 := newSimpleRalphLoop(t, "implement", checkPath, 3)
			if err := store.SetMetadata(run1.ID, "gc.outcome", "pass"); err != nil {
				t.Fatalf("set run outcome: %v", err)
			}
			if err := store.Close(run1.ID); err != nil {
				t.Fatalf("close run1: %v", err)
			}
			return store, check1, ProcessOptions{CityPath: cityPath, Settlements: spy}
		}},
		beadmeta.KindRetryEval: {wantEmits: 1, build: func(t *testing.T, spy *spySettlementEmitter) (beads.Store, beads.Bead, ProcessOptions) {
			store, _, _, eval := retryEvalFixture(t, map[string]string{"gc.outcome": "pass"}, 3, "hard_fail")
			return store, mustGet(t, store, eval.ID), ProcessOptions{Settlements: spy}
		}},
		beadmeta.KindRetry: {wantEmits: 1, build: func(t *testing.T, spy *spySettlementEmitter) (beads.Store, beads.Bead, ProcessOptions) {
			store, _, control := retrySelfEvalFixture(t, map[string]string{"gc.outcome": "pass"}, 3)
			return store, mustGet(t, store, control.ID), ProcessOptions{Settlements: spy}
		}},
		beadmeta.KindRalph: {wantEmits: 1, build: func(t *testing.T, spy *spySettlementEmitter) (beads.Store, beads.Bead, ProcessOptions) {
			store, _, control := ralphSelfEvalExhaustedFixture(t)
			return store, mustGet(t, store, control.ID), ProcessOptions{Settlements: spy}
		}},
		beadmeta.KindWorkflowFinalize: {wantEmits: 2, build: func(t *testing.T, spy *spySettlementEmitter) (beads.Store, beads.Bead, ProcessOptions) {
			store := beads.NewMemStore()
			_, finalizer := finalizeFixture(t, store)
			return store, finalizer, ProcessOptions{Settlements: spy}
		}},
	}
}

func TestSettlementEmittingKindsTrackHandlers(t *testing.T) {
	t.Parallel()
	cases := settlementLockstepCases()

	// 1. The fixture table's key set == SettlementEmittingKinds. This is the
	// structural equality that would have caught HIGH-1: a kind added to the
	// emitting set without a settling fixture (or vice versa) fails here.
	wantKeys := append([]string(nil), SettlementEmittingKinds...)
	slices.Sort(wantKeys)
	gotKeys := make([]string, 0, len(cases))
	for k := range cases {
		gotKeys = append(gotKeys, k)
	}
	slices.Sort(gotKeys)
	if !slices.Equal(gotKeys, wantKeys) {
		t.Fatalf("lockstep fixture kinds = %v, want SettlementEmittingKinds = %v", gotKeys, wantKeys)
	}

	// 2. Every listed kind genuinely emits when driven to a terminal settlement,
	// and EmitsSettlement agrees. A handler that stopped emitting (the KindRalph
	// bug shape) would record zero emits here.
	for kind, tc := range cases {
		t.Run("emits/"+kind, func(t *testing.T) {
			if !EmitsSettlement(kind) {
				t.Fatalf("EmitsSettlement(%q) = false, want true (kind is in the fixture table)", kind)
			}
			spy := &spySettlementEmitter{}
			store, bead, opts := tc.build(t, spy)
			if _, err := ProcessControl(store, bead, opts); err != nil {
				t.Fatalf("ProcessControl(%q): %v", kind, err)
			}
			if spy.total() != tc.wantEmits {
				t.Fatalf("kind %q emitted %d settlements, want %d", kind, spy.total(), tc.wantEmits)
			}
			assertAllEnginesV2(t, spy)
		})
	}

	// 3. Every control kind NOT in the emitting set must report EmitsSettlement
	// false — so the cmd control dispatcher never builds a wasted emitter for a
	// non-settling kind (fanout, drain, scope-check).
	for _, kind := range beadmeta.ControlKinds {
		inSet := slices.Contains(SettlementEmittingKinds, kind)
		if EmitsSettlement(kind) != inSet {
			t.Fatalf("EmitsSettlement(%q) = %v, want %v", kind, EmitsSettlement(kind), inSet)
		}
	}

	// 4. Every emitting kind is a real control kind (no orphan entries).
	for _, kind := range SettlementEmittingKinds {
		if !beadmeta.IsControlKind(kind) {
			t.Fatalf("SettlementEmittingKinds contains %q, which is not a ControlKind", kind)
		}
	}
}

// TestNonSettlingControlKindsDoNotEmit drives the non-emitting control kinds
// through ProcessControl with a spy and asserts zero settlements — the negative
// half of emit-iff-wired. Minimal beads make these handlers error before any
// terminal settle, which is exactly the point: they never emit.
func TestNonSettlingControlKindsDoNotEmit(t *testing.T) {
	t.Parallel()
	for _, kind := range beadmeta.ControlKinds {
		if EmitsSettlement(kind) {
			continue
		}
		t.Run(kind, func(t *testing.T) {
			store := beads.NewMemStore()
			bead, err := store.Create(beads.Bead{
				Title:    "non-settling probe " + kind,
				Metadata: map[string]string{beadmeta.KindMetadataKey: kind},
			})
			if err != nil {
				t.Fatalf("create: %v", err)
			}
			spy := &spySettlementEmitter{}
			// The handler may error on the minimal bead; we only care that it
			// never emitted a settlement.
			_, _ = ProcessControl(store, mustGet(t, store, bead.ID), ProcessOptions{Settlements: spy})
			if spy.total() != 0 {
				t.Fatalf("non-settling kind %q emitted %d settlements, want 0", kind, spy.total())
			}
		})
	}
}
