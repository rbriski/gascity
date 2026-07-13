package convergence

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
)

func TestNormalTerminalEventsRequireDurableProof(t *testing.T) {
	for _, stage := range []string{"children", "state", "close", "marker"} {
		t.Run(stage, func(t *testing.T) {
			handler, store, emitter := setupBasicHandler(t, map[string]string{
				FieldGateOutcomeWisp: "wisp-iter-1",
				FieldGateOutcome:     GatePass,
			})
			injected := errors.New("injected " + stage + " failure")
			if stage == "children" {
				store.ChildrenFunc = func(string) ([]BeadInfo, error) { return nil, injected }
			}
			store.SetMetadataErrFunc = func(key string) error {
				if (stage == "state" && key == FieldState) || (stage == "marker" && key == FieldLastProcessedWisp) {
					return injected
				}
				return nil
			}
			store.CloseBeadFunc = func(id, _ string) error {
				if stage == "close" && id == "root-1" {
					return injected
				}
				return nil
			}

			_, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
			if !errors.Is(err, injected) {
				t.Fatalf("HandleWispClosed error = %v, want injected failure", err)
			}
			assertNoEventsOfType(t, emitter, EventIteration, EventTerminated)
			if stage == "children" {
				meta, getErr := store.GetMetadata("root-1")
				if getErr != nil {
					t.Fatal(getErr)
				}
				info, getErr := store.GetBead("root-1")
				if getErr != nil {
					t.Fatal(getErr)
				}
				if meta[FieldState] != StateActive || meta[FieldLastProcessedWisp] != "" || info.Status == "closed" {
					t.Fatalf("terminal proof changed after child failure: metadata=%#v status=%q", meta, info.Status)
				}
			}
		})
	}
}

func TestNormalTerminalUsesOneCheckedChildSnapshot(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GatePass,
	})
	children, err := store.Children("root-1")
	if err != nil {
		t.Fatal(err)
	}
	secondReadErr := errors.New("second child snapshot must not occur")
	childReads := 0
	store.ChildrenFunc = func(parentID string) ([]BeadInfo, error) {
		if parentID != "root-1" {
			// Terminal cleanup may inspect the speculative successor's subtree;
			// only root snapshots are part of terminal count/duration proof.
			return nil, nil
		}
		childReads++
		if childReads > 1 {
			return nil, secondReadErr
		}
		return children, nil
	}

	if _, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1"); err != nil {
		t.Fatalf("HandleWispClosed: %v", err)
	}
	if childReads != 1 {
		t.Fatalf("terminal child snapshots = %d, want exactly one checked snapshot", childReads)
	}
	event, ok := emitter.findEvent(EventTerminated)
	if !ok {
		t.Fatal("missing terminal event")
	}
	var payload TerminatedPayload
	if err := json.Unmarshal(event.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	if payload.CumulativeDurationMs <= 0 {
		t.Fatalf("cumulative duration = %dms, want value from checked child snapshot", payload.CumulativeDurationMs)
	}
}

func TestNormalConditionGateUsesOneCheckedChildSnapshot(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldGateCondition: writeTriggerScript(t, 0),
	})
	children, err := store.Children("root-1")
	if err != nil {
		t.Fatal(err)
	}
	secondReadErr := errors.New("condition gate must not fetch a second root snapshot")
	childReads := 0
	store.ChildrenFunc = func(parentID string) ([]BeadInfo, error) {
		if parentID != "root-1" {
			return nil, nil
		}
		childReads++
		if childReads > 1 {
			return nil, secondReadErr
		}
		return children, nil
	}

	result, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
	if err != nil {
		t.Fatalf("HandleWispClosed: %v", err)
	}
	if result.Action != ActionApproved {
		t.Fatalf("result = %+v, want approved", result)
	}
	if childReads != 1 {
		t.Fatalf("condition-terminal child snapshots = %d, want exactly one", childReads)
	}
	if _, ok := emitter.findEvent(EventTerminated); !ok {
		t.Fatal("missing terminal event")
	}
}

func TestManualApproveTerminalEventsRequireDurableProof(t *testing.T) {
	for _, stage := range []string{"state", "close", "marker"} {
		t.Run(stage, func(t *testing.T) {
			handler, store, emitter := setupWaitingManualTerminalHandler()
			injected := errors.New("injected " + stage + " failure")
			store.SetMetadataErrFunc = func(key string) error {
				if (stage == "state" && key == FieldState) || (stage == "marker" && key == FieldLastProcessedWisp) {
					return injected
				}
				return nil
			}
			store.CloseBeadFunc = func(id, _ string) error {
				if stage == "close" && id == "root-1" {
					return injected
				}
				return nil
			}

			_, err := handler.ApproveHandler(context.Background(), "root-1", "alice", "")
			if !errors.Is(err, injected) {
				t.Fatalf("ApproveHandler error = %v, want injected failure", err)
			}
			assertNoEventsOfType(t, emitter, EventTerminated, EventManualApprove)
		})
	}
}

func TestManualStopTerminalEventsRequireDurableProof(t *testing.T) {
	for _, stage := range []string{"state", "close", "marker"} {
		t.Run(stage, func(t *testing.T) {
			handler, store, emitter := setupActiveManualStopHandler()
			injected := errors.New("injected " + stage + " failure")
			store.SetMetadataErrFunc = func(key string) error {
				if (stage == "state" && key == FieldState) || (stage == "marker" && key == FieldLastProcessedWisp) {
					return injected
				}
				return nil
			}
			store.CloseBeadFunc = func(id, _ string) error {
				if stage == "close" && id == "root-1" {
					return injected
				}
				return nil
			}

			_, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
			if !errors.Is(err, injected) {
				t.Fatalf("StopHandler error = %v, want injected failure", err)
			}
			assertNoEventsOfType(t, emitter, EventIteration, EventTerminated, EventManualStop)
		})
	}
}

func TestManualTerminalEventsRepairStaleMarkerFromOneCheckedChildSnapshot(t *testing.T) {
	tests := []struct {
		name   string
		state  string
		invoke func(*Handler) error
	}{
		{
			name:  "approve waiting manual",
			state: StateWaitingManual,
			invoke: func(h *Handler) error {
				_, err := h.ApproveHandler(context.Background(), "root-1", "alice", "")
				return err
			},
		},
		{
			name:  "stop waiting manual",
			state: StateWaitingManual,
			invoke: func(h *Handler) error {
				_, err := h.StopHandler(context.Background(), "root-1", "alice", "")
				return err
			},
		},
		{
			name:  "stop waiting trigger",
			state: StateWaitingTrigger,
			invoke: func(h *Handler) error {
				_, err := h.StopHandler(context.Background(), "root-1", "alice", "")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			store.addBead("root-1", "in_progress", "", "", map[string]string{
				FieldState:             tt.state,
				FieldWaitingReason:     WaitManual,
				FieldIteration:         "2",
				FieldMaxIterations:     "5",
				FieldFormula:           "test-formula",
				FieldTarget:            "test-agent",
				FieldGateMode:          GateModeManual,
				FieldLastProcessedWisp: "wisp-iter-1",
			})
			store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
			store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)
			children, err := store.Children("root-1")
			if err != nil {
				t.Fatal(err)
			}
			childCalls := 0
			store.ChildrenFunc = func(parentID string) ([]BeadInfo, error) {
				if parentID != "root-1" {
					return nil, fmt.Errorf("unexpected Children(%q)", parentID)
				}
				childCalls++
				return children, nil
			}

			emitter := &terminalProofEmitter{store: store, rootID: "root-1", marker: "wisp-iter-2"}
			handler := &Handler{Store: store, Emitter: emitter}
			if err := tt.invoke(handler); err != nil {
				t.Fatalf("manual terminal transition: %v", err)
			}
			if err := emitter.err(); err != nil {
				t.Fatal(err)
			}
			meta, err := store.GetMetadata("root-1")
			if err != nil {
				t.Fatal(err)
			}
			if got := meta[FieldLastProcessedWisp]; got != "wisp-iter-2" {
				t.Fatalf("last_processed_wisp = %q, want repaired highest closed wisp", got)
			}
			if childCalls != 1 {
				t.Fatalf("terminal child snapshots = %d, want exactly 1", childCalls)
			}
		})
	}
}

func TestManualTerminalChildEvidenceFailureEmitsNoTerminalEvents(t *testing.T) {
	tests := []struct {
		name      string
		state     string
		eventType string
		invoke    func(*Handler) error
	}{
		{
			name: "approve", state: StateWaitingManual, eventType: EventManualApprove,
			invoke: func(h *Handler) error {
				_, err := h.ApproveHandler(context.Background(), "root-1", "alice", "")
				return err
			},
		},
		{
			name: "stop waiting manual", state: StateWaitingManual, eventType: EventManualStop,
			invoke: func(h *Handler) error {
				_, err := h.StopHandler(context.Background(), "root-1", "alice", "")
				return err
			},
		},
		{
			name: "stop waiting trigger", state: StateWaitingTrigger, eventType: EventManualStop,
			invoke: func(h *Handler) error {
				_, err := h.StopHandler(context.Background(), "root-1", "alice", "")
				return err
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			store := newFakeStore()
			store.addBead("root-1", "in_progress", "", "", map[string]string{
				FieldState: tt.state, FieldIteration: "1", FieldMaxIterations: "5",
				FieldFormula: "test-formula", FieldTarget: "test-agent", FieldGateMode: GateModeManual,
			})
			store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
			injected := errors.New("child evidence unavailable")
			store.ChildrenFunc = func(string) ([]BeadInfo, error) { return nil, injected }
			emitter := &fakeEmitter{}
			handler := &Handler{Store: store, Emitter: emitter}

			if err := tt.invoke(handler); !errors.Is(err, injected) {
				t.Fatalf("manual terminal transition error = %v, want child evidence error", err)
			}
			assertNoEventsOfType(t, emitter, EventIteration, EventTerminated, tt.eventType)
		})
	}
}

func TestTerminalEmittersObserveStateCloseAndMarkerProof(t *testing.T) {
	tests := []struct {
		name       string
		setup      func() (*Handler, *fakeStore, string)
		invoke     func(*Handler) error
		wantEvents []string
	}{
		{
			name: "normal",
			setup: func() (*Handler, *fakeStore, string) {
				h, store, _ := setupBasicHandler(t, map[string]string{
					FieldGateOutcomeWisp: "wisp-iter-1",
					FieldGateOutcome:     GatePass,
				})
				return h, store, "wisp-iter-1"
			},
			invoke: func(h *Handler) error {
				_, err := h.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
				return err
			},
			wantEvents: []string{EventIteration, EventTerminated},
		},
		{
			name: "manual approve",
			setup: func() (*Handler, *fakeStore, string) {
				h, store, _ := setupWaitingManualTerminalHandler()
				return h, store, "wisp-iter-1"
			},
			invoke: func(h *Handler) error {
				_, err := h.ApproveHandler(context.Background(), "root-1", "alice", "")
				return err
			},
			wantEvents: []string{EventTerminated, EventManualApprove},
		},
		{
			name: "manual stop",
			setup: func() (*Handler, *fakeStore, string) {
				h, store, _ := setupActiveManualStopHandler()
				return h, store, "wisp-iter-1"
			},
			invoke: func(h *Handler) error {
				_, err := h.StopHandler(context.Background(), "root-1", "alice", "")
				return err
			},
			wantEvents: []string{EventIteration, EventTerminated, EventManualStop},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, store, marker := tt.setup()
			emitter := &terminalProofEmitter{store: store, rootID: "root-1", marker: marker}
			handler.Emitter = emitter
			if err := tt.invoke(handler); err != nil {
				t.Fatalf("terminal transition: %v", err)
			}
			if err := emitter.err(); err != nil {
				t.Fatal(err)
			}
			for _, eventType := range tt.wantEvents {
				if !emitter.saw(eventType) {
					t.Errorf("did not emit %s", eventType)
				}
			}
		})
	}
}

func TestTerminalEventOrderIsStable(t *testing.T) {
	tests := []struct {
		name      string
		setup     func() (*Handler, *fakeEmitter)
		invoke    func(*Handler) error
		wantOrder []string
	}{
		{
			name: "normal terminal",
			setup: func() (*Handler, *fakeEmitter) {
				h, _, emitter := setupBasicHandler(t, map[string]string{
					FieldGateOutcomeWisp: "wisp-iter-1",
					FieldGateOutcome:     GatePass,
				})
				return h, emitter
			},
			invoke: func(h *Handler) error {
				_, err := h.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1")
				return err
			},
			wantOrder: []string{EventIteration, EventTerminated},
		},
		{
			name: "manual approve",
			setup: func() (*Handler, *fakeEmitter) {
				h, _, emitter := setupWaitingManualTerminalHandler()
				return h, emitter
			},
			invoke: func(h *Handler) error {
				_, err := h.ApproveHandler(context.Background(), "root-1", "alice", "")
				return err
			},
			wantOrder: []string{EventTerminated, EventManualApprove},
		},
		{
			name: "manual stop with force-closed wisp",
			setup: func() (*Handler, *fakeEmitter) {
				h, _, emitter := setupActiveManualStopHandler()
				return h, emitter
			},
			invoke: func(h *Handler) error {
				_, err := h.StopHandler(context.Background(), "root-1", "alice", "")
				return err
			},
			wantOrder: []string{EventIteration, EventTerminated, EventManualStop},
		},
		{
			name: "manual stop without force-closed wisp",
			setup: func() (*Handler, *fakeEmitter) {
				h, _, emitter := setupWaitingManualTerminalHandler()
				return h, emitter
			},
			invoke: func(h *Handler) error {
				_, err := h.StopHandler(context.Background(), "root-1", "alice", "")
				return err
			},
			wantOrder: []string{EventTerminated, EventManualStop},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, emitter := tt.setup()
			if err := tt.invoke(handler); err != nil {
				t.Fatalf("terminal transition: %v", err)
			}
			emitter.mu.Lock()
			gotOrder := make([]string, len(emitter.events))
			for i, event := range emitter.events {
				gotOrder[i] = event.Type
			}
			emitter.mu.Unlock()
			if len(gotOrder) != len(tt.wantOrder) {
				t.Fatalf("event order = %v, want %v", gotOrder, tt.wantOrder)
			}
			for i := range tt.wantOrder {
				if gotOrder[i] != tt.wantOrder[i] {
					t.Fatalf("event order = %v, want %v", gotOrder, tt.wantOrder)
				}
			}
		})
	}
}

func TestDroppedTerminalEventsDoNotRollBackProof(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, map[string]string{
		FieldGateOutcomeWisp: "wisp-iter-1",
		FieldGateOutcome:     GatePass,
	})
	handler.Emitter = droppingConvergenceEmitter{}
	if _, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-1"); err != nil {
		t.Fatalf("HandleWispClosed with dropping emitter: %v", err)
	}
	assertTerminalProof(t, store, "root-1", "wisp-iter-1")
}

func TestRecoveryTerminalEventsRequireDurableProof(t *testing.T) {
	for _, stage := range []string{"children", "state", "close", "marker"} {
		t.Run(stage, func(t *testing.T) {
			reconciler, store, emitter := setupReconciler(t)
			store.addBead("root-1", "in_progress", "", "", map[string]string{
				FieldState:          StateActive,
				FieldTerminalReason: TerminalStopped,
				FieldTerminalActor:  "operator:alice",
				FieldFormula:        "test-formula",
			})
			store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
			injected := errors.New("injected " + stage + " failure")
			if stage == "children" {
				store.ChildrenFunc = func(string) ([]BeadInfo, error) { return nil, injected }
			}
			store.SetMetadataErrFunc = func(key string) error {
				if (stage == "state" && key == FieldState) || (stage == "marker" && key == FieldLastProcessedWisp) {
					return injected
				}
				return nil
			}
			store.CloseBeadFunc = func(id, _ string) error {
				if stage == "close" && id == "root-1" {
					return injected
				}
				return nil
			}

			report, err := reconciler.ReconcileBeads(context.Background(), []string{"root-1"})
			if err != nil {
				t.Fatalf("ReconcileBeads: %v", err)
			}
			if report.Errors != 1 || !errors.Is(report.Details[0].Error, injected) {
				t.Fatalf("report = %+v, want injected recovery failure", report)
			}
			assertNoEventsOfType(t, emitter, EventTerminated)
		})
	}
}

func TestRecoveryRepairsClosedRootMarkerBeforeTerminalEvent(t *testing.T) {
	store := newFakeStore()
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:          StateActive,
		FieldTerminalReason: TerminalStopped,
		FieldTerminalActor:  "operator:alice",
		FieldFormula:        "test-formula",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	markerErr := errors.New("marker write unavailable")
	store.SetMetadataErrFunc = func(key string) error {
		if key == FieldLastProcessedWisp {
			return markerErr
		}
		return nil
	}

	firstEmitter := &fakeEmitter{}
	first := &Reconciler{Handler: &Handler{Store: store, Emitter: firstEmitter}}
	report, err := first.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("first ReconcileBeads: %v", err)
	}
	if report.Errors != 1 || !errors.Is(report.Details[0].Error, markerErr) {
		t.Fatalf("first report = %+v, want marker failure", report)
	}
	assertNoEventsOfType(t, firstEmitter, EventTerminated)
	info, err := store.GetBead("root-1")
	if err != nil || info.Status != "closed" {
		t.Fatalf("root after close-success/marker-failure = %+v, %v; want closed", info, err)
	}

	store.SetMetadataErrFunc = nil
	proofEmitter := &terminalProofEmitter{store: store, rootID: "root-1", marker: "wisp-iter-1"}
	fresh := &Reconciler{Handler: &Handler{Store: store, Emitter: proofEmitter}}
	report, err = fresh.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("fresh ReconcileBeads: %v", err)
	}
	if report.Errors != 0 || report.Details[0].Action != ActionCompletedTerminal {
		t.Fatalf("fresh report = %+v, want completed terminal repair", report)
	}
	if err := proofEmitter.err(); err != nil {
		t.Fatal(err)
	}
	if !proofEmitter.saw(EventTerminated) {
		t.Fatal("fresh explicit-ID recovery did not emit the stable terminal event after proof")
	}
}

func TestRecoveryTerminatedWithoutReasonIsPartialCreation(t *testing.T) {
	reconciler, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState: StateTerminated,
	})
	report, err := reconciler.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatalf("ReconcileBeads: %v", err)
	}
	if report.Errors != 0 {
		t.Fatalf("report = %+v", report)
	}
	meta, err := store.GetMetadata("root-1")
	if err != nil {
		t.Fatal(err)
	}
	if meta[FieldTerminalReason] != TerminalPartialCreation {
		t.Fatalf("terminal_reason = %q, want %q", meta[FieldTerminalReason], TerminalPartialCreation)
	}
}

func setupWaitingManualTerminalHandler() (*Handler, *fakeStore, *fakeEmitter) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateWaitingManual,
		FieldWaitingReason:     WaitManual,
		FieldIteration:         "1",
		FieldMaxIterations:     "5",
		FieldFormula:           "test-formula",
		FieldTarget:            "test-agent",
		FieldGateMode:          GateModeManual,
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	return &Handler{Store: store, Emitter: emitter}, store, emitter
}

func setupActiveManualStopHandler() (*Handler, *fakeStore, *fakeEmitter) {
	store := newFakeStore()
	emitter := &fakeEmitter{}
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:         StateActive,
		FieldIteration:     "0",
		FieldMaxIterations: "5",
		FieldFormula:       "test-formula",
		FieldTarget:        "test-agent",
		FieldGateMode:      GateModeManual,
		FieldActiveWisp:    "wisp-iter-1",
	})
	store.addBead("wisp-iter-1", "in_progress", "root-1", IdempotencyKey("root-1", 1), nil)
	return &Handler{Store: store, Emitter: emitter}, store, emitter
}

func assertNoEventsOfType(t *testing.T, emitter *fakeEmitter, eventTypes ...string) {
	t.Helper()
	for _, eventType := range eventTypes {
		if _, ok := emitter.findEvent(eventType); ok {
			t.Errorf("emitted %s before durable terminal proof", eventType)
		}
	}
}

func assertTerminalProof(t *testing.T, store *fakeStore, rootID, marker string) {
	t.Helper()
	meta, err := store.GetMetadata(rootID)
	if err != nil {
		t.Fatalf("read terminal metadata: %v", err)
	}
	if meta[FieldState] != StateTerminated {
		t.Errorf("state = %q, want %q", meta[FieldState], StateTerminated)
	}
	if marker != "" && meta[FieldLastProcessedWisp] != marker {
		t.Errorf("last_processed_wisp = %q, want %q", meta[FieldLastProcessedWisp], marker)
	}
	info, err := store.GetBead(rootID)
	if err != nil {
		t.Fatalf("read terminal root: %v", err)
	}
	if info.Status != "closed" {
		t.Errorf("root status = %q, want closed", info.Status)
	}
}

type terminalProofEmitter struct {
	mu     sync.Mutex
	store  *fakeStore
	rootID string
	marker string
	events map[string]int
	errs   []error
}

func (e *terminalProofEmitter) Emit(eventType, _, _ string, _ json.RawMessage, _ bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.events == nil {
		e.events = make(map[string]int)
	}
	e.events[eventType]++
	meta, err := e.store.GetMetadata(e.rootID)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s observed metadata error: %w", eventType, err))
		return
	}
	info, err := e.store.GetBead(e.rootID)
	if err != nil {
		e.errs = append(e.errs, fmt.Errorf("%s observed root error: %w", eventType, err))
		return
	}
	if meta[FieldState] != StateTerminated || info.Status != "closed" || (e.marker != "" && meta[FieldLastProcessedWisp] != e.marker) {
		e.errs = append(e.errs, fmt.Errorf(
			"%s observed incomplete proof: state=%q status=%q marker=%q want_marker=%q",
			eventType, meta[FieldState], info.Status, meta[FieldLastProcessedWisp], e.marker,
		))
	}
}

func (e *terminalProofEmitter) saw(eventType string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.events[eventType] > 0
}

func (e *terminalProofEmitter) err() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	return errors.Join(e.errs...)
}

type droppingConvergenceEmitter struct{}

func (droppingConvergenceEmitter) Emit(string, string, string, json.RawMessage, bool) {}
