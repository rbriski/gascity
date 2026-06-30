package main

// Tests for the hook-claim-continuation nudge enqueue seam (ga-7n7vth.1).
//
// These tests define the expected behavior for ga-7n7vth.2:
//
//   - A newly claimed pool graph.v2 workflow root that pre-assigns at least one
//     continuation sibling must enqueue exactly one queued nudge with source
//     "hook-claim-continuation", targeting the claiming session by name, using
//     the canonical propulsion message.
//   - Non-workflow step-bead claims must not enqueue the nudge.
//   - Re-found existing assignments must not enqueue the nudge (idempotence).
//   - Workflow root claims with zero continuation siblings must not enqueue.
//
// All tests that assert the nudge IS enqueued (TestHookClaimWorkflowRootEnqueuesContinuationNudge)
// fail against the current codebase until ga-7n7vth.2 adds the call site in
// writeHookClaimWorkResultForBead. Tests that assert the nudge is NOT enqueued
// pass now and serve as guardrails against over-enqueueing after the fix lands.

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

const (
	hookClaimContinuationNudgeSource  = "hook-claim-continuation"
	hookClaimContinuationNudgeMessage = "Work slung. Check your hook."
)

// makeWorkflowRootBead returns a bead shaped like a pool graph.v2 workflow root
// ready to be claimed: gc.kind=workflow, gc.run_target pointing at the pool
// template, and gc.root_bead_id / gc.continuation_group so pre-assignment runs.
func makeWorkflowRootBead(id, runTarget, rootID, continuationGroup string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "workflow",
			"gc.run_target":         runTarget,
			"gc.root_bead_id":       rootID,
			"gc.continuation_group": continuationGroup,
		},
	}
}

// makeStepBead returns a bead shaped like a formula step (gc.kind=step) with
// the same continuation metadata a workflow root has. Step beads reach the
// work queue via gc.routed_to (set by preassignHookContinuationGroup), not
// via gc.run_target, so routedTo is the claiming session name.
func makeStepBead(id, routedTo, rootID, continuationGroup string) beads.Bead {
	return beads.Bead{
		ID:     id,
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "step",
			"gc.routed_to":          routedTo,
			"gc.root_bead_id":       rootID,
			"gc.continuation_group": continuationGroup,
		},
	}
}

// captureNudgeOps returns a hookClaimOps that captures the city path and nudge
// item passed to EnqueueContinuationNudge. The capture is written to *calls.
func captureNudgeOps(t *testing.T, calls *[]queuedNudge, bead beads.Bead, sibling beads.Bead) hookClaimOps {
	t.Helper()
	output, err := json.Marshal([]beads.Bead{bead})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		Claim: func(_ context.Context, _ string, _ []string, _, assignee string) (beads.Bead, bool, error) {
			claimed := bead
			claimed.Assignee = assignee
			claimed.Status = "in_progress"
			return claimed, true, nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			if sibling.ID == "" {
				return nil, nil
			}
			return []beads.Bead{sibling}, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error {
			return nil
		},
		DrainAck:          func(io.Writer) error { return nil },
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkBranch:   func(_ context.Context, _ string, _ []string, _, _, _ string) error { return nil },
		RecordSessionPointers: func(_ context.Context, _ string, _ []string, _, _, _, _ string) error {
			return nil
		},
		EnqueueContinuationNudge: func(_ string, item queuedNudge) error {
			*calls = append(*calls, item)
			return nil
		},
	}
}

// TestHookClaimWorkflowRootEnqueuesContinuationNudge verifies that claiming a
// new pool graph.v2 workflow root with at least one pre-assigned continuation
// sibling enqueues exactly one queued nudge with the canonical propulsion
// semantics (source, message, agent equal to the claiming session name).
//
// This test is skipped until ga-7n7vth.2 removes the t.Skip call below and
// adds the production call site in writeHookClaimWorkResultForBead. After the
// Skip is removed this test will fail (RED) until the call site is added, then
// pass (GREEN). That is the intended TDD sequence.
func TestHookClaimWorkflowRootEnqueuesContinuationNudge(t *testing.T) {
	t.Skip("ga-7n7vth.2: remove this Skip and add the EnqueueContinuationNudge call site in writeHookClaimWorkResultForBead")
	root := makeWorkflowRootBead("root-1", "pool-worker", "root-1", "group-a")
	sibling := beads.Bead{
		ID:     "step-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "step",
			"gc.run_target":         "pool-worker",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}

	var calls []queuedNudge
	ops := captureNudgeOps(t, &calls, root, sibling)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "", hookClaimOptions{
		Assignee:           "pool-worker/slot-0",
		IdentityCandidates: []string{"pool-worker/slot-0"},
		RouteTargets:       []string{"pool-worker"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	if len(calls) != 1 {
		t.Fatalf("EnqueueContinuationNudge called %d times, want exactly 1; stderr=%s", len(calls), stderr.String())
	}
	got := calls[0]
	if got.Agent != "pool-worker/slot-0" {
		t.Errorf("nudge.Agent = %q, want %q", got.Agent, "pool-worker/slot-0")
	}
	if got.Source != hookClaimContinuationNudgeSource {
		t.Errorf("nudge.Source = %q, want %q", got.Source, hookClaimContinuationNudgeSource)
	}
	if got.Message != hookClaimContinuationNudgeMessage {
		t.Errorf("nudge.Message = %q, want %q", got.Message, hookClaimContinuationNudgeMessage)
	}
}

// TestHookClaimStepBeadDoesNotEnqueueContinuationNudge verifies that claiming a
// non-workflow step bead — even one that has continuation metadata and pre-assigns
// siblings — does not enqueue the hook-claim-continuation nudge. Only workflow
// root claims propel the session; step claims happen while the session is already
// active.
func TestHookClaimStepBeadDoesNotEnqueueContinuationNudge(t *testing.T) {
	// Step beads are routed to the concrete session name (set by
	// preassignHookContinuationGroup on the prior root claim), so the route
	// target here is the session name, not the pool template.
	step := makeStepBead("step-1", "pool-worker/slot-0", "root-1", "group-a")
	sibling := beads.Bead{
		ID:     "step-2",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "step",
			"gc.routed_to":          "pool-worker/slot-0",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}

	var calls []queuedNudge
	ops := captureNudgeOps(t, &calls, step, sibling)

	var stdout, stderr bytes.Buffer
	// A step bead's route target is the concrete session name (pool-worker/slot-0),
	// not the pool template name; include both to mirror real hook invocation.
	code := doHookClaim("query", "", hookClaimOptions{
		Assignee:           "pool-worker/slot-0",
		IdentityCandidates: []string{"pool-worker/slot-0"},
		RouteTargets:       []string{"pool-worker/slot-0", "pool-worker"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	if len(calls) != 0 {
		t.Errorf("EnqueueContinuationNudge called %d times for step-bead claim, want 0", len(calls))
	}
}

// TestHookClaimExistingAssignmentDoesNotEnqueueContinuationNudge verifies that
// re-finding a workflow root that is already assigned to the claiming session
// (idempotent re-find on retry) does not enqueue an additional continuation
// nudge. The session is already active; a second nudge would cause a redundant
// hook re-entry.
func TestHookClaimExistingAssignmentDoesNotEnqueueContinuationNudge(t *testing.T) {
	root := beads.Bead{
		ID:       "root-1",
		Status:   "in_progress",
		Assignee: "pool-worker/slot-0",
		Metadata: map[string]string{
			"gc.kind":               "workflow",
			"gc.run_target":         "pool-worker",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}
	output, err := json.Marshal([]beads.Bead{root})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	var calls []queuedNudge
	sibling := beads.Bead{
		ID:     "step-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "step",
			"gc.run_target":         "pool-worker",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) { return string(output), nil },
		// Claim is not called for existing assignments; provide a no-op to satisfy applyDefaults.
		Claim: func(_ context.Context, _ string, _ []string, _, _ string) (beads.Bead, bool, error) {
			return beads.Bead{}, false, nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return []beads.Bead{sibling}, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error {
			return nil
		},
		DrainAck:          func(io.Writer) error { return nil },
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkBranch:   func(_ context.Context, _ string, _ []string, _, _, _ string) error { return nil },
		RecordSessionPointers: func(_ context.Context, _ string, _ []string, _, _, _, _ string) error {
			return nil
		},
		EnqueueContinuationNudge: func(_ string, item queuedNudge) error {
			calls = append(calls, item)
			return nil
		},
	}

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "", hookClaimOptions{
		Assignee:           "pool-worker/slot-0",
		IdentityCandidates: []string{"pool-worker/slot-0"},
		RouteTargets:       []string{"pool-worker"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	if len(calls) != 0 {
		t.Errorf("EnqueueContinuationNudge called %d times for re-found assignment, want 0", len(calls))
	}
}

// TestHookClaimZeroContinuationDoesNotEnqueueContinuationNudge verifies that
// claiming a workflow root where no continuation siblings are available — either
// because the formula has no steps or all step beads are already assigned —
// does not enqueue the nudge. There is nothing to propel into.
func TestHookClaimZeroContinuationDoesNotEnqueueContinuationNudge(t *testing.T) {
	root := makeWorkflowRootBead("root-1", "pool-worker", "root-1", "group-a")

	var calls []queuedNudge
	// sibling is empty: ListContinuation returns nothing.
	ops := captureNudgeOps(t, &calls, root, beads.Bead{})

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", "", hookClaimOptions{
		Assignee:           "pool-worker/slot-0",
		IdentityCandidates: []string{"pool-worker/slot-0"},
		RouteTargets:       []string{"pool-worker"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}

	if len(calls) != 0 {
		t.Errorf("EnqueueContinuationNudge called %d times for zero-continuation claim, want 0", len(calls))
	}
}
