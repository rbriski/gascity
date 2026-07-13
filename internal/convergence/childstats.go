package convergence

import (
	"fmt"
	"strings"
	"time"
)

// ChildStats holds the projections derived from a bead's validated convergence
// children. Non-convergence children are ignored. Every convergence-looking
// child must carry exact canonical identity, parent, iteration, and lifecycle
// evidence before any count or marker projection is returned.
type ChildStats struct {
	// ClosedCount is the number of closed convergence wisps.
	ClosedCount int
	// CumulativeDur is the summed lifetime of every closed convergence wisp
	// that has both a non-zero created and closed timestamp.
	CumulativeDur time.Duration

	// HighestClosed is the closed convergence wisp with the highest parseable
	// iteration number. HighestClosedFound is false when none qualifies, and
	// HighestClosedIter is then -1.
	HighestClosed      BeadInfo
	HighestClosedIter  int
	HighestClosedFound bool

	// HighestOpen is the open/in_progress convergence wisp with the highest
	// parseable iteration number. HighestOpenFound is false when none
	// qualifies, and HighestOpenIter is then -1.
	HighestOpen      BeadInfo
	HighestOpenIter  int
	HighestOpenFound bool
}

// childStats derives every convergence child projection from a single
// pre-fetched child list. It is pure: it performs no store I/O, so callers can
// fetch Children() once per transition and read the fields they need.
func childStats(children []BeadInfo, beadID string) (ChildStats, error) {
	stats := ChildStats{HighestClosedIter: -1, HighestOpenIter: -1}
	seenIDs := make(map[string]int)
	seenIterations := make(map[int]string)

	for _, child := range children {
		if !strings.HasPrefix(child.IdempotencyKey, "converge:") {
			continue
		}
		iter, ok := ParseIterationFromKey(child.IdempotencyKey)
		if !ok || iter < 1 || child.IdempotencyKey != IdempotencyKey(beadID, iter) {
			return ChildStats{}, fmt.Errorf("child wisp %q has noncanonical idempotency key %q for root %q", child.ID, child.IdempotencyKey, beadID)
		}
		if child.ID == "" {
			return ChildStats{}, fmt.Errorf("iteration %d has an empty child wisp ID", iter)
		}
		if child.ParentID != beadID {
			return ChildStats{}, fmt.Errorf("child wisp %q has parent %q, want %q", child.ID, child.ParentID, beadID)
		}
		if priorIter, duplicate := seenIDs[child.ID]; duplicate {
			return ChildStats{}, fmt.Errorf("child wisp ID %q is duplicated at iterations %d and %d", child.ID, priorIter, iter)
		}
		if priorID, duplicate := seenIterations[iter]; duplicate {
			return ChildStats{}, fmt.Errorf("iteration %d has duplicate child wisps %q and %q", iter, priorID, child.ID)
		}
		seenIDs[child.ID] = iter
		seenIterations[iter] = child.ID

		switch child.Status {
		case "closed":
			stats.ClosedCount++
			if !child.ClosedAt.IsZero() && !child.CreatedAt.IsZero() {
				stats.CumulativeDur += child.ClosedAt.Sub(child.CreatedAt)
			}
			if iter > stats.HighestClosedIter {
				stats.HighestClosed = child
				stats.HighestClosedIter = iter
				stats.HighestClosedFound = true
			}
		case "open", "in_progress":
			if iter > stats.HighestOpenIter {
				stats.HighestOpen = child
				stats.HighestOpenIter = iter
				stats.HighestOpenFound = true
			}
		default:
			return ChildStats{}, fmt.Errorf("child wisp %q has unsupported status %q", child.ID, child.Status)
		}
	}

	return stats, nil
}
