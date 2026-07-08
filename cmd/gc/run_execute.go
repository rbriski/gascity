package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
)

// runOutcome is the terminal disposition of a one-shot workflow run.
type runOutcome struct {
	// Terminal is true when the workflow root closed; false when the watch gave
	// up on the deadline (a non-destructive timeout, not a completion).
	Terminal bool
	// Outcome is the gc.outcome stamped on the closed root ("pass" | "fail" | …).
	Outcome string
}

// Passed reports whether the run completed successfully. Only a terminal
// gc.outcome of "pass" counts; a non-terminal (deadline) result is never a pass.
func (o runOutcome) Passed() bool {
	return o.Terminal && o.Outcome == beadmeta.OutcomePass
}

// watchWorkflowRoot polls the workflow root bead until it CLOSES, returning its
// gc.outcome. The control-dispatcher closes the root inside
// processWorkflowFinalize — before it closes the finalize bead itself — so a
// closed root is the authoritative terminal signal (internal/dispatch/runtime.go).
// We watch the root's close rather than a ready-queue-empty heuristic, which the
// fork's wisp-flood history disproved.
//
// If deadline elapses before the root closes, it returns a NON-terminal result:
// the caller keeps the city directory and never destroys work on a timer. The
// deadline is a fail-safe against a wedged run hanging forever, not a reaper.
func watchWorkflowRoot(ctx context.Context, store beads.Store, rootID string, poll, deadline time.Duration) (runOutcome, error) {
	if poll <= 0 {
		poll = time.Second
	}
	var deadlineC <-chan time.Time
	if deadline > 0 {
		timer := time.NewTimer(deadline)
		defer timer.Stop()
		deadlineC = timer.C
	}
	ticker := time.NewTicker(poll)
	defer ticker.Stop()

	for {
		root, err := store.Get(rootID)
		switch {
		case err == nil && root.Status == "closed":
			return runOutcome{Terminal: true, Outcome: root.Metadata[beadmeta.OutcomeMetadataKey]}, nil
		case err != nil && !errors.Is(err, beads.ErrNotFound):
			return runOutcome{}, fmt.Errorf("watching workflow root %s: %w", rootID, err)
		}
		select {
		case <-ctx.Done():
			return runOutcome{}, ctx.Err()
		case <-deadlineC:
			return runOutcome{Terminal: false}, nil
		case <-ticker.C:
		}
	}
}
