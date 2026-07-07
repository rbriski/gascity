package enginehost

import (
	"context"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/agent"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/worker"
)

// Default poll/timeout knobs for [WorkerHost]. A do step is bounded work, so
// the defaults are generous headroom, not a policy.
const (
	defaultPollInterval = 25 * time.Millisecond
	defaultMaxWait      = 5 * time.Minute
)

// WorkerHostConfig configures a [WorkerHost]. Store and Provider are required.
type WorkerHostConfig struct {
	// Store backs the throwaway session ledger for spawned do sessions.
	Store beads.Store
	// Provider is the runtime provider sessions run under (subprocess, exec,
	// fake, …). GC_SESSION=fake selects the fake provider at the CLI edge.
	Provider runtime.Provider
	// CityPath is the city root for session state; "" for standalone runs.
	CityPath string
	// ProviderName is the cosmetic provider label stamped on the session bead.
	ProviderName string
	// Command is the agent CLI to launch, e.g. "claude".
	Command string
	// PromptFlag is the CLI flag the prompt rides, e.g. "-p"; "" appends the
	// prompt as a bare positional argument.
	PromptFlag string
	// WorkDir is the default working directory for do sessions; a per-request
	// WorkDir overrides it.
	WorkDir string
	// MaxWait bounds a single do step's wait for a terminal phase. 0 = default.
	MaxWait time.Duration
	// PollInterval is the lifecycle poll cadence. 0 = default.
	PollInterval time.Duration
}

// WorkerHost is the real [AgentHost]: it maps one [DoRequest] onto one one-shot
// agent session through the canonical worker boundary (internal/worker.Factory,
// which constructs the session.Manager internally so no caller touches it), and
// observes completion by lifecycle PHASE.
//
// Honest completion limit: the session boundary exposes a lifecycle PHASE, not
// a turn-done/exit-code primitive, and the default subprocess provider has no
// exit code to surface. So ANY process exit — clean OR crashed (e.g. an agent
// that dies on an expired key) — reaches a stopped phase and reads pass. A
// failed outcome comes only from a spawn failure, a blocked interaction, a
// timeout, or a cancellation, never from a non-zero agent exit. A clean stop
// after bad work therefore reads pass; sharper pass/fail needs the agent's
// self-reported Tier-B gc.outcome (P4.5, deferred). Determinism:
// outcome-determinism only; the byte-deterministic guarantees live above this
// seam, under StubHost.
type WorkerHost struct {
	factory      *worker.Factory
	providerName string
	command      string
	promptFlag   string
	workDir      string
	maxWait      time.Duration
	pollInterval time.Duration

	// afterSpawn, when non-nil, runs once immediately after a session is
	// created and before the lifecycle poll loop begins. It is a test seam
	// used to model a provider that does not itself simulate process exit
	// (the fake): a white-box test stops or blocks the fake session here so
	// the first poll observes a terminal phase deterministically. Nil in
	// production.
	afterSpawn func(sessionName string)
}

var _ AgentHost = (*WorkerHost)(nil)

// NewWorkerHost builds a WorkerHost over a worker.Factory constructed from cfg.
// The factory owns session.Manager construction, keeping every caller (and the
// cmd/gc worker-boundary invariant) off the direct session-manager path.
func NewWorkerHost(cfg WorkerHostConfig) (*WorkerHost, error) {
	if cfg.Store == nil {
		return nil, fmt.Errorf("enginehost: WorkerHost requires a bead store")
	}
	if cfg.Provider == nil {
		return nil, fmt.Errorf("enginehost: WorkerHost requires a runtime provider")
	}
	if strings.TrimSpace(cfg.Command) == "" {
		return nil, fmt.Errorf("enginehost: WorkerHost requires an agent command")
	}
	factory, err := worker.NewFactory(worker.FactoryConfig{
		Store:    cfg.Store,
		Provider: cfg.Provider,
		CityPath: cfg.CityPath,
	})
	if err != nil {
		return nil, fmt.Errorf("enginehost: building worker factory: %w", err)
	}
	poll := cfg.PollInterval
	if poll <= 0 {
		poll = defaultPollInterval
	}
	maxWait := cfg.MaxWait
	if maxWait <= 0 {
		maxWait = defaultMaxWait
	}
	return &WorkerHost{
		factory:      factory,
		providerName: cfg.ProviderName,
		command:      cfg.Command,
		promptFlag:   cfg.PromptFlag,
		workDir:      cfg.WorkDir,
		maxWait:      maxWait,
		pollInterval: poll,
	}, nil
}

// DoSessionName is the deterministic session name for a do step, so callers and
// tests can address the same session the host spawns.
func DoSessionName(runID, nodeID string) string {
	return agent.SessionNameFor("", "lumen/"+runID+"-"+nodeID, "")
}

// RunDo spawns a one-shot agent session for req, polls it to a terminal phase,
// harvests its output, and reports the outcome. See [WorkerHost] for the
// phase-based completion contract.
func (h *WorkerHost) RunDo(ctx context.Context, req DoRequest) (DoResult, error) {
	sessionName := DoSessionName(req.RunID, req.NodeID)
	workDir := req.WorkDir
	if workDir == "" {
		workDir = h.workDir
	}
	if workDir == "" {
		workDir = "."
	}
	providerLabel := h.providerName
	if providerLabel == "" {
		providerLabel = "lumen-agent"
	}
	spec := worker.SessionSpec{
		ExplicitName: sessionName,
		Template:     "lumen-do",
		Title:        "lumen do " + req.NodeID,
		Command:      h.command,
		WorkDir:      workDir,
		Provider:     providerLabel,
		Hints: runtime.Config{
			PromptFlag:   h.promptFlag,
			PromptSuffix: req.Prompt,
			Lifecycle:    runtime.LifecycleOneShot,
		},
		Metadata: map[string]string{"lumen_idem_token": req.IdemToken},
	}

	handle, err := h.factory.Session(spec)
	if err != nil {
		return DoResult{}, fmt.Errorf("enginehost: building session handle: %w", err)
	}

	if _, err := handle.Create(ctx, worker.CreateModeStarted); err != nil {
		// The agent could not be spawned — an observed operational failure, not
		// an internal error. Report it as a failed outcome.
		return DoResult{
			Outcome:    OutcomeFailed,
			SessionRef: sessionName,
			Detail:     "spawn failed: " + err.Error(),
		}, nil
	}

	if h.afterSpawn != nil {
		h.afterSpawn(sessionName)
	}

	timeout := req.Timeout
	if timeout <= 0 {
		timeout = h.maxWait
	}
	res := h.waitTerminal(ctx, handle, sessionName, timeout)

	// Cleanup is best-effort and must survive a canceled request context, so
	// the one-shot's session bead always closes. A genuine terminate failure is
	// NOT swallowed: Manager.CloseDetailed propagates a "closed but still
	// running" wedge, which here could leave a live orphan agent — the throwaway
	// MemStore ledger has no persistent session to adopt it from later — so
	// surface it on the result Detail and to stderr rather than drop it.
	cleanupCtx := context.WithoutCancel(ctx)
	if _, closeErr := handle.CloseDetailed(cleanupCtx); closeErr != nil {
		note := "session close failed (possible orphan agent): " + closeErr.Error()
		log.Printf("enginehost: do session %s: %s", sessionName, note)
		if res.Detail == "" {
			res.Detail = note
		} else {
			res.Detail += "; " + note
		}
	}
	return res, nil
}

// waitTerminal polls the session lifecycle until it reaches a terminal phase,
// the request times out, or the context is canceled, and maps the observation
// onto a DoResult.
func (h *WorkerHost) waitTerminal(ctx context.Context, handle worker.Handle, sessionName string, timeout time.Duration) DoResult {
	deadline := time.Now().Add(timeout)
	timer := time.NewTimer(h.pollInterval)
	defer timer.Stop()

	for {
		st, err := handle.State(ctx)
		if err != nil {
			return DoResult{Outcome: OutcomeFailed, SessionRef: sessionName, Detail: "reading session state: " + err.Error()}
		}
		ref := st.SessionName
		if ref == "" {
			ref = sessionName
		}
		switch st.Phase {
		case worker.PhaseStopped:
			return DoResult{Outcome: OutcomePass, SessionRef: ref, Output: h.harvest(ctx, handle)}
		case worker.PhaseFailed:
			// Currently UNREACHABLE: the phase-based worker boundary never yields
			// PhaseFailed — worker.State maps every terminal provider state to
			// PhaseStopped — so a crashed agent settles as PhaseStopped/pass above.
			// This arm is kept for forward-compatibility: if a provider ever
			// surfaces a genuine terminal-failure phase, it folds straight to a
			// failed outcome without a code change here.
			detail := st.Detail
			if detail == "" {
				detail = "session reached a failed phase"
			}
			return DoResult{Outcome: OutcomeFailed, SessionRef: ref, Output: h.harvest(ctx, handle), Detail: detail}
		case worker.PhaseBlocked:
			h.kill(handle)
			return DoResult{Outcome: OutcomeFailed, SessionRef: ref, Detail: "interaction_required: one-shot do step blocked on an interaction"}
		default:
			// Non-terminal (starting/ready/busy/stopping/unknown): keep polling.
		}

		if ctxErr := ctx.Err(); ctxErr != nil {
			h.kill(handle)
			return DoResult{Outcome: OutcomeFailed, SessionRef: ref, Detail: "canceled: " + ctxErr.Error()}
		}
		if !time.Now().Before(deadline) {
			h.kill(handle)
			return DoResult{Outcome: OutcomeFailed, SessionRef: ref, Detail: fmt.Sprintf("timeout: do step exceeded %s", timeout)}
		}

		timer.Reset(h.pollInterval)
		select {
		case <-ctx.Done():
			h.kill(handle)
			return DoResult{Outcome: OutcomeFailed, SessionRef: ref, Detail: "canceled: " + ctx.Err().Error()}
		case <-timer.C:
		}
	}
}

// harvest reads a best-effort output tail from the session. Capture is
// provider-dependent: the default subprocess provider's Peek returns "", so the
// tail is empty in production (the {{ref}} pipeline is proven, not the capture);
// a provider whose Peek returns scrollback (tmux/herdr) delivers a real tail.
// Peek failures are non-fatal: an empty tail is an acceptable output.
func (h *WorkerHost) harvest(ctx context.Context, handle worker.Handle) string {
	out, err := handle.Peek(ctx, 0)
	if err != nil {
		return ""
	}
	return strings.TrimRight(out, "\n")
}

// kill best-effort terminates the live runtime without waiting on the request
// context (which may already be canceled).
func (h *WorkerHost) kill(handle worker.Handle) {
	_ = handle.Kill(context.Background())
}
