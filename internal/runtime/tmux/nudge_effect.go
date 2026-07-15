package tmux

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/shellquote"
)

const (
	tmuxNudgeEffectEnteredMarker   = "GC_NUDGE_ENTERED"
	tmuxNudgeEffectSubmittedMarker = "GC_NUDGE_SUBMITTED"
	tmuxNudgeEffectRefusedMarker   = "GC_NUDGE_REFUSED"
)

// NudgeEffect conditionally injects one durable nudge at the tmux server's
// native command queue. The first send and its launch/attachment/copy-mode
// predicate are one if-shell command, so another tmux client cannot interleave
// between the final safety check and native entry.
func (p *Provider) NudgeEffect(ctx context.Context, name string, request runtime.NudgeEffectRequest) (runtime.NudgeEffectResult, error) {
	if ctx == nil {
		return tmuxNudgeNotEntered(), fmt.Errorf("%w: context is nil", runtime.ErrNudgeEffectInvalid)
	}
	if err := ctx.Err(); err != nil {
		return tmuxNudgeNotEntered(), err
	}
	if err := request.Contract.Validate(); err != nil {
		return tmuxNudgeNotEntered(), err
	}
	message := runtime.FlattenText(request.Content)
	if message == "" {
		return tmuxNudgeNotEntered(), fmt.Errorf("%w: content is empty", runtime.ErrNudgeEffectInvalid)
	}
	if err := validateSessionName(name); err != nil {
		return tmuxNudgeNotEntered(), err
	}
	if strings.ContainsAny(request.Contract.ExpectedLaunchIdentity, ",{}") {
		return tmuxNudgeNotEntered(), fmt.Errorf("%w: launch identity cannot be embedded in a tmux predicate", runtime.ErrNudgeEffectInvalid)
	}

	if !acquireNudgeLock(name, p.tm.cfg.NudgeLockTimeout) {
		return tmuxNudgeNotEntered(), fmt.Errorf("nudge lock timeout for session %q", name)
	}
	defer releaseNudgeLock(name)
	if err := ctx.Err(); err != nil {
		return tmuxNudgeNotEntered(), err
	}

	target := name
	if pane, err := p.tm.FindAgentPane(name); err != nil {
		return tmuxNudgeNotEntered(), err
	} else if pane != "" {
		target = pane
	}
	observation, err := p.tm.observeNudgeEffectTarget(ctx, target)
	if err != nil {
		return tmuxNudgeNotEntered(), err
	}
	if err := classifyTmuxNudgeObservation(observation, request.Contract.ExpectedLaunchIdentity); err != nil {
		return tmuxNudgeNotEntered(), err
	}
	priorActivity, _ := p.tm.GetSessionActivity(name)
	pokeAt := time.Now()

	entry, err := p.tm.runConditionalNudgeInput(ctx, target, request.Contract.ExpectedLaunchIdentity, message, true, tmuxNudgeEffectEnteredMarker)
	if err != nil {
		return tmuxNudgeAmbiguous(err)
	}
	if entry != tmuxNudgeEffectEnteredMarker {
		return tmuxNudgeNotEntered(), classifyUnexpectedTmuxNudgeRefusal(entry, request.Contract.ExpectedLaunchIdentity)
	}
	p.tm.recordPokeEvidence(name, priorActivity, pokeAt)

	if delay := time.Duration(p.tm.cfg.DebounceMs) * time.Millisecond; delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return tmuxNudgeAmbiguous(ctx.Err())
		}
	}

	submitted, err := p.tm.runConditionalNudgeInput(ctx, target, request.Contract.ExpectedLaunchIdentity, "Enter", false, tmuxNudgeEffectSubmittedMarker)
	if err != nil {
		return tmuxNudgeAmbiguous(err)
	}
	if submitted != tmuxNudgeEffectSubmittedMarker {
		refusalErr := classifyUnexpectedTmuxNudgeRefusal(submitted, request.Contract.ExpectedLaunchIdentity)
		return tmuxNudgeAmbiguous(refusalErr)
	}
	return runtime.NudgeEffectResult{
		Stage:      runtime.NudgeEffectStageAccepted,
		Completion: runtime.NudgeEffectCompletionCompleted,
	}, nil
}

func (t *Tmux) runConditionalNudgeInput(ctx context.Context, target, expectedLaunch, input string, literal bool, successMarker string) (string, error) {
	condition := fmt.Sprintf(
		"#{&&:#{==:#{E:GC_INSTANCE_TOKEN},%s},#{==:#{session_attached},0},#{==:#{pane_in_mode},0}}",
		expectedLaunch,
	)
	inputArgs := []string{"send-keys", "-t", target}
	if literal {
		inputArgs = append(inputArgs, "-l")
	}
	inputArgs = append(inputArgs, input)
	success := shellquote.Join(inputArgs) + " ; " + shellquote.Join([]string{"display-message", "-t", target, "-p", successMarker})
	refused := shellquote.Join([]string{"display-message", "-t", target, tmuxNudgeEffectRefusedMarker})
	return t.runCtx(ctx, "if-shell", "-t", target, "-F", condition, success, refused)
}

func (t *Tmux) observeNudgeEffectTarget(ctx context.Context, target string) (string, error) {
	return t.runCtx(ctx,
		"display-message", "-t", target, "-p",
		tmuxNudgeEffectRefusedMarker+"|#{session_attached}|#{pane_in_mode}|#{E:GC_INSTANCE_TOKEN}",
	)
}

func classifyTmuxNudgeObservation(observation, expectedLaunch string) error {
	parts := strings.SplitN(strings.TrimSpace(observation), "|", 4)
	if len(parts) != 4 || parts[0] != tmuxNudgeEffectRefusedMarker {
		return fmt.Errorf("%w: tmux interaction evidence is incomplete", runtime.ErrNudgeEffectInvalid)
	}
	if strings.TrimSpace(parts[3]) != expectedLaunch {
		return runtime.ErrNudgeTargetChanged
	}
	attached, err := strconv.ParseUint(strings.TrimSpace(parts[1]), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: tmux attachment evidence is invalid", runtime.ErrNudgeEffectInvalid)
	}
	if attached > 0 {
		return runtime.ErrNudgeHumanAttached
	}
	inMode, err := strconv.ParseUint(strings.TrimSpace(parts[2]), 10, 64)
	if err != nil {
		return fmt.Errorf("%w: tmux copy-mode evidence is invalid", runtime.ErrNudgeEffectInvalid)
	}
	if inMode > 0 {
		return runtime.ErrNudgeCopyMode
	}
	return nil
}

func classifyUnexpectedTmuxNudgeRefusal(observation, expectedLaunch string) error {
	if err := classifyTmuxNudgeObservation(observation, expectedLaunch); err != nil {
		return err
	}
	return fmt.Errorf("%w: tmux refused native input without a classified conflict", runtime.ErrNudgeEffectInvalid)
}

func (t *Tmux) recordPokeEvidence(session string, prior, at time.Time) {
	t.pokeMu.Lock()
	defer t.pokeMu.Unlock()
	if t.pokes == nil {
		t.pokes = make(map[string]pokeInfo)
	}
	t.pokes[session] = pokeInfo{at: at, prior: prior}
}

func tmuxNudgeNotEntered() runtime.NudgeEffectResult {
	return runtime.NudgeEffectResult{
		Stage:      runtime.NudgeEffectStageNotEntered,
		Completion: runtime.NudgeEffectCompletionNotCompleted,
	}
}

func tmuxNudgeAmbiguous(err error) (runtime.NudgeEffectResult, error) {
	return runtime.NudgeEffectResult{
		Stage:      runtime.NudgeEffectStageMayHaveEntered,
		Completion: runtime.NudgeEffectCompletionUnknown,
	}, errors.Join(runtime.ErrNudgeDeliveryUnknown, err)
}

var _ runtime.NudgeEffectProvider = (*Provider)(nil)
