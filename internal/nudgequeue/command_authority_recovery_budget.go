package nudgequeue

import (
	"context"
	"errors"
	"fmt"
)

const (
	commandAuthorityRecoveryMaxPasses = 8
	commandAuthorityRecoveryMaxWork   = localAuthorityRecoveryPageSize
)

// ErrCommandAuthorityRecoveryYield reports safe, durable partial recovery
// that exhausted one invocation's deterministic work budget. Callers should
// retry with backoff; the next invocation resumes from authority-owned state.
var ErrCommandAuthorityRecoveryYield = errors.New("durable nudge command authority recovery yielded")

type commandAuthorityRecoveryBudgetContextKey struct{}

type commandAuthorityRecoveryBudget struct {
	passes int
	work   int
}

func withCommandAuthorityRecoveryBudget(ctx context.Context) (context.Context, *commandAuthorityRecoveryBudget) {
	if budget, ok := ctx.Value(commandAuthorityRecoveryBudgetContextKey{}).(*commandAuthorityRecoveryBudget); ok && budget != nil {
		return ctx, budget
	}
	budget := &commandAuthorityRecoveryBudget{}
	return context.WithValue(ctx, commandAuthorityRecoveryBudgetContextKey{}, budget), budget
}

func (b *commandAuthorityRecoveryBudget) takePass(operation string) error {
	if b.passes >= commandAuthorityRecoveryMaxPasses {
		return fmt.Errorf("%w: %s exceeded the %d-pass budget", ErrCommandAuthorityRecoveryYield, operation, commandAuthorityRecoveryMaxPasses)
	}
	b.passes++
	return nil
}

func (b *commandAuthorityRecoveryBudget) takeWork(operation string) error {
	if b.work >= commandAuthorityRecoveryMaxWork {
		return fmt.Errorf("%w: %s exceeded the %d-unit work budget", ErrCommandAuthorityRecoveryYield, operation, commandAuthorityRecoveryMaxWork)
	}
	b.work++
	return nil
}

func (b *commandAuthorityRecoveryBudget) remainingWork() int {
	if b == nil || b.work >= commandAuthorityRecoveryMaxWork {
		return 0
	}
	return commandAuthorityRecoveryMaxWork - b.work
}

func (b *commandAuthorityRecoveryBudget) takeWorkUnits(units int, operation string) error {
	if units < 0 || units > b.remainingWork() {
		return fmt.Errorf("%w: %s exceeded the %d-unit work budget", ErrCommandAuthorityRecoveryYield, operation, commandAuthorityRecoveryMaxWork)
	}
	b.work += units
	return nil
}
