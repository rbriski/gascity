package main

import (
	"context"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// The production-source test authority models exact membership and terminal
// publication but has no independent persistent store to rewind. These no-op
// effect-fence methods complete that fake boundary; LocalNudgeAuthority tests
// exercise the durable monotonic implementation.
func (a *productionNudgeTestAuthority) VerifyCommandRepositoryEffectFence(ctx context.Context, _ nudgequeue.CommandRepositoryState) error {
	return ctx.Err()
}

func (a *productionNudgeTestAuthority) RecordCommandRepositoryEffectFence(ctx context.Context, _ nudgequeue.CommandRepositoryState) error {
	return ctx.Err()
}
