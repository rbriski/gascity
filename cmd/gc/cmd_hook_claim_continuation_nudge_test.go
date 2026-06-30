package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// workflowRootCandidates returns a slice of candidates suitable for testing
// the hook-claim-continuation-nudge path: a pool-routed graph.v2 workflow root
// that routes to route-1 and carries the pool continuation group so
// preassignHookContinuationGroup will assign siblings. The formula_contract and
// pool continuation_group are required by isPoolGraphV2WorkflowRoot — they are
// what a real pool graph.v2 root carries (graphroute stamps the pool group on
// MetadataOnly bindings; compile stamps the contract on the root).
func workflowRootCandidates() []beads.Bead {
	return []beads.Bead{{
		ID:     "root-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "workflow",
			"gc.formula_contract":   "graph.v2",
			"gc.run_target":         "route-1",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "pool-workflow",
		},
	}}
}

// namedWorkflowRootCandidates returns a workflow root that pre-assigns siblings
// but is NOT a pool graph.v2 root — it lacks the graph.v2 contract and carries
// a non-pool continuation group, mirroring a named-session workflow root. It
// must NOT enqueue a continuation nudge.
func namedWorkflowRootCandidates() []beads.Bead {
	return []beads.Bead{{
		ID:     "root-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "workflow",
			"gc.run_target":         "route-1",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}}
}

// stepBeadCandidates returns candidates whose gc.kind is "task" (a step
// bead), which must NOT trigger the hook-claim-continuation nudge even when
// siblings are assigned. Step beads are routed via gc.routed_to (not
// gc.run_target), so we set that for route matching.
func stepBeadCandidates() []beads.Bead {
	return []beads.Bead{{
		ID:     "step-1",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":               "task",
			"gc.routed_to":          "route-1",
			"gc.root_bead_id":       "root-1",
			"gc.continuation_group": "group-a",
		},
	}}
}

// buildContinuationNudgeOps returns a hookClaimOps that captures nudge
// enqueue calls. siblingIDs controls what ListContinuation returns.
func buildContinuationNudgeOps(candidates []beads.Bead, siblingIDs []string, enqueued *[]string) hookClaimOps {
	siblings := make([]beads.Bead, 0, len(siblingIDs))
	for _, id := range siblingIDs {
		siblings = append(siblings, beads.Bead{
			ID:     id,
			Status: "open",
			Metadata: map[string]string{
				"gc.kind":               "workflow",
				"gc.run_target":         "route-1",
				"gc.root_bead_id":       "root-1",
				"gc.continuation_group": "group-a",
			},
		})
	}
	return hookClaimOps{
		Runner: func(string, string) (string, error) {
			out, _ := json.Marshal(candidates)
			return string(out), nil
		},
		Claim: func(_ context.Context, _ string, _ []string, beadID, assignee string) (beads.Bead, bool, error) {
			meta := candidates[0].Metadata
			return beads.Bead{ID: beadID, Assignee: assignee, Status: "in_progress", Metadata: meta}, true, nil
		},
		ListContinuation: func(_ context.Context, _ string, _ []string, _, _ string) ([]beads.Bead, error) {
			return siblings, nil
		},
		AssignContinuation: func(_ context.Context, _ string, _ []string, _, _ string) error {
			return nil
		},
		DrainAck: func(io.Writer) error { return nil },
		EnqueueContinuationNudge: func(assignee string) {
			*enqueued = append(*enqueued, assignee)
		},
	}
}

// TestHookClaimContinuationNudge_WorkflowRootEnqueuesNudge verifies that
// claiming a workflow root that pre-assigns at least one sibling enqueues a
// hook-claim-continuation nudge for the claiming session name.
func TestHookClaimContinuationNudge_WorkflowRootEnqueuesNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(workflowRootCandidates(), []string{"sib-1", "sib-2"}, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 1 || enqueued[0] != "gascity/worker/slot-0" {
		t.Fatalf("continuation nudge enqueued for %v, want [gascity/worker/slot-0]", enqueued)
	}
}

// TestHookClaimContinuationNudge_StepBeadNoNudge verifies that claiming a
// step bead (gc.kind="task") does NOT enqueue a hook-claim-continuation
// nudge even when siblings are pre-assigned. The nudge is only needed for
// self-propelling pool workflow roots; step claims happen while the session
// is already executing and will naturally poll.
func TestHookClaimContinuationNudge_StepBeadNoNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(stepBeadCandidates(), []string{"sib-1"}, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued for a step-bead claim, got %v", enqueued)
	}
}

// TestHookClaimContinuationNudge_NamedWorkflowRootNoNudge verifies that
// claiming a workflow root that pre-assigns siblings but is NOT a pool graph.v2
// root (no graph.v2 contract, non-pool continuation group) does NOT enqueue a
// nudge. This is the named-session workflow root case the narrowed gate
// (isPoolGraphV2WorkflowRoot) excludes — the broader gc.kind==workflow gate
// fired for it incorrectly (#3554 review).
func TestHookClaimContinuationNudge_NamedWorkflowRootNoNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(namedWorkflowRootCandidates(), []string{"sib-1", "sib-2"}, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued for a non-pool-graph.v2 workflow root, got %v", enqueued)
	}
}

// TestHookClaimContinuationNudge_NoSiblingsNoNudge verifies that claiming a
// workflow root that has NO unassigned siblings does NOT enqueue a nudge.
// An empty continuation means the session has no further work queued and
// should not be immediately propelled.
func TestHookClaimContinuationNudge_NoSiblingsNoNudge(t *testing.T) {
	var enqueued []string
	ops := buildContinuationNudgeOps(workflowRootCandidates(), nil /* no siblings */, &enqueued)

	var stdout, stderr bytes.Buffer
	code := doHookClaim("query", ".", hookClaimOptions{
		Assignee:           "gascity/worker/slot-0",
		IdentityCandidates: []string{"gascity/worker/slot-0"},
		RouteTargets:       []string{"route-1"},
		JSON:               true,
	}, ops, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("doHookClaim() = %d, want 0; stderr=%s", code, stderr.String())
	}
	if len(enqueued) != 0 {
		t.Fatalf("continuation nudge must not be enqueued when no siblings assigned, got %v", enqueued)
	}
}

// supervisorCityConfig builds a minimal city config with one pool agent whose
// transport is `session`, for the poller-spawn tests.
func nudgeTargetCityConfig(dispatcher, session string) *config.City {
	return &config.City{
		Daemon: config.DaemonConfig{NudgeDispatcher: dispatcher},
		Agents: []config.Agent{{Name: "worker", Dir: "gascity", Session: session}},
	}
}

// TestHookContinuationNudgeTarget_NilCfgFallsBackToBare verifies the
// best-effort fallback: with no resolvable config the target is the bare
// {cityPath, sessionName} shape (legacy behavior), never a panic.
func TestHookContinuationNudgeTarget_NilCfgFallsBackToBare(t *testing.T) {
	target := hookContinuationNudgeTarget("/city", "gascity/worker", nil)
	if target.cfg != nil {
		t.Fatalf("nil cfg must leave target.cfg nil, got %+v", target.cfg)
	}
	if target.cityPath != "/city" || target.sessionName != "gascity/worker" {
		t.Fatalf("bare target mismatch: %+v", target)
	}
}

// TestHookContinuationNudgeTarget_PopulatesCfg verifies the poller-race fix:
// the target carries the city config so maybeStartNudgePoller's
// nudgeDispatcherIsSupervisor(target.cfg) check can fire. A nil cfg there
// defaults to legacy mode and spawns a sidecar poller that races the supervisor
// dispatcher (#3838 review).
func TestHookContinuationNudgeTarget_PopulatesCfg(t *testing.T) {
	cfg := nudgeTargetCityConfig("supervisor", "acp")
	target := hookContinuationNudgeTarget("/city", "gascity/worker", cfg)
	if !nudgeDispatcherIsSupervisor(target.cfg) {
		t.Fatal("target must carry cfg so the supervisor short-circuit fires")
	}
}

// TestMaybeStartNudgePoller_HookContinuationTargets exercises the end-to-end
// spawn decision for the hook-continuation target across dispatcher modes and
// transports. Supervisor mode and ACP transport must NOT spawn a per-session
// poller; legacy mode with a pollable transport must.
func TestMaybeStartNudgePoller_HookContinuationTargets(t *testing.T) {
	cases := []struct {
		name       string
		dispatcher string
		session    string
		wantSpawn  bool
	}{
		{"supervisor mode skips (race fix)", "supervisor", "tmux", false},
		{"legacy acp skips (sidecar cannot deliver)", "legacy", "acp", false},
		{"legacy tmux spawns", "legacy", "tmux", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			orig := startNudgePoller
			t.Cleanup(func() { startNudgePoller = orig })
			var spawned int
			startNudgePoller = func(_, _, _ string) error { spawned++; return nil }

			target := hookContinuationNudgeTarget(t.TempDir(), "gascity/worker", nudgeTargetCityConfig(tc.dispatcher, tc.session))
			maybeStartNudgePoller(target)

			if got := spawned > 0; got != tc.wantSpawn {
				t.Fatalf("spawned=%d, want spawn=%v", spawned, tc.wantSpawn)
			}
		})
	}
}
