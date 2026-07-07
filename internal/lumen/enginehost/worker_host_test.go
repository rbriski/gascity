package enginehost

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
)

// newFakeWorkerHost builds a WorkerHost over the real worker.Factory boundary
// backed by an in-memory bead store and the fake runtime provider. afterSpawn
// models the one-shot session's fate (the fake does not itself simulate process
// exit), so each arm observes a deterministic terminal phase on the first poll.
func newFakeWorkerHost(t *testing.T, afterSpawn func(fake *runtime.Fake, name string)) (*WorkerHost, *runtime.Fake) {
	t.Helper()
	fake := runtime.NewFake()
	host, err := NewWorkerHost(WorkerHostConfig{
		Store:      beads.NewMemStore(),
		Provider:   fake,
		Command:    "claude",
		PromptFlag: "-p",
	})
	if err != nil {
		t.Fatalf("NewWorkerHost: %v", err)
	}
	if afterSpawn != nil {
		host.afterSpawn = func(name string) { afterSpawn(fake, name) }
	}
	return host, fake
}

// pendingErrProvider embeds the fake provider but forces Pending to error, so a
// live session's worker.State read fails — the deterministic way to exercise
// WorkerHost's State-error arm without breaking session creation.
type pendingErrProvider struct {
	*runtime.Fake
}

func (pendingErrProvider) Pending(string) (*runtime.PendingInteraction, error) {
	return nil, errors.New("pending boom")
}

func TestWorkerHostCleanStopIsPassAndCarriesOneShotPrompt(t *testing.T) {
	// A one-shot that exits (fake session stopped) reads pass.
	host, fake := newFakeWorkerHost(t, func(f *runtime.Fake, name string) {
		f.SetPeekOutput(name, "summary line\n")
		_ = f.Stop(name)
	})

	req := DoRequest{RunID: "run1", NodeID: "summarize", Prompt: "Summarize the repo.", IdemToken: "tok-1"}
	res, err := host.RunDo(context.Background(), req)
	if err != nil {
		t.Fatalf("RunDo: %v", err)
	}
	if res.Outcome != OutcomePass {
		t.Fatalf("Outcome = %q (detail %q), want pass", res.Outcome, res.Detail)
	}
	if res.Output != "summary line" {
		t.Errorf("Output = %q, want harvested peek tail", res.Output)
	}

	name := DoSessionName("run1", "summarize")
	cfg := fake.LastStartConfig(name)
	if cfg == nil {
		t.Fatalf("no Start recorded for session %q", name)
	}
	if cfg.PromptSuffix != "Summarize the repo." {
		t.Errorf("PromptSuffix = %q, want the rendered prompt", cfg.PromptSuffix)
	}
	if cfg.PromptFlag != "-p" {
		t.Errorf("PromptFlag = %q, want -p", cfg.PromptFlag)
	}
	if cfg.Lifecycle != runtime.LifecycleOneShot {
		t.Errorf("Lifecycle = %q, want one_shot", cfg.Lifecycle)
	}
	if cfg.Command != "claude" {
		t.Errorf("Command = %q, want claude", cfg.Command)
	}
}

func TestWorkerHostSpawnFailureIsFailed(t *testing.T) {
	host, fake := newFakeWorkerHost(t, nil)
	name := DoSessionName("run1", "summarize")
	fake.StartErrors[name] = errors.New("provider boom")

	res, err := host.RunDo(context.Background(), DoRequest{RunID: "run1", NodeID: "summarize", Prompt: "x"})
	if err != nil {
		t.Fatalf("RunDo returned an internal error, want a failed outcome: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "spawn failed") {
		t.Errorf("Detail = %q, want it to mention spawn failure", res.Detail)
	}
}

func TestWorkerHostBlockedInteractionIsFailed(t *testing.T) {
	host, _ := newFakeWorkerHost(t, func(f *runtime.Fake, name string) {
		f.SetPendingInteraction(name, &runtime.PendingInteraction{Kind: "question", Prompt: "ok?"})
	})

	res, err := host.RunDo(context.Background(), DoRequest{RunID: "run1", NodeID: "summarize", Prompt: "x"})
	if err != nil {
		t.Fatalf("RunDo: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "interaction_required") {
		t.Errorf("Detail = %q, want interaction_required", res.Detail)
	}
}

func TestWorkerHostContextCancelIsFailedAndKills(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	// Cancel right after spawn (before any terminal transition) so the first
	// poll observes the cancellation.
	host, fake := newFakeWorkerHost(t, func(_ *runtime.Fake, _ string) { cancel() })

	res, err := host.RunDo(ctx, DoRequest{RunID: "run1", NodeID: "summarize", Prompt: "x"})
	if err != nil {
		t.Fatalf("RunDo: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "canceled") {
		t.Errorf("Detail = %q, want canceled", res.Detail)
	}
	name := DoSessionName("run1", "summarize")
	if fake.CountCalls("Stop", name) == 0 {
		t.Errorf("expected a Stop (kill) call for %q after cancellation", name)
	}
}

// TestWorkerHostTimeoutIsFailedAndKills covers the deadline arm: a session that
// never reaches a terminal phase with an already-elapsed timeout settles failed
// with a timeout detail and is killed (no leaked live session).
func TestWorkerHostTimeoutIsFailedAndKills(t *testing.T) {
	host, fake := newFakeWorkerHost(t, nil) // no afterSpawn: session stays live
	name := DoSessionName("run1", "summarize")

	res, err := host.RunDo(context.Background(), DoRequest{
		RunID: "run1", NodeID: "summarize", Prompt: "x", Timeout: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("RunDo: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "timeout") {
		t.Errorf("Detail = %q, want a timeout reason", res.Detail)
	}
	if fake.CountCalls("Stop", name) == 0 {
		t.Errorf("expected a Stop (kill) call for %q on timeout", name)
	}
	if fake.IsRunning(name) {
		t.Errorf("timeout path leaked a live session %q", name)
	}
}

// TestWorkerHostStateErrorIsFailed covers the State-error arm: a session whose
// lifecycle read errors settles failed with the read-state detail (never a
// non-nil error out of RunDo).
func TestWorkerHostStateErrorIsFailed(t *testing.T) {
	fake := runtime.NewFake()
	host, err := NewWorkerHost(WorkerHostConfig{
		Store:    beads.NewMemStore(),
		Provider: pendingErrProvider{Fake: fake},
		Command:  "claude",
	})
	if err != nil {
		t.Fatalf("NewWorkerHost: %v", err)
	}

	res, err := host.RunDo(context.Background(), DoRequest{RunID: "run1", NodeID: "summarize", Prompt: "x"})
	if err != nil {
		t.Fatalf("RunDo returned an internal error, want a failed outcome: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "reading session state") {
		t.Errorf("Detail = %q, want the read-state failure", res.Detail)
	}
}

// TestWorkerHostClosesSessionNoLeak asserts the one-shot session is torn down on
// BOTH the pass and an error path — CloseDetailed always runs, so no live agent
// session leaks past a do step.
func TestWorkerHostClosesSessionNoLeak(t *testing.T) {
	// Pass path: afterSpawn stops the session (→ pass); the cleanup CloseDetailed
	// must ALSO stop it — a second Stop is the no-leak proof beyond afterSpawn's.
	passName := DoSessionName("pass", "n")
	passHost, passFake := newFakeWorkerHost(t, func(f *runtime.Fake, name string) { _ = f.Stop(name) })
	passRes, err := passHost.RunDo(context.Background(), DoRequest{RunID: "pass", NodeID: "n", Prompt: "x"})
	if err != nil {
		t.Fatalf("RunDo pass: %v", err)
	}
	if passRes.Outcome != OutcomePass {
		t.Fatalf("pass Outcome = %q, want pass", passRes.Outcome)
	}
	if passFake.IsRunning(passName) {
		t.Errorf("pass path leaked a live session %q", passName)
	}
	if got := passFake.CountCalls("Stop", passName); got < 2 {
		t.Errorf("pass Stop calls = %d, want >=2 (afterSpawn + cleanup CloseDetailed)", got)
	}

	// Error path (timeout): kill + cleanup must leave no live session either.
	errName := DoSessionName("err", "n")
	errHost, errFake := newFakeWorkerHost(t, nil)
	errRes, err := errHost.RunDo(context.Background(), DoRequest{RunID: "err", NodeID: "n", Prompt: "x", Timeout: time.Nanosecond})
	if err != nil {
		t.Fatalf("RunDo err: %v", err)
	}
	if errRes.Outcome != OutcomeFailed {
		t.Fatalf("err Outcome = %q, want failed", errRes.Outcome)
	}
	if errFake.IsRunning(errName) {
		t.Errorf("error path leaked a live session %q", errName)
	}
}

// TestWorkerHostSurfacesCloseFailure proves a terminate failure during cleanup
// (Manager.CloseDetailed's "closed but still running" wedge) is surfaced on the
// result Detail rather than swallowed — a live orphan agent must not be silent.
func TestWorkerHostSurfacesCloseFailure(t *testing.T) {
	host, fake := newFakeWorkerHost(t, nil) // session stays live
	name := DoSessionName("run1", "summarize")
	fake.StopErrors[name] = errors.New("terminate wedged")

	res, err := host.RunDo(context.Background(), DoRequest{
		RunID: "run1", NodeID: "summarize", Prompt: "x", Timeout: time.Nanosecond,
	})
	if err != nil {
		t.Fatalf("RunDo: %v", err)
	}
	if res.Outcome != OutcomeFailed {
		t.Fatalf("Outcome = %q, want failed", res.Outcome)
	}
	if !strings.Contains(res.Detail, "timeout") {
		t.Errorf("Detail = %q, want the timeout reason preserved", res.Detail)
	}
	if !strings.Contains(res.Detail, "session close failed") {
		t.Errorf("Detail = %q, want the close failure surfaced", res.Detail)
	}
}
