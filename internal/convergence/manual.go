package convergence

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
)

// ApproveHandler processes an operator's approval of a convergence loop
// that is in the waiting_manual state. It terminates the loop with
// terminal_reason=approved.
//
// Idempotent: if the bead is already terminated with reason=approved,
// returns a no-op result without error.
//
// Write ordering contract: last_processed_wisp is written LAST (dedup marker).
func (h *Handler) ApproveHandler(_ context.Context, beadID, username, _ string) (HandlerResult, error) {
	meta, err := h.Store.GetMetadata(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading bead %q metadata: %w", beadID, err)
	}

	state := meta[FieldState]
	actor := "operator:" + username

	// Idempotent: already terminated+approved is a no-op.
	if state == StateTerminated && meta[FieldTerminalReason] == TerminalApproved {
		return HandlerResult{
			Action: ActionApproved,
		}, nil
	}

	// Must be in waiting_manual state.
	if state != StateWaitingManual {
		return HandlerResult{}, fmt.Errorf(
			"cannot approve bead %q: state is %q, expected %q",
			beadID, state, StateWaitingManual,
		)
	}

	// One checked child snapshot supplies the iteration count, duration, and
	// applicable terminal marker. In particular, waiting_manual may be durable
	// while its trailing last_processed_wisp write is still stale after a crash.
	children, err := h.Store.Children(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("listing children for terminal proof on bead %q: %w", beadID, err)
	}
	stats, err := childStats(children, beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("validating child evidence for terminal proof on bead %q: %w", beadID, err)
	}
	iterationCount := stats.ClosedCount

	// Read the last active wisp for event payload.
	activeWisp := meta[FieldActiveWisp]
	lastProcessedWisp := meta[FieldLastProcessedWisp]
	terminalMarker := lastProcessedWisp
	if stats.HighestClosedFound {
		terminalMarker = stats.HighestClosed.ID
	}
	// Use the most recent wisp reference for the event.
	eventWispID := terminalMarker
	if activeWisp != "" {
		eventWispID = activeWisp
	}

	// Prepare terminal payloads before commit, but publish them only after state,
	// close, and last_processed_wisp are all durable.
	termPayload := TerminatedPayload{
		TerminalReason:       TerminalApproved,
		TotalIterations:      iterationCount,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: stats.CumulativeDur.Milliseconds(),
	}
	approvePayload := ManualActionPayload{
		Actor:      actor,
		PriorState: StateWaitingManual,
		NewState:   StateTerminated,
		Iteration:  iterationCount,
		WispID:     NullableString(eventWispID),
	}

	// Write ordering: terminal_reason, terminal_actor, clear waiting_reason,
	// state=terminated, CloseBead, then last_processed_wisp LAST.
	if err := h.commit(beadID,
		[]metaWrite{
			{FieldTerminalReason, TerminalApproved, "setting terminal reason"},
			{FieldTerminalActor, actor, "setting terminal actor"},
			{FieldWaitingReason, "", "clearing waiting reason"},
			{FieldState, StateTerminated, "setting state to terminated"},
		},
		func() error {
			if err := h.Store.CloseBead(beadID, CloseReasonManualApprove); err != nil {
				return fmt.Errorf("closing bead %q: %w", beadID, err)
			}
			return nil
		},
		metaWrite{FieldLastProcessedWisp, terminalMarker, "setting last processed wisp"},
	); err != nil {
		return HandlerResult{}, err
	}

	h.emitEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)
	h.emitEvent(EventManualApprove, EventIDManualApprove(beadID), beadID, approvePayload)

	return HandlerResult{
		Action:    ActionApproved,
		Iteration: iterationCount,
	}, nil
}

// IterateHandler processes an operator's request to continue iterating a
// convergence loop that is in the waiting_manual state. It pours a new
// wisp and transitions the loop back to active state.
//
// Write ordering contract: last_processed_wisp is NOT written here because
// the new wisp hasn't been processed yet — it will be written when the
// new wisp closes.
func (h *Handler) IterateHandler(_ context.Context, beadID, username, _ string) (HandlerResult, error) {
	meta, err := h.Store.GetMetadata(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading bead %q metadata: %w", beadID, err)
	}

	state := meta[FieldState]

	// Must be in waiting_manual state.
	if state != StateWaitingManual {
		return HandlerResult{}, fmt.Errorf(
			"cannot iterate bead %q: state is %q, expected %q",
			beadID, state, StateWaitingManual,
		)
	}

	// Derive the next iteration from the durable marker. A closed successor may
	// already exist after an ambiguous prior pour; total closed-child count would
	// skip that successor and create duplicate work.
	children, err := h.Store.Children(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("listing children for bead %q: %w", beadID, err)
	}
	lastProcessedWisp := meta[FieldLastProcessedWisp]
	nextIteration, err := nextIterationAfterLastProcessed(beadID, lastProcessedWisp, children)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("deriving next iteration for bead %q: %w", beadID, err)
	}
	maxIterations, _ := DecodeInt(meta[FieldMaxIterations])
	if nextIteration > maxIterations {
		return HandlerResult{}, fmt.Errorf(
			"cannot iterate bead %q: at max iterations (%d/%d)",
			beadID, nextIteration-1, maxIterations,
		)
	}

	actor := "operator:" + username

	// Pour next wisp with idempotency key BEFORE any state mutations.
	// If PourWisp fails, the bead stays in waiting_manual (safe to retry).
	nextKey := IdempotencyKey(beadID, nextIteration)
	formula := meta[FieldFormula]
	vars := ExtractVars(meta)
	evaluatePrompt := meta[FieldEvaluatePrompt]

	nextWispID, err := h.Store.PourWisp(beadID, formula, nextKey, vars, evaluatePrompt)
	if err != nil {
		// Check if wisp was created despite the error.
		existingID, found, lookupErr := h.Store.FindByIdempotencyKey(nextKey)
		if lookupErr == nil && found {
			nextWispID = existingID
		} else {
			return HandlerResult{}, fmt.Errorf("pouring next wisp for bead %q: %w", beadID, err)
		}
	}
	if _, err := h.exactWispEvidence(beadID, nextKey, nextWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("validating next wisp for bead %q: %w", beadID, err)
	}

	// PourWisp succeeded — now mutate state.
	// Clear verdict (scoped to last processed wisp) after PourWisp so it's
	// preserved if PourWisp fails and the operator retries.
	if lastProcessedWisp != "" && meta[FieldAgentVerdictWisp] == lastProcessedWisp {
		if err := h.Store.SetMetadata(beadID, FieldAgentVerdict, ""); err != nil {
			return HandlerResult{}, fmt.Errorf("clearing agent verdict: %w", err)
		}
		if err := h.Store.SetMetadata(beadID, FieldAgentVerdictWisp, ""); err != nil {
			return HandlerResult{}, fmt.Errorf("clearing agent verdict wisp: %w", err)
		}
	}
	// Clear waiting_reason and set state=active.
	if err := h.Store.SetMetadata(beadID, FieldWaitingReason, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing waiting reason: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldState, StateActive); err != nil {
		return HandlerResult{}, fmt.Errorf("setting state to active: %w", err)
	}

	// Set active_wisp.
	if err := h.Store.SetMetadata(beadID, FieldActiveWisp, nextWispID); err != nil {
		return HandlerResult{}, fmt.Errorf("setting active wisp: %w", err)
	}
	// Emit ConvergenceManualIterate event.
	iterPayload := ManualActionPayload{
		Actor:      actor,
		PriorState: StateWaitingManual,
		NewState:   StateActive,
		Iteration:  nextIteration,
		WispID:     NullableString(lastProcessedWisp),
		NextWispID: NullableString(nextWispID),
	}
	h.emitEvent(EventManualIterate, EventIDManualIterate(beadID, nextIteration), beadID, iterPayload)

	return HandlerResult{
		Action:     ActionIterate,
		Iteration:  nextIteration,
		NextWispID: nextWispID,
	}, nil
}

// StopHandler processes an operator's request to stop a convergence loop.
// The loop can be in active or waiting_manual state. It terminates the loop
// with terminal_reason=stopped.
//
// Enhanced stop sequence:
//  1. Validate state (active or waiting_manual)
//  2. Drain completed iteration — if active wisp is already closed, process it
//     through HandleWispClosed first to avoid discarding a legitimate iteration
//  3. Force-close active wisp — if still open after drain, force-close it
//  4. Derive iteration count (after force-close so count is accurate)
//  5. Clear stale verdicts — prevent interrupted wisp's verdict from leaking
//  6. Write terminal state metadata
//  7. CloseBead
//  8. Write last_processed_wisp LAST (dedup marker)
//  9. Emit the synthetic stopped iteration, terminated, and manual-stop events
//     only after all terminal proof is durable
//
// Idempotent: if the bead is already terminated with reason=stopped,
// returns a no-op result without error.
//
// Write ordering contract: last_processed_wisp is written LAST (dedup marker).
func (h *Handler) StopHandler(ctx context.Context, beadID, username, _ string) (HandlerResult, error) {
	meta, err := h.Store.GetMetadata(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("reading bead %q metadata: %w", beadID, err)
	}

	state := meta[FieldState]
	actor := "operator:" + username

	// Idempotent: already terminated+stopped is a no-op.
	if state == StateTerminated && meta[FieldTerminalReason] == TerminalStopped {
		return HandlerResult{
			Action: ActionStopped,
		}, nil
	}

	// Must be active, waiting_manual, or waiting_trigger.
	if state != StateActive && state != StateWaitingManual && state != StateWaitingTrigger {
		return HandlerResult{}, fmt.Errorf(
			"cannot stop bead %q: state is %q, expected %q, %q, or %q",
			beadID, state, StateActive, StateWaitingManual, StateWaitingTrigger,
		)
	}

	activeWisp := meta[FieldActiveWisp]
	lastProcessedWisp := meta[FieldLastProcessedWisp]
	forceClosedWisp := false

	// Step 2: Drain completed iteration — if the active wisp is already closed,
	// process it through HandleWispClosed before stopping. This prevents
	// discarding a legitimately completed iteration.
	if activeWisp != "" {
		wispInfo, err := h.Store.GetBead(activeWisp)
		if err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				return HandlerResult{}, fmt.Errorf("reading active wisp %q: %w", activeWisp, err)
			}
			recoveredWisp, found, recoverErr := h.recoverCurrentActiveWisp(beadID, lastProcessedWisp)
			if recoverErr != nil {
				return HandlerResult{}, recoverErr
			}
			if !found {
				activeWisp = ""
			} else {
				activeWisp = recoveredWisp.ID
				wispInfo = recoveredWisp
			}
		}

		if activeWisp != "" && wispInfo.Status == "closed" {
			// Drain: process the completed wisp through the normal handler.
			_, drainErr := h.HandleWispClosed(ctx, beadID, activeWisp)
			if drainErr != nil {
				return HandlerResult{}, fmt.Errorf("draining completed wisp %q: %w", activeWisp, drainErr)
			}

			// Re-read metadata after drain — HandleWispClosed may have terminated
			// the loop (gate passed or max iterations reached).
			meta, err = h.Store.GetMetadata(beadID)
			if err != nil {
				return HandlerResult{}, fmt.Errorf("re-reading metadata after drain: %w", err)
			}
			if meta[FieldState] == StateTerminated {
				// HandleWispClosed already terminated the loop — stop is a no-op.
				return HandlerResult{
					Action: ActionStopped,
				}, nil
			}
			// Update local vars from refreshed metadata.
			state = meta[FieldState]
			activeWisp = meta[FieldActiveWisp]
			lastProcessedWisp = meta[FieldLastProcessedWisp]
			if activeWisp != "" && activeWisp != lastProcessedWisp {
				successor, err := h.Store.GetBead(activeWisp)
				if err != nil {
					return HandlerResult{}, fmt.Errorf("reading adopted successor %q after drain: %w", activeWisp, err)
				}
				if successor.Status == "closed" {
					return HandlerResult{
						Action:     ActionSkipped,
						NextWispID: activeWisp,
					}, fmt.Errorf("closed successor %q awaits convergence tick owner before stop can continue", activeWisp)
				}
			}
		}
	}

	// Step 3: Force-close active wisp if still open.
	if activeWisp != "" {
		wispInfo, err := h.Store.GetBead(activeWisp)
		if err != nil {
			if !errors.Is(err, beads.ErrNotFound) {
				return HandlerResult{}, fmt.Errorf("reading active wisp %q for force-close: %w", activeWisp, err)
			}
			recoveredWisp, found, recoverErr := h.recoverCurrentActiveWisp(beadID, lastProcessedWisp)
			if recoverErr != nil {
				return HandlerResult{}, recoverErr
			}
			if !found {
				activeWisp = ""
			} else {
				activeWisp = recoveredWisp.ID
				wispInfo = recoveredWisp
			}
		}
		if activeWisp != "" && wispInfo.Status != "closed" {
			if err := h.Store.CloseBead(activeWisp, CloseReasonManualSupersede); err != nil {
				return HandlerResult{}, fmt.Errorf("force-closing active wisp %q: %w", activeWisp, err)
			}
			forceClosedWisp = true
		}
	}

	// Step 4: Take one checked child snapshot after force-close. It supplies
	// event totals and the applicable marker, repairing a stale marker left by
	// an interrupted waiting transition before terminal events are published.
	children, err := h.Store.Children(beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("listing children for terminal proof on bead %q: %w", beadID, err)
	}
	stats, err := childStats(children, beadID)
	if err != nil {
		return HandlerResult{}, fmt.Errorf("validating child evidence for terminal proof on bead %q: %w", beadID, err)
	}
	iterationCount := stats.ClosedCount

	// Step 5: Clear stale verdicts — prevent an interrupted wisp's verdict
	// from leaking into a future retry.
	if err := h.Store.SetMetadata(beadID, FieldAgentVerdict, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing stale agent verdict: %w", err)
	}
	if err := h.Store.SetMetadata(beadID, FieldAgentVerdictWisp, ""); err != nil {
		return HandlerResult{}, fmt.Errorf("clearing stale agent verdict wisp: %w", err)
	}

	// The highest closed convergence child is the terminal dedup proof. Preserve
	// the existing marker only for the legitimate no-closed-child case.
	finalLPW := lastProcessedWisp
	if stats.HighestClosedFound {
		finalLPW = stats.HighestClosed.ID
	}

	// Use the best available wisp reference for event payloads.
	eventWispID := finalLPW
	if activeWisp != "" {
		eventWispID = activeWisp
	}

	stopPayload := ManualActionPayload{
		Actor:      actor,
		PriorState: state,
		NewState:   StateTerminated,
		Iteration:  iterationCount,
		WispID:     NullableString(eventWispID),
	}

	var stoppedIterationPayload *IterationPayload
	if forceClosedWisp && activeWisp != "" {
		wispIteration := iterationCount
		var iterationDurationMs int64
		for _, child := range children {
			if child.ID == activeWisp && !child.CreatedAt.IsZero() && !child.ClosedAt.IsZero() {
				iterationDurationMs = child.ClosedAt.Sub(child.CreatedAt).Milliseconds()
				break
			}
		}
		gateMode := meta[FieldGateMode]
		if gateMode == "" {
			gateMode = GateModeManual
		}
		stoppedIterationPayload = &IterationPayload{
			Iteration:            wispIteration,
			WispID:               activeWisp,
			Action:               string(ActionStopped),
			GateMode:             gateMode,
			IterationDurationMs:  iterationDurationMs,
			CumulativeDurationMs: stats.CumulativeDur.Milliseconds(),
		}
	}
	termPayload := TerminatedPayload{
		TerminalReason:       TerminalStopped,
		TotalIterations:      iterationCount,
		FinalStatus:          "closed",
		Actor:                actor,
		CumulativeDurationMs: stats.CumulativeDur.Milliseconds(),
	}

	// Commit terminal state, close the root, and stamp the dedup marker last.
	if err := h.commit(beadID,
		[]metaWrite{
			{FieldTerminalReason, TerminalStopped, "setting terminal reason"},
			{FieldTerminalActor, actor, "setting terminal actor"},
			{FieldWaitingReason, "", "clearing waiting reason"},
			{FieldState, StateTerminated, "setting state to terminated"},
		},
		func() error {
			if err := h.Store.CloseBead(beadID, CloseReasonManualStop); err != nil {
				return fmt.Errorf("closing bead %q: %w", beadID, err)
			}
			return nil
		},
		metaWrite{FieldLastProcessedWisp, finalLPW, "setting last processed wisp"},
	); err != nil {
		return HandlerResult{}, err
	}

	if stoppedIterationPayload != nil {
		h.emitEvent(EventIteration, EventIDIteration(beadID, stoppedIterationPayload.Iteration), beadID, *stoppedIterationPayload)
	}
	h.emitEvent(EventTerminated, EventIDTerminated(beadID), beadID, termPayload)
	h.emitEvent(EventManualStop, EventIDManualStop(beadID), beadID, stopPayload)

	return HandlerResult{
		Action:    ActionStopped,
		Iteration: iterationCount,
	}, nil
}

func (h *Handler) recoverCurrentActiveWisp(beadID, lastProcessedWisp string) (BeadInfo, bool, error) {
	children, err := h.Store.Children(beadID)
	if err != nil {
		return BeadInfo{}, false, fmt.Errorf("listing children for stale active wisp recovery: %w", err)
	}
	if _, err := childStats(children, beadID); err != nil {
		return BeadInfo{}, false, fmt.Errorf("validating child evidence for stale active wisp recovery: %w", err)
	}

	if lastProcessedWisp != "" {
		lastProcessedInfo, err := h.Store.GetBead(lastProcessedWisp)
		if err != nil {
			return BeadInfo{}, false, fmt.Errorf("reading last processed wisp %q: %w", lastProcessedWisp, err)
		}
		processedIter, ok := ParseIterationFromKey(lastProcessedInfo.IdempotencyKey)
		if !ok || processedIter < 1 {
			return BeadInfo{}, false, fmt.Errorf("last processed wisp %q has invalid idempotency key %q", lastProcessedWisp, lastProcessedInfo.IdempotencyKey)
		}
		if err := validateExactWispEvidence(beadID, IdempotencyKey(beadID, processedIter), lastProcessedWisp, lastProcessedInfo); err != nil {
			return BeadInfo{}, false, fmt.Errorf("validating last processed wisp: %w", err)
		}
		if lastProcessedInfo.Status != "closed" {
			return BeadInfo{}, false, fmt.Errorf("last processed wisp %q has status %q, want closed", lastProcessedWisp, lastProcessedInfo.Status)
		}
		nextIter, err := nextIterationAfterLastProcessed(beadID, lastProcessedWisp, children)
		if err != nil {
			return BeadInfo{}, false, fmt.Errorf("deriving replacement active iteration: %w", err)
		}
		nextKey := IdempotencyKey(beadID, nextIter)
		candidateID, found, err := h.Store.FindByIdempotencyKey(nextKey)
		if err != nil {
			return BeadInfo{}, false, fmt.Errorf("looking up replacement active wisp %q: %w", nextKey, err)
		}
		if !found {
			if candidateID != "" {
				return BeadInfo{}, false, fmt.Errorf("replacement lookup for %q returned bead ID %q without found evidence", nextKey, candidateID)
			}
			return BeadInfo{}, false, nil
		}
		if candidateID == "" {
			return BeadInfo{}, false, fmt.Errorf("replacement lookup for %q returned found with an empty bead ID", nextKey)
		}
		wispInfo, err := h.Store.GetBead(candidateID)
		if err != nil {
			return BeadInfo{}, false, fmt.Errorf("reading replacement active wisp %q: %w", candidateID, err)
		}
		if err := validateExactWispEvidence(beadID, nextKey, candidateID, wispInfo); err != nil {
			return BeadInfo{}, false, fmt.Errorf("validating replacement active wisp: %w", err)
		}
		return wispInfo, true, nil
	}

	// With no marker, only canonical iteration 1 is actionable. Selecting a
	// later child would silently skip the missing prefix of the chain.
	nextIter, err := nextIterationAfterLastProcessed(beadID, "", children)
	if err != nil {
		return BeadInfo{}, false, fmt.Errorf("deriving marker-less replacement iteration: %w", err)
	}
	nextKey := IdempotencyKey(beadID, nextIter)
	for _, child := range children {
		if child.IdempotencyKey == nextKey {
			return child, true, nil
		}
	}
	return BeadInfo{}, false, nil
}
