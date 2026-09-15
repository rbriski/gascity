package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

type publishRunMapSpy struct {
	calls  int
	runID  string
	beadID string
	keys   []string
	err    error
}

type recordCurrentBeadSpy struct {
	calls         int
	sessionBeadID string
	workBeadID    string
	err           error
}

func (s *recordCurrentBeadSpy) fn(_ context.Context, _ string, _ []string, _ string, sessionBeadID, workBeadID string) error {
	s.calls++
	s.sessionBeadID = sessionBeadID
	s.workBeadID = workBeadID
	return s.err
}

func noopRecordCurrentBead(context.Context, string, []string, string, string, string) error {
	return nil
}

func (s *publishRunMapSpy) fn(runID, beadID string, keys ...string) error {
	s.calls++
	s.runID = runID
	s.beadID = beadID
	s.keys = append([]string(nil), keys...)
	return s.err
}

func claimOpsForRunMap(beadID string, claimedMeta map[string]string, spy *publishRunMapSpy) (hookClaimOps, hookClaimOptions) {
	ops := hookClaimOps{
		Runner: func(string, string) (string, error) {
			return `[{"id":"` + beadID + `","status":"open","metadata":{"gc.routed_to":"worker"}}]`, nil
		},
		Claim: func(_ context.Context, _ string, _ []string, id, assignee string) (beads.Bead, bool, error) {
			return beads.Bead{ID: id, Status: "in_progress", Assignee: assignee, Metadata: claimedMeta}, true, nil
		},
		ResolveWorkBranch: func(string) string { return "" },
		StampWorkMeta:     noopStampWorkMeta,
		PublishRunMap:     spy.fn,
		RecordCurrentBead: noopRecordCurrentBead,
	}
	opts := hookClaimOptions{
		Assignee:           "worker-1",
		IdentityCandidates: []string{"worker-1"},
		RouteTargets:       []string{"worker"},
		Env: []string{
			"GC_SESSION_NAME=worker-1",
			"GC_SESSION_ID=session-1",
			"BEADS_ACTOR=actor-1",
		},
		JSON: true,
	}
	return ops, opts
}

// TestDoHookClaimRefusesReceiptWhenSessionPointerWriteFails pins the coherent
// receipt boundary. Once a work claim commits, a missing or stale durable
// session row must withhold the receipt so the worker cannot close a drain step
// while currently_processing_bead_id still points at prior work.
func TestDoHookClaimRefusesReceiptWhenSessionPointerWriteFails(t *testing.T) {
	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("hw-safe", map[string]string{
		"gc.routed_to":    "worker",
		"gc.root_bead_id": "root-safe",
	}, spy)
	pointerSpy := &recordCurrentBeadSpy{err: errors.New("exact session bead disappeared")}
	ops.RecordCurrentBead = pointerSpy.fn

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 1 {
		t.Fatalf("doHookClaim = %d, want 1; stderr=%s", code, stderr.String())
	}
	if pointerSpy.calls != 1 || pointerSpy.sessionBeadID != "session-1" || pointerSpy.workBeadID != "hw-safe" {
		t.Fatalf("current-bead write = %+v, want one session-1/hw-safe attempt", pointerSpy)
	}
	if stdout.Len() != 0 {
		t.Fatalf("stdout = %q, want no exposed work receipt", stdout.String())
	}
	if spy.calls != 0 {
		t.Fatalf("run-map publish calls = %d, want 0 when session pointer cannot be written", spy.calls)
	}
	if !strings.Contains(stderr.String(), "session-1") {
		t.Fatalf("stderr = %q, want failed session-pointer diagnostic", stderr.String())
	}
}

// TestDoHookClaimRebindsDurableCurrentBeadBeforeReceipt reproduces a replacement
// worker claiming a drain step whose work metadata still names the prior
// session. The receipt is emitted only after the replacement session's durable
// currently_processing_bead_id has been rebound to that drain step.
func TestDoHookClaimRebindsDurableCurrentBeadBeforeReceipt(t *testing.T) {
	runMapSpy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("ep-drain", map[string]string{
		"gc.routed_to":    "worker",
		"gc.session_id":   "old-session",
		"gc.session_name": "old-worker",
	}, runMapSpy)
	pointerSpy := &recordCurrentBeadSpy{}
	ops.RecordCurrentBead = pointerSpy.fn

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if pointerSpy.calls != 1 || pointerSpy.sessionBeadID != "session-1" || pointerSpy.workBeadID != "ep-drain" {
		t.Fatalf("current-bead write = %+v, want one replacement-session binding to ep-drain", pointerSpy)
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Action != "work" || result.BeadID != "ep-drain" {
		t.Fatalf("claim result = %+v, want work receipt for ep-drain", result)
	}
}

func TestDoHookClaimRunMapUsesBeadIDWithoutRunChain(t *testing.T) {
	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("hw-standalone", map[string]string{
		"gc.routed_to": "worker",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 || spy.runID != "hw-standalone" {
		t.Fatalf("run-map publish = %+v, want standalone bead ID as run ID", spy)
	}
}

func TestDoHookClaimSkipsRunMapWithoutSessionID(t *testing.T) {
	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("hw-nosess", map[string]string{"gc.routed_to": "worker"}, spy)
	opts.Env = []string{"GC_SESSION_NAME=worker-1", "BEADS_ACTOR=actor-1"}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 0 {
		t.Fatalf("run-map calls = %d, want 0 without a session bead ID", spy.calls)
	}
}

func TestDoHookClaimRunMapFailureDoesNotFailClaim(t *testing.T) {
	spy := &publishRunMapSpy{err: errors.New("run-map unavailable")}
	ops, opts := claimOpsForRunMap("hw-err", map[string]string{
		"gc.routed_to":    "worker",
		"gc.root_bead_id": "root-err",
	}, spy)

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.BeadID != "hw-err" || result.Reason != "claimed" {
		t.Fatalf("claim result = %+v, want bead hw-err reason claimed", result)
	}
	if !strings.Contains(stderr.String(), "publishing run-map for session session-1") {
		t.Fatalf("stderr missing best-effort run-map diagnostic: %s", stderr.String())
	}
}

func TestDoHookClaimPublishesRunMapOnExistingAssignment(t *testing.T) {
	spy := &publishRunMapSpy{}
	ops, opts := claimOpsForRunMap("unused", nil, spy)
	ops.Runner = func(string, string) (string, error) {
		return `[{"id":"hw-existing","status":"in_progress","assignee":"worker-1","metadata":{"gc.routed_to":"worker","gc.root_bead_id":"root-existing"}}]`, nil
	}
	ops.Claim = func(context.Context, string, []string, string, string) (beads.Bead, bool, error) {
		t.Fatal("Claim must not run for an existing assignment")
		return beads.Bead{}, false, nil
	}

	var stdout, stderr bytes.Buffer
	if code := doHookClaim("bd ready --json", "/tmp/work", opts, ops, &stdout, &stderr); code != 0 {
		t.Fatalf("doHookClaim = %d, want 0; stderr=%s", code, stderr.String())
	}
	if spy.calls != 1 || spy.runID != "root-existing" || spy.beadID != "hw-existing" {
		t.Fatalf("run-map publish = %+v, want existing assignment mapping", spy)
	}
}
