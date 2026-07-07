package enginehost

import (
	"context"
	"errors"
	"testing"
)

func TestStubHostReturnsScriptedResultAndRecordsCall(t *testing.T) {
	h := &StubHost{Results: map[string]DoResult{
		"summarize": {Outcome: OutcomePass, Output: "three bullets"},
	}}

	req := DoRequest{RunID: "run1", NodeID: "summarize", Activation: "summarize:0", Prompt: "Summarize."}
	res, err := h.RunDo(context.Background(), req)
	if err != nil {
		t.Fatalf("RunDo: %v", err)
	}
	if res.Outcome != OutcomePass {
		t.Errorf("Outcome = %q, want %q", res.Outcome, OutcomePass)
	}
	if res.Output != "three bullets" {
		t.Errorf("Output = %q, want %q", res.Output, "three bullets")
	}
	if res.SessionRef != "stub:summarize" {
		t.Errorf("SessionRef = %q, want a defaulted stub ref", res.SessionRef)
	}

	calls := h.Calls()
	if len(calls) != 1 || calls[0].NodeID != "summarize" || calls[0].Prompt != "Summarize." {
		t.Errorf("Calls() = %+v, want one call recording the request", calls)
	}
}

func TestStubHostScriptedError(t *testing.T) {
	wantErr := context.DeadlineExceeded
	h := &StubHost{Errs: map[string]error{"boom": wantErr}}

	_, err := h.RunDo(context.Background(), DoRequest{NodeID: "boom"})
	if !errors.Is(err, wantErr) {
		t.Fatalf("RunDo err = %v, want %v", err, wantErr)
	}
}

func TestStubHostUnscriptedNodeIsWiringError(t *testing.T) {
	h := &StubHost{}
	_, err := h.RunDo(context.Background(), DoRequest{NodeID: "missing"})
	if err == nil {
		t.Fatal("expected an error for an unscripted do node, got nil")
	}
}
