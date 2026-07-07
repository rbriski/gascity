// Package enginehost is the agent-`do` bridge for the Lumen executor: the
// narrow seam through which a Lumen `do` step becomes a real agent session.
//
// The executor (internal/lumen/engine) stays pure and byte-deterministic above
// this line — its reducer folds a journal, its plan is a topo sort. An agent
// step, by contrast, spawns a session, waits for it, and reports an outcome
// that depends on a model. That is where determinism honestly stops. This
// package draws that line as a single interface, [AgentHost], with two
// implementations:
//
//   - [StubHost]: scripted and deterministic (a map from step id to result).
//     It drives goldens, the engine's unit tests, and the CLI-path tests. No
//     I/O, no processes.
//   - [WorkerHost]: the real bridge. It maps one [DoRequest] onto one one-shot
//     session through the canonical worker boundary (internal/worker.Factory),
//     polls the session to a terminal lifecycle phase, and harvests its output.
//
// Completion is observed by lifecycle PHASE, not by an exit code. The session
// boundary surfaces "stopped / failed / blocked", not "the agent's turn
// finished with status N", and the default subprocess provider exposes no exit
// code — so at this seam ANY process exit, clean OR crashed (e.g. an agent that
// dies on an expired key), reaches a stopped phase and reads pass. A failed
// outcome is produced only by a spawn failure, a blocked interaction, a
// timeout, or a cancellation — never by a non-zero agent exit. Reliable
// pass/fail adjudication of the agent's WORK therefore requires the agent to
// self-report its own outcome (the Tier-B gc.outcome path, P4.5). That honest
// limit is documented on [WorkerHost] and [DoResult].
//
// ZERO hardcoded roles: the prompt in a [DoRequest] is user-supplied IR. No
// role name appears here; the prompt rides the generic "pass a prompt to the
// agent CLI" channel (runtime.Config.PromptSuffix/PromptFlag).
package enginehost

import (
	"context"
	"time"
)

// Outcome vocabulary for a do step. These string values mirror the executor's
// outcome names exactly, so the engine folds a DoResult.Outcome straight into a
// node settlement without translation. The package deliberately does NOT import
// internal/lumen/engine (the engine imports this package), so the constants are
// duplicated as the stable contract between the two.
const (
	// OutcomePass reports the agent step completed cleanly.
	OutcomePass = "pass"
	// OutcomeFailed reports the agent step did not complete cleanly (spawn
	// failure, non-clean stop, blocked on interaction, or timeout).
	OutcomeFailed = "failed"
	// OutcomeDegraded reports a partial success a host may choose to surface.
	OutcomeDegraded = "degraded"
)

// DoRequest is one rendered agent step handed to a host. It carries the
// identity the engine uses to key the effect in the journal (so a host may
// stamp the idem token on the session for provenance) and the fully rendered
// prompt — interpolation happens in the engine, above this seam.
type DoRequest struct {
	// RunID is the run's journal stream id, used to name the session.
	RunID string
	// NodeID is the do node's IR id, used to name the session.
	NodeID string
	// Activation is the settling activation key the engine journals.
	Activation string
	// IdemToken is the effect token the engine journaled in effect.scheduled.
	IdemToken string
	// Prompt is the rendered body the agent is asked to run.
	Prompt string
	// AgentRef is the optional interpreter.agent binding name; "" = default.
	AgentRef string
	// WorkDir is the working directory for the agent session; "" = host default.
	WorkDir string
	// Timeout bounds a single do step; 0 = the host's configured default.
	Timeout time.Duration
}

// DoResult is a host's report for one [DoRequest]. Outcome is always one of the
// Outcome* constants; a host never returns an unmapped outcome.
type DoResult struct {
	// Outcome is pass, failed, or degraded. HONEST LIMIT at the phase-based
	// worker boundary: any process exit (clean OR crashed) reads pass — the
	// boundary carries no exit code and never yields PhaseFailed. Only a spawn
	// failure, a blocked interaction, a timeout, or a cancellation reads failed.
	// True pass/fail of the agent's work needs Tier-B gc.outcome (P4.5).
	Outcome string
	// Output is the captured agent output tail, for scope/{{ref}} downstream.
	// Capture is PROVIDER-DEPENDENT: the default subprocess provider's Peek
	// returns nothing, so Output is empty in production (the pipeline is proven,
	// not the capture); a provider whose Peek returns scrollback (tmux/herdr)
	// delivers a real tail. {{ref}} chaining therefore interpolates "" under the
	// subprocess default.
	Output string
	// SessionRef is the session name/id for provenance (effect.settled.session).
	SessionRef string
	// Detail is a human-readable reason on a non-pass outcome.
	Detail string
}

// AgentHost runs a single agent `do` step synchronously and reports its
// outcome. RunDo returns a non-nil error only for an internal/misconfiguration
// failure the host could not turn into an outcome (e.g. a nil session handle);
// an operational failure the host CAN observe — a spawn error, a failed phase,
// a timeout — is reported as a DoResult with Outcome == OutcomeFailed and a
// non-nil error is NOT returned. The engine folds either shape into a settled
// effect, so a scheduled effect always gets a settled record.
type AgentHost interface {
	RunDo(ctx context.Context, req DoRequest) (DoResult, error)
}
