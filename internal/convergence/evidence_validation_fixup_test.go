package convergence

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestApproveHandlerRejectsCorruptChildEvidenceBeforeTerminalProof(t *testing.T) {
	valid := BeadInfo{
		ID:             "wisp-iter-1",
		Status:         "closed",
		ParentID:       "root-1",
		IdempotencyKey: IdempotencyKey("root-1", 1),
	}
	tests := []struct {
		name    string
		corrupt BeadInfo
	}{
		{name: "noncanonical key", corrupt: BeadInfo{ID: "bad", Status: "closed", ParentID: "root-1", IdempotencyKey: "converge:root-1:iter:01"}},
		{name: "wrong parent", corrupt: BeadInfo{ID: "bad", Status: "closed", ParentID: "other-root", IdempotencyKey: IdempotencyKey("root-1", 2)}},
		{name: "empty id", corrupt: BeadInfo{Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)}},
		{name: "duplicate id", corrupt: BeadInfo{ID: valid.ID, Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)}},
		{name: "duplicate iteration", corrupt: BeadInfo{ID: "duplicate", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)}},
		{name: "iteration gap", corrupt: BeadInfo{ID: "gap", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 3)}},
		{name: "invalid status", corrupt: BeadInfo{ID: "bad", Status: "queued", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, store, emitter := setupBasicHandler(t, map[string]string{
				FieldState:             StateWaitingManual,
				FieldLastProcessedWisp: valid.ID,
			})
			store.ChildrenFunc = func(parentID string) ([]BeadInfo, error) {
				if parentID != "root-1" {
					t.Fatalf("Children(%q), want root-1", parentID)
				}
				return []BeadInfo{valid, tt.corrupt}, nil
			}
			closed := false
			store.CloseBeadFunc = func(_, _ string) error {
				closed = true
				return nil
			}

			_, err := handler.ApproveHandler(context.Background(), "root-1", "alice", "")
			if err == nil {
				t.Fatal("ApproveHandler succeeded with corrupt child evidence")
			}
			if closed || len(store.WriteLog) != 0 || len(emitter.events) != 0 {
				t.Fatalf("effects after corrupt evidence: closed=%v writes=%v events=%v", closed, store.WriteLog, emitter.events)
			}
		})
	}
}

func TestReconcileTerminalRejectsCorruptChildEvidenceBeforeProof(t *testing.T) {
	valid := BeadInfo{
		ID:             "wisp-iter-1",
		Status:         "closed",
		ParentID:       "root-1",
		IdempotencyKey: IdempotencyKey("root-1", 1),
	}
	tests := []struct {
		name    string
		corrupt BeadInfo
	}{
		{name: "noncanonical key", corrupt: BeadInfo{ID: "bad", Status: "closed", ParentID: "root-1", IdempotencyKey: "converge:root-1:iter:x"}},
		{name: "wrong parent", corrupt: BeadInfo{ID: "bad", Status: "closed", ParentID: "other-root", IdempotencyKey: IdempotencyKey("root-1", 2)}},
		{name: "empty id", corrupt: BeadInfo{Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)}},
		{name: "duplicate id", corrupt: BeadInfo{ID: valid.ID, Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)}},
		{name: "duplicate iteration", corrupt: BeadInfo{ID: "duplicate", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)}},
		{name: "iteration gap", corrupt: BeadInfo{ID: "gap", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 3)}},
		{name: "invalid status", corrupt: BeadInfo{ID: "bad", Status: "queued", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec, store, emitter := setupReconciler(t)
			store.addBead("root-1", "in_progress", "", "", map[string]string{
				FieldState:          StateTerminated,
				FieldTerminalReason: TerminalNoConvergence,
				FieldTerminalActor:  "controller",
			})
			store.ChildrenFunc = func(string) ([]BeadInfo, error) {
				return []BeadInfo{valid, tt.corrupt}, nil
			}
			closed := false
			store.CloseBeadFunc = func(_, _ string) error {
				closed = true
				return nil
			}

			report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
			if err != nil {
				t.Fatal(err)
			}
			if report.Errors != 1 || report.Details[0].Error == nil {
				t.Fatalf("report = %+v, want corrupt-evidence error", report)
			}
			if closed || len(store.WriteLog) != 0 || len(emitter.events) != 0 {
				t.Fatalf("effects after corrupt evidence: closed=%v writes=%v events=%v", closed, store.WriteLog, emitter.events)
			}
		})
	}
}

func TestReconcileStaleActiveRecoveryRejectsForeignMarkerBeforeMutation(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "missing-active",
		FieldLastProcessedWisp: "foreign-marker",
	})
	store.addBead("other-root", "in_progress", "", "", nil)
	store.addBead("foreign-marker", "closed", "other-root", IdempotencyKey("root-1", 1), nil)
	store.addBead("candidate", "open", "root-1", IdempotencyKey("root-1", 2), nil)

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 1 || report.Details[0].Error == nil || !strings.Contains(report.Details[0].Error.Error(), "last processed") {
		t.Fatalf("report = %+v, want foreign-marker error", report)
	}
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "missing-active" || len(store.WriteLog) != 0 {
		t.Fatalf("foreign marker mutated root: metadata=%#v writes=%v", meta, store.WriteLog)
	}
	info, _ := store.GetBead("root-1")
	if info.Status == "closed" {
		t.Fatal("foreign marker closed the root")
	}
}

func TestReconcileStaleActiveRecoveryRejectsMismatchedCandidateIDBeforeMutation(t *testing.T) {
	rec, store, _ := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "missing-active",
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("wisp-iter-1", "closed", "root-1", IdempotencyKey("root-1", 1), nil)
	store.addBead("candidate", "open", "root-1", IdempotencyKey("root-1", 2), nil)
	marker, _ := store.GetBead("wisp-iter-1")
	candidate, _ := store.GetBead("candidate")
	candidate.ID = "wrong-id"
	store.GetBeadFunc = func(id string) (BeadInfo, error) {
		switch id {
		case "missing-active":
			return BeadInfo{}, fmt.Errorf("missing active: %w", beads.ErrNotFound)
		case "wisp-iter-1":
			return marker, nil
		case "candidate":
			return candidate, nil
		default:
			return BeadInfo{}, fmt.Errorf("unexpected GetBead(%q)", id)
		}
	}

	report, err := rec.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 1 || report.Details[0].Error == nil {
		t.Fatalf("report = %+v, want mismatched-candidate error", report)
	}
	meta, _ := store.GetMetadata("root-1")
	if meta[FieldActiveWisp] != "missing-active" || len(store.WriteLog) != 0 {
		t.Fatalf("mismatched candidate mutated root: metadata=%#v writes=%v", meta, store.WriteLog)
	}
}

func TestReconcileActiveRejectsForeignLiveWispBeforeMutation(t *testing.T) {
	reconciler, store, emitter := setupReconciler(t)
	store.addBead("root-1", "in_progress", "", "", map[string]string{
		FieldState:      StateActive,
		FieldActiveWisp: "foreign-wisp",
	})
	store.addBead("other-root", "in_progress", "", "", nil)
	store.addBead("foreign-wisp", "in_progress", "other-root", IdempotencyKey("other-root", 1), nil)

	report, err := reconciler.ReconcileBeads(context.Background(), []string{"root-1"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Errors != 1 || report.Details[0].Error == nil || !strings.Contains(report.Details[0].Error.Error(), "active wisp") {
		t.Fatalf("report = %+v, want foreign active-wisp evidence error", report)
	}
	if len(store.WriteLog) != 0 || len(emitter.events) != 0 {
		t.Fatalf("foreign live wisp caused effects: writes=%v events=%v", store.WriteLog, emitter.events)
	}
	foreign, getErr := store.GetBead("foreign-wisp")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if foreign.Status != "in_progress" {
		t.Fatalf("foreign wisp status = %q, want in_progress", foreign.Status)
	}
}

func TestStopHandlerRejectsForeignActiveWispBeforeEffects(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "foreign-wisp",
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("other-root", "in_progress", "", "", nil)
	store.addBead("foreign-wisp", "in_progress", "other-root", IdempotencyKey("other-root", 1), nil)

	_, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err == nil || !strings.Contains(err.Error(), "active wisp") {
		t.Fatalf("StopHandler error = %v, want foreign active-wisp evidence error", err)
	}
	if len(store.WriteLog) != 0 || len(emitter.events) != 0 {
		t.Fatalf("foreign active wisp caused root effects: writes=%v events=%v", store.WriteLog, emitter.events)
	}
	foreign, getErr := store.GetBead("foreign-wisp")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if foreign.Status != "in_progress" {
		t.Fatalf("foreign wisp status = %q, want in_progress", foreign.Status)
	}
	root, getErr := store.GetBead("root-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if root.Status == "closed" {
		t.Fatal("foreign active-wisp evidence closed the root")
	}
}

func TestHandleWispClosedRejectsIterationGapBeforeMutation(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldLastProcessedWisp: "wisp-iter-1",
	})
	store.addBead("wisp-iter-3", "closed", "root-1", IdempotencyKey("root-1", 3), nil)

	_, err := handler.HandleWispClosed(context.Background(), "root-1", "wisp-iter-3")
	if err == nil || !strings.Contains(err.Error(), "iteration gap") {
		t.Fatalf("HandleWispClosed error = %v, want iteration-gap evidence error", err)
	}
	if len(store.WriteLog) != 0 || len(emitter.events) != 0 {
		t.Fatalf("iteration gap caused effects: writes=%v events=%v", store.WriteLog, emitter.events)
	}
}

func TestRecoverCurrentActiveWispRejectsCorruptFallbackEvidence(t *testing.T) {
	tests := []struct {
		name     string
		children []BeadInfo
	}{
		{name: "wrong parent", children: []BeadInfo{{ID: "candidate", Status: "open", ParentID: "other-root", IdempotencyKey: IdempotencyKey("root-1", 1)}}},
		{name: "iteration gap", children: []BeadInfo{{ID: "candidate", Status: "open", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)}}},
		{name: "internal iteration gap", children: []BeadInfo{
			{ID: "wisp-iter-1", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)},
			{ID: "candidate", Status: "open", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 3)},
		}},
		{name: "invalid status", children: []BeadInfo{{ID: "candidate", Status: "queued", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)}}},
		{name: "malformed key", children: []BeadInfo{{ID: "candidate", Status: "closed", ParentID: "root-1", IdempotencyKey: "converge:root-1:iter:x"}}},
		{name: "empty id", children: []BeadInfo{{Status: "open", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)}}},
		{name: "duplicate iteration", children: []BeadInfo{
			{ID: "candidate-a", Status: "open", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)},
			{ID: "candidate-b", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)},
		}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			handler, store, _ := setupBasicHandler(t, nil)
			store.ChildrenFunc = func(string) ([]BeadInfo, error) { return tt.children, nil }
			_, found, err := handler.recoverCurrentActiveWisp("root-1", "")
			if err == nil {
				t.Fatalf("recoverCurrentActiveWisp found=%v, want corruption error", found)
			}
		})
	}
}

func TestRecoverCurrentActiveWispRejectsGapAfterMarker(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, nil)
	store.ChildrenFunc = func(string) ([]BeadInfo, error) {
		return []BeadInfo{
			{ID: "wisp-iter-1", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)},
			{ID: "wisp-iter-3", Status: "open", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 3)},
		}, nil
	}
	store.FindByIdempotencyKeyFunc = func(key string) (string, bool, error) {
		t.Fatalf("gap recovery must fail before looking up %q", key)
		return "", false, nil
	}

	_, found, err := handler.recoverCurrentActiveWisp("root-1", "wisp-iter-1")
	if err == nil || found || !strings.Contains(err.Error(), "iteration gap") {
		t.Fatalf("recoverCurrentActiveWisp = (found=%t, err=%v), want iteration-gap error", found, err)
	}
	if len(store.WriteLog) != 0 {
		t.Fatalf("gap recovery writes = %v, want none", store.WriteLog)
	}
}

func TestRecoverCurrentActiveWispRejectsMarkerSnapshotDisagreement(t *testing.T) {
	handler, store, _ := setupBasicHandler(t, nil)
	store.ChildrenFunc = func(string) ([]BeadInfo, error) {
		return []BeadInfo{
			{ID: "snapshot-iter-1", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 1)},
			// The point read for wisp-iter-1 says iteration 1, while this independently
			// checked child snapshot says the same ID is iteration 2.
			{ID: "wisp-iter-1", Status: "closed", ParentID: "root-1", IdempotencyKey: IdempotencyKey("root-1", 2)},
		}, nil
	}
	store.FindByIdempotencyKeyFunc = func(key string) (string, bool, error) {
		t.Fatalf("disagreeing marker evidence must fail before looking up %q", key)
		return "", false, nil
	}

	_, found, err := handler.recoverCurrentActiveWisp("root-1", "wisp-iter-1")
	if err == nil || found || !strings.Contains(err.Error(), "disagrees") {
		t.Fatalf("recoverCurrentActiveWisp = (found=%t, err=%v), want marker-disagreement error", found, err)
	}
	if len(store.WriteLog) != 0 {
		t.Fatalf("marker-disagreement recovery writes = %v, want none", store.WriteLog)
	}
}

func TestStopHandlerYieldsClosedAdoptedSuccessorToTickOwner(t *testing.T) {
	handler, store, emitter := setupBasicHandler(t, map[string]string{
		FieldState:             StateActive,
		FieldActiveWisp:        "wisp-iter-1",
		FieldLastProcessedWisp: "",
		FieldMaxIterations:     "5",
		FieldGateMode:          GateModeCondition,
		FieldGateOutcomeWisp:   "wisp-iter-1",
		FieldGateOutcome:       GateFail,
	})
	// Models a crash after the successor was activated but before the parent
	// commit named it. Both children close before the manual stop is handled.
	store.addBead("wisp-iter-2", "closed", "root-1", IdempotencyKey("root-1", 2), nil)

	_, err := handler.StopHandler(context.Background(), "root-1", "alice", "")
	if err == nil || !strings.Contains(err.Error(), "tick") {
		t.Fatalf("StopHandler error = %v, want yield-to-tick error", err)
	}
	meta, getErr := store.GetMetadata("root-1")
	if getErr != nil {
		t.Fatal(getErr)
	}
	if meta[FieldState] != StateActive || meta[FieldActiveWisp] != "wisp-iter-2" || meta[FieldLastProcessedWisp] != "wisp-iter-1" {
		t.Fatalf("metadata = %#v, want successor pending for tick owner", meta)
	}
	info, _ := store.GetBead("root-1")
	if info.Status == "closed" || meta[FieldTerminalReason] != "" {
		t.Fatalf("stop incorrectly terminalized root: info=%+v metadata=%#v", info, meta)
	}
	if _, ok := emitter.findEvent(EventTerminated); ok {
		t.Fatal("yielded stop emitted terminal event")
	}
	for _, event := range emitter.events {
		if event.Type != EventIteration {
			continue
		}
		var payload IterationPayload
		if decodeErr := json.Unmarshal(event.Payload, &payload); decodeErr != nil {
			t.Fatal(decodeErr)
		}
		if payload.WispID == "wisp-iter-2" {
			t.Fatal("stop stamped an iteration event for the tick-owned successor")
		}
	}
}
