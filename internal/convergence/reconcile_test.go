package convergence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// --- Test helpers ---

// setupReconciler creates a Reconciler with a fresh fakeStore and fakeEmitter.
// The returned store starts empty — callers add beads as needed.
func setupReconciler(t *testing.T) (*Reconciler, *fakeStore, *fakeEmitter) {
	t.Helper()
	store := newFakeStore()
	emitter := &fakeEmitter{}
	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}
	return &Reconciler{Handler: handler}, store, emitter
}

func completeEmptyStateMetadata() map[string]string {
	return map[string]string{
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldMaxIterations:     "5",
		FieldIteration:         "1",
		FieldGateMode:          GateModeManual,
		FieldGateCondition:     "",
		FieldGateTimeout:       "",
		FieldGateTimeoutAction: "",
		FieldTrigger:           TriggerNone,
		FieldTriggerCondition:  "",
	}
}

func cloneMetadata(meta map[string]string) map[string]string {
	cloned := make(map[string]string, len(meta))
	for key, value := range meta {
		cloned[key] = value
	}
	return cloned
}

// --- Path 3t: waiting_trigger ---

func TestReconcile_WaitingTrigger_NoAction(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:            StateWaitingTrigger,
		FieldFormula:          "test-formula",
		FieldMaxIterations:    "5",
		FieldTarget:           "test-agent",
		FieldTrigger:          TriggerEvent,
		FieldTriggerCondition: "/scripts/check",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Details) != 1 || report.Details[0].Action != ActionNoAction {
		t.Errorf("action = %+v, want no_action", report.Details)
	}
	// State must be preserved so the tick re-evaluates the trigger.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateWaitingTrigger {
		t.Errorf("state = %q, want %q", meta[FieldState], StateWaitingTrigger)
	}
}

func TestReconcile_WaitingTrigger_CompletesInterruptedStop(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateWaitingTrigger,
		FieldFormula:        "test-formula",
		FieldMaxIterations:  "5",
		FieldTarget:         "test-agent",
		FieldTrigger:        TriggerEvent,
		FieldTerminalReason: TerminalStopped,
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(report.Details) != 1 || report.Details[0].Action != ActionCompletedTerminal {
		t.Errorf("action = %+v, want completed_terminal", report.Details)
	}
	info, _ := store.GetBead("root-1")
	if info.Status != "closed" {
		t.Errorf("status = %q, want closed", info.Status)
	}
}

// --- Path 1: Missing state ---

func TestReconcile_MissingState_NoWisps_PoursFirst(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// Root bead with complete creation metadata but no convergence.state set.
	store.addBead("root-1", "in_progress", "", "", completeEmptyStateMetadata())

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Scanned != 1 {
		t.Errorf("Scanned = %d, want 1", report.Scanned)
	}
	if report.Recovered != 1 {
		t.Errorf("Recovered = %d, want 1", report.Recovered)
	}
	if report.Errors != 0 {
		t.Errorf("Errors = %d, want 0", report.Errors)
	}

	d := report.Details[0]
	if d.Action != ActionPouredWisp {
		t.Errorf("Action = %q, want %q", d.Action, ActionPouredWisp)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// Verify state was set to active.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
	if meta[FieldActiveWisp] == "" {
		t.Error("active_wisp should be set after pouring")
	}
}

func TestReconcile_MissingState_WispExists_Adopts(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", completeEmptyStateMetadata())

	// Pre-existing wisp for iteration 1.
	key1 := IdempotencyKey("root-1", 1)
	store.addBead("existing-wisp", "in_progress", "root-1", key1, nil)
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		t.Fatal("complete empty-state recovery must adopt the existing iteration-1 wisp, not pour")
		return "", nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionAdoptedWisp {
		t.Errorf("Action = %q, want %q", d.Action, ActionAdoptedWisp)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateActive {
		t.Errorf("state = %q, want %q", meta[FieldState], StateActive)
	}
	if meta[FieldActiveWisp] != "existing-wisp" {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], "existing-wisp")
	}
}

func TestReconcile_ActiveWithoutMarkerAdoptsEarliestClosedChild(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:         StateActive,
		FieldIteration:     "0",
		FieldMaxIterations: "5",
		FieldFormula:       "test-formula",
		FieldTarget:        "test-agent",
		FieldGateMode:      GateModeManual,
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.PourWispFunc = func(_, _, key string, _ map[string]string, _ string) (string, error) {
		t.Fatalf("marker-less recovery must adopt earliest closed child, not pour %q", key)
		return "", nil
	}
	store.ActivateWispFunc = func(id string) error {
		t.Fatalf("closed child %q must not be activated", id)
		return nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 0 || report.Details[0].Action != ActionAdoptedWisp {
		t.Fatalf("report = %+v, want closed-child adoption", report)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldActiveWisp] != "wisp-iter-1" || meta[FieldLastProcessedWisp] != "" || meta[FieldState] != StateActive {
		t.Fatalf("metadata after adoption = %#v, want closed iteration 1 left for legacy tick owner", meta)
	}
}

func TestReconcile_ActiveWithoutMarkerRejectsIterationGap(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:         StateActive,
		FieldIteration:     "0",
		FieldMaxIterations: "5",
		FieldFormula:       "test-formula",
		FieldTarget:        "test-agent",
		FieldGateMode:      GateModeManual,
	})
	store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)
	store.PourWispFunc = func(_, _, key string, _ map[string]string, _ string) (string, error) {
		t.Fatalf("marker-less recovery with an iteration gap must fail closed, not pour %q", key)
		return "", nil
	}
	store.ActivateWispFunc = func(id string) error {
		t.Fatalf("marker-less recovery with an iteration gap must not activate %q", id)
		return nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 1 || report.Details[0].Error == nil || !strings.Contains(report.Details[0].Error.Error(), "iteration gap") {
		t.Fatalf("report = %+v, want iteration-gap corruption error", report)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldActiveWisp] != "" || meta[FieldLastProcessedWisp] != "" || meta[FieldState] != StateActive {
		t.Fatalf("metadata after rejected gap = %#v, want unchanged active root", meta)
	}
}

func TestReconcile_MissingState_IncompleteCreationMetadataTerminatesWithoutPour(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(map[string]string)
	}{
		{name: "missing formula", mutate: func(meta map[string]string) { delete(meta, FieldFormula) }},
		{name: "empty formula", mutate: func(meta map[string]string) { meta[FieldFormula] = "" }},
		{name: "missing target", mutate: func(meta map[string]string) { delete(meta, FieldTarget) }},
		{name: "empty target", mutate: func(meta map[string]string) { meta[FieldTarget] = "" }},
		{name: "missing max iterations", mutate: func(meta map[string]string) { delete(meta, FieldMaxIterations) }},
		{name: "invalid max iterations", mutate: func(meta map[string]string) { meta[FieldMaxIterations] = "many" }},
		{name: "zero max iterations", mutate: func(meta map[string]string) { meta[FieldMaxIterations] = "0" }},
		{name: "negative max iterations", mutate: func(meta map[string]string) { meta[FieldMaxIterations] = "-1" }},
		{name: "missing iteration", mutate: func(meta map[string]string) { delete(meta, FieldIteration) }},
		{name: "invalid iteration", mutate: func(meta map[string]string) { meta[FieldIteration] = "first" }},
		{name: "negative iteration", mutate: func(meta map[string]string) { meta[FieldIteration] = "-1" }},
		{name: "iteration exceeds max", mutate: func(meta map[string]string) { meta[FieldIteration] = "6" }},
		{name: "missing gate mode", mutate: func(meta map[string]string) { delete(meta, FieldGateMode) }},
		{name: "empty gate mode", mutate: func(meta map[string]string) { meta[FieldGateMode] = "" }},
		{name: "invalid gate mode", mutate: func(meta map[string]string) { meta[FieldGateMode] = "automatic" }},
		{name: "missing gate condition", mutate: func(meta map[string]string) { delete(meta, FieldGateCondition) }},
		{name: "missing gate timeout", mutate: func(meta map[string]string) { delete(meta, FieldGateTimeout) }},
		{name: "invalid gate timeout", mutate: func(meta map[string]string) { meta[FieldGateTimeout] = "eventually" }},
		{name: "missing gate timeout action", mutate: func(meta map[string]string) { delete(meta, FieldGateTimeoutAction) }},
		{name: "invalid gate timeout action", mutate: func(meta map[string]string) { meta[FieldGateTimeoutAction] = "ignore" }},
		{name: "missing trigger", mutate: func(meta map[string]string) { delete(meta, FieldTrigger) }},
		{name: "invalid trigger", mutate: func(meta map[string]string) { meta[FieldTrigger] = "timer" }},
		{name: "missing trigger condition", mutate: func(meta map[string]string) { delete(meta, FieldTriggerCondition) }},
		{name: "event trigger without condition", mutate: func(meta map[string]string) { meta[FieldTrigger] = TriggerEvent }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, store, _ := setupReconciler(t)
			meta := cloneMetadata(completeEmptyStateMetadata())
			tt.mutate(meta)
			store.addBead("root-1", "in_progress", "", "", meta)

			store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
				t.Fatal("incomplete creation metadata must be classified before wisp lookup")
				return "", false, nil
			}
			store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
				t.Fatal("incomplete creation metadata must not pour a wisp")
				return "", nil
			}

			report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
			if err != nil {
				t.Fatalf("ReconcileBeads: %v", err)
			}
			if report.Errors != 0 || report.Recovered != 1 {
				t.Fatalf("report = %+v, want one successful recovery", report)
			}
			detail := report.Details[0]
			if detail.Action != ActionCompletedTerminal || detail.Error != nil {
				t.Fatalf("detail = %+v, want completed_terminal without error", detail)
			}

			gotMeta, getErr := store.GetMetadata("root-1")
			if getErr != nil {
				t.Fatalf("GetMetadata(root-1): %v", getErr)
			}
			if gotMeta[FieldState] != StateTerminated {
				t.Errorf("state = %q, want %q", gotMeta[FieldState], StateTerminated)
			}
			if gotMeta[FieldTerminalReason] != TerminalPartialCreation {
				t.Errorf("terminal_reason = %q, want %q", gotMeta[FieldTerminalReason], TerminalPartialCreation)
			}
			info, getErr := store.GetBead("root-1")
			if getErr != nil {
				t.Fatalf("GetBead(root-1): %v", getErr)
			}
			if info.Status != "closed" {
				t.Errorf("status = %q, want closed", info.Status)
			}
		})
	}
}

func TestReconcile_MissingState_FreshReconcilerHealsDoubleStateWriteFailure(t *testing.T) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	initialStateErr := errors.New("initial creating-state write failed")
	rollbackStateErr := errors.New("rollback terminated-state write failed")
	stateWrites := 0
	store.SetMetadataErrFunc = func(key string) error {
		if key != FieldState {
			return nil
		}
		stateWrites++
		if stateWrites == 1 {
			return initialStateErr
		}
		return rollbackStateErr
	}
	pourCalls := 0
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		pourCalls++
		return "unexpected-wisp", nil
	}

	creator := &Handler{Store: store, Emitter: emitter, Clock: time.Now}
	_, createErr := creator.CreateHandler(context.Background(), CreateParams{
		Formula:       "test-formula",
		Target:        "test-agent",
		MaxIterations: 5,
		GateMode:      GateModeManual,
	})
	if !errors.Is(createErr, initialStateErr) || !errors.Is(createErr, rollbackStateErr) {
		t.Fatalf("create error = %v, want both initial and rollback state failures", createErr)
	}
	if stateWrites != 2 {
		t.Fatalf("state write attempts = %d, want 2", stateWrites)
	}
	beforeMeta, err := store.GetMetadata("conv-1")
	if err != nil {
		t.Fatalf("GetMetadata(conv-1) before recovery: %v", err)
	}
	if len(beforeMeta) != 0 {
		t.Fatalf("metadata before recovery = %#v, want empty after double fault", beforeMeta)
	}

	store.SetMetadataErrFunc = nil
	freshHandler := &Handler{Store: store, Emitter: &fakeEmitter{}, Clock: time.Now}
	freshReconciler := &Reconciler{Handler: freshHandler}
	report, err := freshReconciler.ReconcileBeads(context.Background(), []string{"conv-1"})
	if err != nil {
		t.Fatalf("ReconcileBeads: %v", err)
	}
	if report.Errors != 0 || report.Recovered != 1 || report.Details[0].Action != ActionCompletedTerminal {
		t.Fatalf("report = %+v, want completed terminal recovery", report)
	}
	if pourCalls != 0 {
		t.Fatalf("wisp pours = %d, want zero across create and fresh recovery", pourCalls)
	}
	meta, err := store.GetMetadata("conv-1")
	if err != nil {
		t.Fatalf("GetMetadata(conv-1) after recovery: %v", err)
	}
	if meta[FieldState] != StateTerminated || meta[FieldTerminalReason] != TerminalPartialCreation {
		t.Fatalf("metadata after recovery = %#v, want terminated partial_creation", meta)
	}
	info, err := store.GetBead("conv-1")
	if err != nil {
		t.Fatalf("GetBead(conv-1): %v", err)
	}
	if info.Status != "closed" {
		t.Fatalf("root status = %q, want closed", info.Status)
	}
}

func TestReconcile_MissingState_TransientWispEvidenceErrorsDoNotMutate(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeStore, error)
	}{
		{
			name: "lookup",
			configure: func(store *fakeStore, transientErr error) {
				store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
					return "", false, transientErr
				}
			},
		},
		{
			name: "get bead",
			configure: func(store *fakeStore, transientErr error) {
				store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
					return "existing-wisp", true, nil
				}
				store.GetBeadFunc = func(string) (BeadInfo, error) {
					return BeadInfo{}, transientErr
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, store, _ := setupReconciler(t)
			store.addBead("root-1", "in_progress", "", "", completeEmptyStateMetadata())
			transientErr := errors.New("store temporarily unavailable")
			tt.configure(store, transientErr)
			store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
				t.Fatal("uncertain wisp evidence must not pour")
				return "", nil
			}
			store.CloseBeadFunc = func(_, _ string) error {
				t.Fatal("uncertain wisp evidence must not terminalize")
				return nil
			}

			report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
			if err != nil {
				t.Fatalf("ReconcileBeads: %v", err)
			}
			if report.Errors != 1 || !errors.Is(report.Details[0].Error, transientErr) {
				t.Fatalf("report = %+v, want transient evidence error", report)
			}
			meta, getErr := store.GetMetadata("root-1")
			if getErr != nil {
				t.Fatalf("GetMetadata(root-1): %v", getErr)
			}
			if meta[FieldState] != "" || meta[FieldActiveWisp] != "" {
				t.Fatalf("metadata after transient error = %#v, want no recovery mutation", meta)
			}
		})
	}
}

func TestReconcile_MissingState_MismatchedWispEvidenceDoesNotMutate(t *testing.T) {
	key1 := IdempotencyKey("root-1", 1)
	tests := []struct {
		name      string
		foundID   string
		found     bool
		wispInfo  BeadInfo
		wantError string
	}{
		{
			name:      "ID without found evidence",
			foundID:   "existing-wisp",
			found:     false,
			wispInfo:  BeadInfo{},
			wantError: "without found evidence",
		},
		{
			name:      "empty found ID",
			foundID:   "",
			found:     true,
			wispInfo:  BeadInfo{},
			wantError: "empty bead ID",
		},
		{
			name:      "returned bead ID differs",
			foundID:   "existing-wisp",
			found:     true,
			wispInfo:  BeadInfo{ID: "other-wisp", Status: "in_progress", ParentID: "root-1", IdempotencyKey: key1},
			wantError: "returned bead ID",
		},
		{
			name:      "wrong parent",
			foundID:   "existing-wisp",
			found:     true,
			wispInfo:  BeadInfo{ID: "existing-wisp", Status: "in_progress", ParentID: "other-root", IdempotencyKey: key1},
			wantError: "parent",
		},
		{
			name:      "wrong idempotency key",
			foundID:   "existing-wisp",
			found:     true,
			wispInfo:  BeadInfo{ID: "existing-wisp", Status: "in_progress", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)},
			wantError: "idempotency key",
		},
		{
			name:      "invalid status",
			foundID:   "existing-wisp",
			found:     true,
			wispInfo:  BeadInfo{ID: "existing-wisp", Status: "deleted", ParentID: "root-1", IdempotencyKey: key1},
			wantError: "status",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, store, _ := setupReconciler(t)
			store.addBead("root-1", "in_progress", "", "", completeEmptyStateMetadata())
			store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
				return tt.foundID, tt.found, nil
			}
			store.GetBeadFunc = func(string) (BeadInfo, error) {
				return tt.wispInfo, nil
			}
			store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
				t.Fatal("mismatched wisp evidence must not pour")
				return "", nil
			}
			store.CloseBeadFunc = func(_, _ string) error {
				t.Fatal("mismatched wisp evidence must not terminalize")
				return nil
			}

			report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
			if err != nil {
				t.Fatalf("ReconcileBeads: %v", err)
			}
			if report.Errors != 1 || report.Details[0].Error == nil {
				t.Fatalf("report = %+v, want evidence mismatch error", report)
			}
			if !strings.Contains(report.Details[0].Error.Error(), tt.wantError) {
				t.Fatalf("error = %q, want substring %q", report.Details[0].Error, tt.wantError)
			}
			meta, getErr := store.GetMetadata("root-1")
			if getErr != nil {
				t.Fatalf("GetMetadata(root-1): %v", getErr)
			}
			if meta[FieldState] != "" || meta[FieldActiveWisp] != "" {
				t.Fatalf("metadata after mismatch = %#v, want no recovery mutation", meta)
			}
		})
	}
}

// --- Path 1b: StateCreating (partial creation) ---

func TestReconcile_StateCreating_TerminatesPartialCreation(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// Bead stuck in "creating" state — creation was interrupted.
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState: StateCreating,
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Recovered != 1 {
		t.Errorf("Recovered = %d, want 1", report.Recovered)
	}
	if report.Errors != 0 {
		t.Errorf("Errors = %d, want 0", report.Errors)
	}

	d := report.Details[0]
	if d.Action != ActionCompletedTerminal {
		t.Errorf("Action = %q, want %q", d.Action, ActionCompletedTerminal)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// Verify the bead is now terminated and closed.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldTerminalReason] != TerminalPartialCreation {
		t.Errorf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalPartialCreation)
	}
	if meta[FieldTerminalActor] != "recovery" {
		t.Errorf("terminal_actor = %q, want %q", meta[FieldTerminalActor], "recovery")
	}
	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}
}

// --- Path 2: Terminated but not closed ---

func TestReconcile_TerminatedNotClosed_CompletesClosure(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
		FieldFormula:        "test-formula",
		FieldMaxIterations:  "5",
		FieldRig:            "prod",
	})

	// Add a closed wisp child.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionCompletedTerminal {
		t.Errorf("Action = %q, want %q", d.Action, ActionCompletedTerminal)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// Bead should now be closed.
	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}

	// ConvergenceTerminated event should have been emitted with recovery=true.
	ev, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Error("expected ConvergenceTerminated event")
	}
	if ev.BeadID != "root-1" {
		t.Errorf("event bead_id = %q, want %q", ev.BeadID, "root-1")
	}
	var payload TerminatedPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}
	if payload.Rig != "prod" {
		t.Errorf("payload.Rig = %q, want prod", payload.Rig)
	}
}

func TestReconcile_TerminatedNotClosed_BackfillsActor(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// terminal_actor is missing.
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalStopped,
		FieldFormula:        "test-formula",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionCompletedTerminal {
		t.Errorf("Action = %q, want %q", d.Action, ActionCompletedTerminal)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldTerminalActor] != "recovery" {
		t.Errorf("terminal_actor = %q, want %q", meta[FieldTerminalActor], "recovery")
	}
}

func TestReconcile_TerminatedAlreadyClosed_NoAction(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "closed", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionNoAction {
		t.Errorf("Action = %q, want %q", d.Action, ActionNoAction)
	}
}

// --- Path 3: Waiting manual ---

func TestReconcile_WaitingManual_TerminalReasonSet_CompletesTerminal(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateWaitingManual,
		FieldWaitingReason:  WaitManual,
		FieldTerminalReason: TerminalStopped,
		FieldTerminalActor:  "operator:alice",
		FieldFormula:        "test-formula",
	})

	// A child wisp for the iteration.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionCompletedTerminal {
		t.Errorf("Action = %q, want %q", d.Action, ActionCompletedTerminal)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}

	_, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Error("expected ConvergenceTerminated event")
	}
}

func TestReconcile_WaitingManual_GenuineHold_NoStateChange(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateWaitingManual,
		FieldWaitingReason:     WaitManual,
		FieldLastProcessedWisp: "wisp-1",
		FieldFormula:           "test-formula",
		FieldGateMode:          GateModeManual,
		FieldIteration:         "1",
		FieldRig:               "prod",
	})

	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionNoAction {
		t.Errorf("Action = %q, want %q", d.Action, ActionNoAction)
	}

	// State should remain waiting_manual.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateWaitingManual {
		t.Errorf("state = %q, want %q", meta[FieldState], StateWaitingManual)
	}

	// Recovery should re-emit ConvergenceWaitingManual event.
	ev, ok := emitter.findEvent(EventWaitingManual)
	if !ok {
		t.Fatal("expected ConvergenceWaitingManual recovery event")
	}
	if !ev.Recovery {
		t.Error("expected recovery flag to be true")
	}
	var payload WaitingManualPayload
	if err := json.Unmarshal(ev.Payload, &payload); err != nil {
		t.Fatalf("unmarshaling payload: %v", err)
	}
	if payload.Rig != "prod" {
		t.Errorf("payload.Rig = %q, want prod", payload.Rig)
	}
}

func TestReconcile_WaitingManual_GenuineHold_RepairsLastProcessedWisp(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// last_processed_wisp is stale (points to wisp-0, but wisp-1 is the
	// highest closed wisp).
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateWaitingManual,
		FieldWaitingReason:     WaitManual,
		FieldLastProcessedWisp: "wisp-0",
		FieldFormula:           "test-formula",
	})

	store.addBead("wisp-0", "closed", "root-1", IdempotencyKey("root-1", 0), nil)
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionRepairedState {
		t.Errorf("Action = %q, want %q", d.Action, ActionRepairedState)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldLastProcessedWisp] != "wisp-1" {
		t.Errorf("last_processed_wisp = %q, want %q", meta[FieldLastProcessedWisp], "wisp-1")
	}
}

// --- Path 4: Active ---

func TestReconcile_Active_ClosedUnprocessedWisp_Replays(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldActiveWisp:        "wisp-iter-1",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		// Pre-persist the gate outcome so replay skips evaluation.
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})

	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionRepairedState {
		t.Errorf("Action = %q, want %q", d.Action, ActionRepairedState)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	// After replaying wisp_closed with gate=fail and iteration < max,
	// the handler should have iterated: a new wisp should be poured
	// and active_wisp updated.
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] == "" || meta[FieldActiveWisp] == "wisp-iter-1" {
		t.Errorf("active_wisp should be updated to new wisp, got %q", meta[FieldActiveWisp])
	}

	// Verify iteration event was emitted.
	if _, ok := emitter.findEvent(EventIteration); !ok {
		t.Error("expected ConvergenceIteration event from replay")
	}
}

func TestReconcile_Active_MissingActiveWisp_ReconstructsChain(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldActiveWisp:        "wisp-iter-2",
		FieldLastProcessedWisp: "wisp-iter-1",
	})

	// The previous wisp exists and is closed, but the active wisp was
	// cleaned up after the crash. Startup recovery should rebuild the chain
	// from the remaining state instead of stalling on the missing bead.
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error != nil {
		t.Fatalf("reconcile error: %v", d.Error)
	}
	if d.Action != ActionPouredWisp && d.Action != ActionAdoptedWisp {
		t.Fatalf("Action = %q, want %q or %q", d.Action, ActionPouredWisp, ActionAdoptedWisp)
	}

	meta, _ := store.GetMetadata("root-1")
	activeWisp := meta[FieldActiveWisp]
	if activeWisp == "" {
		t.Fatal("active_wisp should be restored after recovery")
	}
	if _, err := store.GetBead(activeWisp); err != nil {
		t.Fatalf("active_wisp %q should point to an existing bead: %v", activeWisp, err)
	}
}

func TestReconcile_Active_MissingActiveWisp_ReplaysRecoveredClosedReplacement(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldActiveWisp:        "wisp-iter-2",
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldGateOutcomeWisp:   "wisp-replacement",
		FieldGateOutcome:       GatePass,
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-replacement", "closed", "root-1", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error != nil {
		t.Fatalf("reconcile error: %v", d.Error)
	}
	if d.Action != ActionRepairedState {
		t.Fatalf("Action = %q, want %q", d.Action, ActionRepairedState)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Fatalf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if meta[FieldLastProcessedWisp] != "wisp-replacement" {
		t.Fatalf("last_processed_wisp = %q, want %q", meta[FieldLastProcessedWisp], "wisp-replacement")
	}
}

func TestReconcile_Active_MissingActiveWisp_RepairsOpenReplacementMetadata(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldGateTimeout:       "60s",
		FieldGateTimeoutAction: TimeoutActionIterate,
		FieldActiveWisp:        "wisp-iter-2",
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-replacement", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error != nil {
		t.Fatalf("reconcile error: %v", d.Error)
	}
	if d.Action != ActionRepairedState {
		t.Fatalf("Action = %q, want %q", d.Action, ActionRepairedState)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "wisp-replacement" {
		t.Fatalf("active_wisp = %q, want %q", meta[FieldActiveWisp], "wisp-replacement")
	}
}

func TestReconcile_Active_StoreErrorReadingActiveWisp_ReportsError(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:      StateActive,
		FieldFormula:    "test-formula",
		FieldActiveWisp: "wisp-iter-1",
	})
	store.GetBeadFunc = func(id string) (BeadInfo, error) {
		return BeadInfo{}, fmt.Errorf("store unavailable for %s", id)
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Error == nil {
		t.Fatal("expected reconcile error")
	}
	if got := d.Error.Error(); !strings.Contains(got, "store unavailable for wisp-iter-1") {
		t.Fatalf("reconcile error = %q, want store failure", got)
	}
	if d.Action != ActionNoAction {
		t.Fatalf("Action = %q, want %q", d.Action, ActionNoAction)
	}
}

func TestReconcile_Active_OpenWisp_NoAction(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:      StateActive,
		FieldActiveWisp: "wisp-iter-1",
		FieldFormula:    "test-formula",
	})

	// Wisp is still open (in_progress).
	store.addBead("wisp-iter-1", "in_progress", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionNoAction {
		t.Errorf("Action = %q, want %q", d.Action, ActionNoAction)
	}
}

func TestReconcile_Active_TerminalReasonSet_CompletesStop(t *testing.T) {
	rec, store, emitter := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateActive,
		FieldTerminalReason: TerminalStopped,
		FieldTerminalActor:  "operator:bob",
		FieldFormula:        "test-formula",
	})

	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionCompletedTerminal {
		t.Errorf("Action = %q, want %q", d.Action, ActionCompletedTerminal)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	beadInfo, _ := store.GetBead("root-1")
	if beadInfo.Status != "closed" {
		t.Errorf("bead status = %q, want %q", beadInfo.Status, "closed")
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}

	_, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Error("expected ConvergenceTerminated event")
	}
}

func TestReconcile_Active_EmptyActiveWisp_PoursNext(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "",
		FieldLastProcessedWisp: "wisp-1",
		FieldFormula:           "test-formula",
	})

	// One closed wisp from iteration 1.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionPouredWisp {
		t.Errorf("Action = %q, want %q", d.Action, ActionPouredWisp)
	}
	if d.Error != nil {
		t.Errorf("unexpected error: %v", d.Error)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] == "" {
		t.Error("active_wisp should be set after pouring next wisp")
	}
}

func TestReconcile_Active_EmptyActiveWisp_AdoptsExisting(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "",
		FieldLastProcessedWisp: "wisp-1",
		FieldFormula:           "test-formula",
	})

	// One closed wisp from iteration 1.
	store.addBead("wisp-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	// An existing wisp for iteration 2 (already poured before crash).
	store.addBead("wisp-2", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionAdoptedWisp {
		t.Errorf("Action = %q, want %q", d.Action, ActionAdoptedWisp)
	}

	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "wisp-2" {
		t.Errorf("active_wisp = %q, want %q", meta[FieldActiveWisp], "wisp-2")
	}
}

// --- Already processed ---

func TestReconcile_Active_AlreadyProcessed_NoAction(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "wisp-iter-1",
		FieldLastProcessedWisp: "wisp-iter-1", // already processed
		FieldFormula:           "test-formula",
	})

	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	d := report.Details[0]
	if d.Action != ActionNoAction {
		t.Errorf("Action = %q, want %q", d.Action, ActionNoAction)
	}
}

// --- Multiple beads ---

func TestReconcile_MultipleBeads_ContinuesOnError(t *testing.T) {
	rec, store, _ := setupReconciler(t)

	// bead-1: valid, needs recovery.
	store.addBead("bead-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
		FieldFormula:        "test-formula",
	})

	// bead-2: does not exist — will cause an error.
	// (not added to the store)

	// bead-3: valid, no action needed.
	store.addBead("bead-3", "closed", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
	})

	report, err := rec.ReconcileBeads(context.Background(), []string{"bead-1", "bead-2", "bead-3"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if report.Scanned != 3 {
		t.Errorf("Scanned = %d, want 3", report.Scanned)
	}
	if report.Errors != 1 {
		t.Errorf("Errors = %d, want 1", report.Errors)
	}
	if report.Recovered != 1 {
		t.Errorf("Recovered = %d, want 1", report.Recovered)
	}
	if len(report.Details) != 3 {
		t.Fatalf("Details length = %d, want 3", len(report.Details))
	}

	// bead-1: completed_terminal
	if report.Details[0].Action != ActionCompletedTerminal {
		t.Errorf("bead-1 Action = %q, want %q", report.Details[0].Action, ActionCompletedTerminal)
	}

	// bead-2: error
	if report.Details[1].Error == nil {
		t.Error("bead-2 should have an error")
	}

	// bead-3: no_action (already closed)
	if report.Details[2].Action != ActionNoAction {
		t.Errorf("bead-3 Action = %q, want %q", report.Details[2].Action, ActionNoAction)
	}
}

// --- Recovery events ---

func TestReconcile_RecoveryEventsHaveRecoveryFlag(t *testing.T) {
	store := newFakeStore()

	// Use a custom emitter that captures the recovery flag.
	type recoveryEvent struct {
		eventType string
		recovery  bool
	}
	var captured []recoveryEvent
	emitter := &recoveryCapturingEmitter{
		capture: func(eventType string, recovery bool) {
			captured = append(captured, recoveryEvent{eventType, recovery})
		},
	}

	handler := &Handler{
		Store:   store,
		Emitter: emitter,
		Clock:   time.Now,
	}
	rec := &Reconciler{Handler: handler}

	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateTerminated,
		FieldTerminalReason: TerminalApproved,
		FieldTerminalActor:  "controller",
		FieldFormula:        "test-formula",
	})

	_, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(captured) == 0 {
		t.Fatal("expected at least one event to be captured")
	}
	for _, ev := range captured {
		if !ev.recovery {
			t.Errorf("event %q should have recovery=true", ev.eventType)
		}
	}
}

// --- Helper functions ---

func TestDeriveIterationFromChildren(t *testing.T) {
	children := []BeadInfo{
		{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 1)},
		{ID: "w2", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 2)},
		{ID: "w3", Status: "in_progress", IdempotencyKey: IdempotencyKey("root-1", 3)},
		{ID: "other", Status: "closed", IdempotencyKey: "unrelated-key"},
	}

	got := deriveIterationFromChildren(children, "root-1")
	if got != 2 {
		t.Errorf("deriveIterationFromChildren = %d, want 2", got)
	}
}

func TestHighestClosedWisp(t *testing.T) {
	children := []BeadInfo{
		{ID: "w1", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 1)},
		{ID: "w3", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 3)},
		{ID: "w2", Status: "closed", IdempotencyKey: IdempotencyKey("root-1", 2)},
		{ID: "w4", Status: "in_progress", IdempotencyKey: IdempotencyKey("root-1", 4)},
	}

	best, iter, found := highestClosedWisp(children, "root-1")
	if !found {
		t.Fatal("expected to find a closed wisp")
	}
	if best.ID != "w3" {
		t.Errorf("best.ID = %q, want %q", best.ID, "w3")
	}
	if iter != 3 {
		t.Errorf("iter = %d, want 3", iter)
	}
}

func TestHighestClosedWisp_NoneFound(t *testing.T) {
	children := []BeadInfo{
		{ID: "w1", Status: "in_progress", IdempotencyKey: IdempotencyKey("root-1", 1)},
	}

	_, _, found := highestClosedWisp(children, "root-1")
	if found {
		t.Error("expected not to find a closed wisp")
	}
}

func TestReconcile_EmptyList_NoOp(t *testing.T) {
	rec, _, _ := setupReconciler(t)

	report, err := rec.ReconcileBeads(context.Background(), nil)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if report.Scanned != 0 {
		t.Errorf("Scanned = %d, want 0", report.Scanned)
	}
	if report.Recovered != 0 {
		t.Errorf("Recovered = %d, want 0", report.Recovered)
	}
	if len(report.Details) != 0 {
		t.Errorf("Details length = %d, want 0", len(report.Details))
	}
}

// --- recoveryCapturingEmitter ---

// recoveryCapturingEmitter is a test-only EventEmitter that captures the
// recovery flag passed to Emit.  It also satisfies the fakeEmitter
// contract for findEvent.
type recoveryCapturingEmitter struct {
	fakeEmitter
	capture func(eventType string, recovery bool)
}

func (e *recoveryCapturingEmitter) Emit(eventType, eventID, beadID string, payload json.RawMessage, recovery bool) {
	e.fakeEmitter.Emit(eventType, eventID, beadID, payload, recovery)
	if e.capture != nil {
		e.capture(eventType, recovery)
	}
}
