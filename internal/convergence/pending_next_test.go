package convergence

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestHandleWispClosed_PendingNextTransientReadPreservesMarkerAndStops(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldPendingNextWisp: "wisp-iter-2",
	})
	store.addBead("wisp-iter-2", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)
	wisp1, err := store.GetBead("wisp-iter-1")
	if err != nil {
		t.Fatalf("GetBead(wisp-iter-1): %v", err)
	}
	transientErr := errors.New("pending read temporarily unavailable")
	store.GetBeadFunc = func(id string) (BeadInfo, error) {
		switch id {
		case "wisp-iter-1":
			return wisp1, nil
		case "wisp-iter-2":
			return BeadInfo{}, transientErr
		default:
			return BeadInfo{}, fmt.Errorf("unexpected GetBead(%q)", id)
		}
	}

	clears := 0
	lookups := 0
	pours := 0
	activations := 0
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldPendingNextWisp {
			clears++
		}
		return nil
	}
	store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
		lookups++
		return "", false, nil
	}
	store.PourSpeculativeWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		pours++
		return "unexpected-wisp", nil
	}
	store.ActivateWispFunc = func(string) error {
		activations++
		return nil
	}

	_, err = handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if !errors.Is(err, transientErr) {
		t.Fatalf("HandleWispClosed error = %v, want transient pending read error", err)
	}
	if clears != 0 || lookups != 0 || pours != 0 || activations != 0 {
		t.Fatalf("effects after transient read: clears=%d lookups=%d pours=%d activations=%d, want all zero", clears, lookups, pours, activations)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1): %v", err)
	}
	if meta[FieldPendingNextWisp] != "wisp-iter-2" {
		t.Fatalf("pending_next_wisp = %q, want preserved", meta[FieldPendingNextWisp])
	}
}

func TestReconcile_PendingNextTransientReadPreservesMarkerAndStops(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldActiveWisp:        "",
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldPendingNextWisp:   "wisp-iter-2",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)
	transientErr := errors.New("pending read temporarily unavailable")
	store.GetBeadFunc = func(id string) (BeadInfo, error) {
		if id == "wisp-iter-2" {
			return BeadInfo{}, transientErr
		}
		return BeadInfo{}, fmt.Errorf("unexpected GetBead(%q)", id)
	}

	clears := 0
	lookups := 0
	pours := 0
	activations := 0
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldPendingNextWisp {
			clears++
		}
		return nil
	}
	store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
		lookups++
		return "", false, nil
	}
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		pours++
		return "unexpected-wisp", nil
	}
	store.ActivateWispFunc = func(string) error {
		activations++
		return nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("ReconcileBeads: %v", err)
	}
	if report.Errors != 1 || !errors.Is(report.Details[0].Error, transientErr) {
		t.Fatalf("report = %+v, want transient pending read error", report)
	}
	if clears != 0 || lookups != 0 || pours != 0 || activations != 0 {
		t.Fatalf("effects after transient read: clears=%d lookups=%d pours=%d activations=%d, want all zero", clears, lookups, pours, activations)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1): %v", err)
	}
	if meta[FieldPendingNextWisp] != "wisp-iter-2" || meta[FieldActiveWisp] != "" {
		t.Fatalf("metadata after transient read = %#v, want pending preserved and no active wisp", meta)
	}
}

func TestReconcile_PendingNextUnsupportedStatusFailsConservatively(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldPendingNextWisp:   "wisp-iter-2",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "queued", "root-1", IdempotencyKey("root-1", 2), nil)

	clears := 0
	lookups := 0
	pours := 0
	activations := 0
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldPendingNextWisp {
			clears++
		}
		return nil
	}
	store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
		lookups++
		return "", false, nil
	}
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		pours++
		return "unexpected-wisp", nil
	}
	store.ActivateWispFunc = func(string) error {
		activations++
		return nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("ReconcileBeads: %v", err)
	}
	if report.Errors != 1 || !strings.Contains(report.Details[0].Error.Error(), "unsupported status") {
		t.Fatalf("report = %+v, want unsupported status error", report)
	}
	if clears != 0 || lookups != 0 || pours != 0 || activations != 0 {
		t.Fatalf("effects after unsupported status: clears=%d lookups=%d pours=%d activations=%d, want all zero", clears, lookups, pours, activations)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1): %v", err)
	}
	if meta[FieldPendingNextWisp] != "wisp-iter-2" || meta[FieldActiveWisp] != "" {
		t.Fatalf("metadata after unsupported status = %#v, want pending preserved and no active wisp", meta)
	}
}

func TestReconcile_PendingNextInapplicableClearFailureReturns(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*fakeStore)
	}{
		{
			name: "not found",
			prepare: func(*fakeStore) {
			},
		},
		{
			name: "parent mismatch",
			prepare: func(store *fakeStore) {
				store.addBead("other-root", "in_progress", "", "", nil)
				store.addBead("wisp-iter-2", "in_progress", "other-root", IdempotencyKey("root-1", 2), nil)
			},
		},
		{
			name: "key mismatch",
			prepare: func(store *fakeStore) {
				store.addBead("wisp-iter-2", "in_progress", "root-1", IdempotencyKey("root-1", 3), nil)
			},
		},
		{
			name: "closed",
			prepare: func(store *fakeStore) {
				store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 3), nil)
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, store, _ := setupReconciler(t)
			store.addBead("root-1", "in_progress", "", "", map[string]string{
				FieldState:             StateActive,
				FieldIteration:         "1",
				FieldMaxIterations:     "5",
				FieldFormula:           "test-formula",
				FieldTarget:            "test-agent",
				FieldGateMode:          GateModeCondition,
				FieldLastProcessedWisp: "wisp-iter-1",
				FieldPendingNextWisp:   "wisp-iter-2",
			})
			store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
			tt.prepare(store)

			clearErr := errors.New("pending marker clear failed")
			store.SetMetadataErrFunc = func(key string) error {
				if key == FieldPendingNextWisp {
					return clearErr
				}
				return nil
			}
			lookups := 0
			pours := 0
			activations := 0
			store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
				lookups++
				return "", false, nil
			}
			store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
				pours++
				return "unexpected-wisp", nil
			}
			store.ActivateWispFunc = func(string) error {
				activations++
				return nil
			}

			report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
			if err != nil {
				t.Fatalf("ReconcileBeads: %v", err)
			}
			if report.Errors != 1 || !errors.Is(report.Details[0].Error, clearErr) {
				t.Fatalf("report = %+v, want pending clear error", report)
			}
			if lookups != 0 || pours != 0 || activations != 0 {
				t.Fatalf("effects after failed clear: lookups=%d pours=%d activations=%d, want all zero", lookups, pours, activations)
			}
			meta, getErr := store.GetMetadata("root-1")
			if getErr != nil {
				t.Fatalf("GetMetadata(root-1): %v", getErr)
			}
			if meta[FieldPendingNextWisp] != "wisp-iter-2" || meta[FieldActiveWisp] != "" {
				t.Fatalf("metadata after failed clear = %#v, want pending preserved and no active wisp", meta)
			}
		})
	}
}

func TestReconcile_PendingNextStaleMarkerClearsExactlyOnce(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldPendingNextWisp:   "missing-wisp",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)
	store.PourWispFunc = func(_, _, key string, _ map[string]string, _ string) (string, error) {
		t.Fatalf("existing iteration-2 wisp must be adopted, not repoured for %q", key)
		return "", nil
	}

	clearCalls := 0
	secondClearErr := errors.New("redundant second clear failed")
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldPendingNextWisp {
			clearCalls++
			if clearCalls > 1 {
				return secondClearErr
			}
		}
		return nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("ReconcileBeads: %v", err)
	}
	if report.Errors != 0 || report.Details[0].Action != ActionAdoptedWisp {
		t.Fatalf("report = %+v, want successful existing-wisp adoption", report)
	}
	if clearCalls != 1 {
		t.Fatalf("pending marker clears = %d, want exactly one", clearCalls)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1): %v", err)
	}
	if meta[FieldPendingNextWisp] != "" || meta[FieldActiveWisp] != "wisp-iter-2" {
		t.Fatalf("metadata after adoption = %#v, want stale marker cleared and existing wisp active", meta)
	}
}

func TestReconcile_PendingNextAdoptionClearFailureRepairsWithoutDuplicateWork(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldPendingNextWisp:   "wisp-iter-2",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)

	clearErr := errors.New("pending marker clear failed")
	failClear := true
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldPendingNextWisp && failClear {
			return clearErr
		}
		return nil
	}
	lookups := 0
	pours := 0
	activations := 0
	store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
		lookups++
		return "", false, nil
	}
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		pours++
		return "unexpected-wisp", nil
	}
	store.ActivateWispFunc = func(string) error {
		activations++
		return nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("first ReconcileBeads: %v", err)
	}
	if report.Errors != 1 || !errors.Is(report.Details[0].Error, clearErr) {
		t.Fatalf("first report = %+v, want pending clear error", report)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1) after first pass: %v", err)
	}
	if meta[FieldActiveWisp] != "wisp-iter-2" || meta[FieldPendingNextWisp] != "wisp-iter-2" {
		t.Fatalf("metadata after first pass = %#v, want active adoption plus durable pending marker", meta)
	}
	if lookups != 0 || pours != 0 || activations != 1 {
		t.Fatalf("first-pass effects: lookups=%d pours=%d activations=%d, want 0/0/1", lookups, pours, activations)
	}

	fresh := &Reconciler{Handler: &Handler{Store: store, Emitter: &fakeEmitter{}}}
	report, err = fresh.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("fresh ReconcileBeads: %v", err)
	}
	if report.Errors != 1 || !errors.Is(report.Details[0].Error, clearErr) {
		t.Fatalf("fresh report with persistent fault = %+v, want checked clear error", report)
	}
	if lookups != 0 || pours != 0 || activations != 1 {
		t.Fatalf("effects during failed repair: lookups=%d pours=%d activations=%d, want 0/0/1", lookups, pours, activations)
	}

	failClear = false
	fresh = &Reconciler{Handler: &Handler{Store: store, Emitter: &fakeEmitter{}}}
	report, err = fresh.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("second fresh ReconcileBeads: %v", err)
	}
	if report.Errors != 0 || report.Details[0].Action != ActionRepairedState {
		t.Fatalf("second fresh report = %+v, want repaired_state", report)
	}
	meta, err = store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1) after repair: %v", err)
	}
	if meta[FieldActiveWisp] != "wisp-iter-2" || meta[FieldPendingNextWisp] != "" {
		t.Fatalf("metadata after repair = %#v, want active preserved and pending cleared", meta)
	}
	if lookups != 0 || pours != 0 || activations != 1 {
		t.Fatalf("total effects after repair: lookups=%d pours=%d activations=%d, want 0/0/1", lookups, pours, activations)
	}
}

func TestHandleWispClosed_IteratePendingCleanupFailureReturnsAndRepairs(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GateFail,
	})
	clearErr := errors.New("post-iterate pending clear failed")
	pendingWrites := 0
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldPendingNextWisp {
			pendingWrites++
			if pendingWrites == 2 {
				return clearErr
			}
		}
		return nil
	}

	_, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if !errors.Is(err, clearErr) {
		t.Fatalf("HandleWispClosed error = %v, want post-iterate clear error", err)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1): %v", err)
	}
	nextWispID := meta[FieldActiveWisp]
	if nextWispID == "" || meta[FieldPendingNextWisp] != nextWispID || meta[FieldLastProcessedWisp] != "wisp-iter-1" {
		t.Fatalf("metadata after failed cleanup = %#v, want committed successor plus pending repair marker", meta)
	}
	activationCount := len(store.ActivatedWispIDs)

	store.SetMetadataErrFunc = nil
	store.FindByIdempotencyKeyFunc = func(key string) (string, bool, error) {
		t.Fatalf("fresh repair must not look up %q", key)
		return "", false, nil
	}
	store.PourWispFunc = func(_, _, key string, _ map[string]string, _ string) (string, error) {
		t.Fatalf("fresh repair must not pour %q", key)
		return "", nil
	}
	fresh := &Reconciler{Handler: &Handler{Store: store, Emitter: &fakeEmitter{}}}
	report, err := fresh.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("fresh ReconcileBeads: %v", err)
	}
	if report.Errors != 0 || report.Details[0].Action != ActionRepairedState {
		t.Fatalf("fresh report = %+v, want repaired_state", report)
	}
	meta, err = store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1) after repair: %v", err)
	}
	if meta[FieldActiveWisp] != nextWispID || meta[FieldPendingNextWisp] != "" {
		t.Fatalf("metadata after repair = %#v, want active preserved and pending cleared", meta)
	}
	if len(store.ActivatedWispIDs) != activationCount {
		t.Fatalf("activations after repair = %v, want no additional activation", store.ActivatedWispIDs)
	}
}

func TestBurnSpeculativeWisp_PendingCleanupFailureReturns(t *testing.T) {
	store := newFakeStore()
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldPendingNextWisp: "wisp-iter-2",
	})
	store.addBead("wisp-iter-2", "in_progress", "root-1", IdempotencyKey("root-1", 2), nil)
	clearErr := errors.New("burn pending clear failed")
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldPendingNextWisp {
			return clearErr
		}
		return nil
	}
	handler := &Handler{Store: store}

	err := handler.burnSpeculativeWisp("root-1", "wisp-iter-2")
	if !errors.Is(err, clearErr) {
		t.Fatalf("burnSpeculativeWisp error = %v, want pending clear error", err)
	}
	if _, err := store.GetBead("wisp-iter-2"); !errors.Is(err, beads.ErrNotFound) {
		t.Fatalf("GetBead(wisp-iter-2) error = %v, want deleted wisp", err)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatalf("GetMetadata(root-1): %v", err)
	}
	if meta[FieldPendingNextWisp] != "wisp-iter-2" {
		t.Fatalf("pending_next_wisp = %q, want retained after failed clear", meta[FieldPendingNextWisp])
	}
}

func TestReconcile_ClosedExistingSuccessorAdoptsWithoutSynchronousReplay(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeManual,
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)
	store.PourWispFunc = func(_, _, key string, _ map[string]string, _ string) (string, error) {
		t.Fatalf("closed successor %q must be adopted, not repoured", key)
		return "", nil
	}
	store.ActivateWispFunc = func(id string) error {
		t.Fatalf("closed successor %q must never be activated", id)
		return nil
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("ReconcileBeads: %v", err)
	}
	if report.Errors != 0 || report.Details[0].Action != ActionAdoptedWisp {
		t.Fatalf("report = %+v, want closed-successor adoption", report)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldState] != StateActive || meta[FieldLastProcessedWisp] != "wisp-iter-1" || meta[FieldActiveWisp] != "wisp-iter-2" {
		t.Fatalf("metadata after closed-successor adoption = %#v, want existing tick owner to process iteration 2", meta)
	}
}

func TestHandleWispClosed_ClosedExistingSuccessorIsAdoptedWithoutSynchronousReplay(t *testing.T) {
	tests := []struct {
		name      string
		configure func(*fakeStore)
	}{
		{name: "pour returns existing closed successor"},
		{
			name: "pour error fallback finds closed successor",
			configure: func(store *fakeStore) {
				store.PourSpeculativeWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
					return "", errors.New("ambiguous speculative pour")
				}
				store.FindByIdempotencyKeyFunc = func(key string) (string, bool, error) {
					if key == IdempotencyKey("root-1", 2) {
						return "wisp-iter-2", true, nil
					}
					return "", false, nil
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, store, _ := setupBasicHandler(t, map[string]string{
				FieldGateOutcomeWisp: "wisp-iter-1",
				FieldGateOutcome:     GateFail,
				FieldGateCondition:   writeTriggerScript(t, 0),
			})
			store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)
			if tt.configure != nil {
				tt.configure(store)
			}
			store.ActivateWispFunc = func(id string) error {
				t.Fatalf("closed successor %q must never be activated", id)
				return nil
			}

			result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
			if err != nil {
				t.Fatalf("HandleWispClosed: %v", err)
			}
			if result.Action != ActionIterate || result.NextWispID != "wisp-iter-2" {
				t.Fatalf("result = %+v, want closed successor adopted for existing tick owner", result)
			}
			meta, err := store.GetMetadata("root-1")
			if err != nil {
				t.Fatal(err)
			}
			if meta[FieldState] != StateActive || meta[FieldActiveWisp] != "wisp-iter-2" || meta[FieldLastProcessedWisp] != "wisp-iter-1" || meta[FieldPendingNextWisp] != "" {
				t.Fatalf("metadata after closed-successor adoption = %#v", meta)
			}
			if _, ok := handler.Emitter.(*fakeEmitter).findEvent(EventTerminated); ok {
				t.Fatal("closed-successor adoption synchronously emitted a terminal event")
			}
		})
	}
}

func TestIterate_PourFallbackClosedSuccessorIsNotActivated(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{FieldGateMode: GateModeManual})
	store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		return "", errors.New("ambiguous pour")
	}
	store.FindByIdempotencyKeyFunc = func(string) (string, bool, error) {
		return "wisp-iter-2", true, nil
	}
	store.ActivateWispFunc = func(id string) error {
		t.Fatalf("closed fallback successor %q must never be activated", id)
		return nil
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}

	result, err := handler.iterate(
		context.Background(), "root-1", "wisp-iter-1", 1,
		GateConfig{Mode: GateModeManual}, GateResult{Outcome: GateFail}, meta, handler.clock(), "",
	)
	if err != nil {
		t.Fatalf("iterate: %v", err)
	}
	if result.Action != ActionIterate || result.NextWispID != "wisp-iter-2" {
		t.Fatalf("result = %+v, want closed fallback successor adopted", result)
	}
	meta, err = store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldState] != StateActive || meta[FieldLastProcessedWisp] != "wisp-iter-1" || meta[FieldActiveWisp] != "wisp-iter-2" {
		t.Fatalf("metadata after closed fallback adoption = %#v", meta)
	}
}

func TestIterateHandler_ClosedExistingSuccessorUsesExistingTickOwner(t *testing.T) {
	handler, store, emitter := setupWaitingManualHandler(t, nil)
	store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)
	store.PourWispFunc = func(_, _, _ string, _ map[string]string, _ string) (string, error) {
		return "", errors.New("ambiguous manual pour")
	}
	store.FindByIdempotencyKeyFunc = func(key string) (string, bool, error) {
		if key == IdempotencyKey("root-1", 2) {
			return "wisp-iter-2", true, nil
		}
		return "", false, nil
	}

	result, err := handler.IterateHandler(context.Background(), "root-1", "alice", "")
	if err != nil {
		t.Fatalf("IterateHandler: %v", err)
	}
	if result.Action != ActionIterate || result.NextWispID != "wisp-iter-2" {
		t.Fatalf("result = %+v, want closed successor adopted", result)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldState] != StateActive || meta[FieldLastProcessedWisp] != "wisp-iter-1" || meta[FieldActiveWisp] != "wisp-iter-2" {
		t.Fatalf("metadata after manual closed-successor adoption = %#v", meta)
	}
	if _, ok := emitter.findEvent(EventManualIterate); !ok {
		t.Fatal("manual iterate did not record the durable adoption")
	}
}

func TestBurnSpeculativeWisp_ClosedSuccessorIsPreserved(t *testing.T) {
	store := newFakeStore()
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldPendingNextWisp: "wisp-iter-2",
	})
	store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)
	handler := &Handler{Store: store}

	err := handler.burnSpeculativeWisp("root-1", "wisp-iter-2")
	if err == nil || !strings.Contains(err.Error(), "closed") {
		t.Fatalf("burnSpeculativeWisp error = %v, want closed-successor refusal", err)
	}
	if _, err := store.GetBead("wisp-iter-2"); err != nil {
		t.Fatalf("closed successor was deleted: %v", err)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldPendingNextWisp] != "wisp-iter-2" {
		t.Fatalf("pending_next_wisp = %q, want preserved closed-successor evidence", meta[FieldPendingNextWisp])
	}
}

func TestReconcile_ActivePendingMatchRequiresExactSuccessorEvidence(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("other-root", "in_progress", "", "", nil)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeCondition,
		FieldActiveWisp:        "wisp-iter-2",
		FieldLastProcessedWisp: "wisp-iter-1",
		FieldPendingNextWisp:   "wisp-iter-2",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("wisp-iter-2", "in_progress", "other-root", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("ReconcileBeads: %v", err)
	}
	if report.Errors != 1 || report.Details[0].Error == nil || !strings.Contains(report.Details[0].Error.Error(), "parent") {
		t.Fatalf("report = %+v, want exact-successor parent error", report)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldPendingNextWisp] != "wisp-iter-2" || meta[FieldActiveWisp] != "wisp-iter-2" {
		t.Fatalf("metadata after mismatched evidence = %#v, want active/pending markers preserved", meta)
	}
}
