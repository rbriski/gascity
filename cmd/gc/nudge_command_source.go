package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// productionNudgeCommandSource narrows the writable durable repository to the
// two read operations available to the keyed shadow. Provisioning and lineage
// repair happen only during the explicit opener below.
type productionNudgeCommandSource struct {
	repository *nudgequeue.CommandPartitionReader
}

func openVerifiedProductionNudgeCommandSource(
	ctx context.Context,
	cityPath string,
	store beads.Store,
	partition nudgequeue.TrustedCityPartition,
	resolver nudgequeue.TrustedCityPartitionResolver,
) (nudgeCommandSource, error) {
	verifier := nudgequeue.NewRestoreAnchorRepositoryVerifier(cityPath)
	repository, err := nudgequeue.NewCommandRepository(store, verifier)
	if err != nil {
		if errors.Is(err, nudgequeue.ErrCommandRepositoryUnsupported) {
			return nil, errors.Join(errNudgeCommandSourceUnverified, err)
		}
		return nil, fmt.Errorf("constructing durable nudge command repository: %w", err)
	}
	if _, err := repository.Provision(ctx); err != nil {
		// A command commit can become durable before its independent anchor
		// advance is acknowledged. This is an explicit writer path, so it may
		// repair only an existing same-store/same-epoch anchor; missing, foreign,
		// rewound, or epoch-changed lineage still fails closed.
		if _, repairErr := repository.RepairLineage(ctx); repairErr != nil {
			return nil, errors.Join(
				fmt.Errorf("provisioning durable nudge command repository: %w", err),
				fmt.Errorf("repairing durable nudge command repository lineage: %w", repairErr),
			)
		}
	}
	partitioned, err := nudgequeue.NewCommandPartitionReader(repository, partition, resolver)
	if err != nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, err)
	}
	return &productionNudgeCommandSource{repository: partitioned}, nil
}

func (s *productionNudgeCommandSource) Snapshot(ctx context.Context, limit int) (nudgequeue.CommandIndexSnapshot, error) {
	if s == nil || s.repository == nil {
		return nudgequeue.CommandIndexSnapshot{}, errors.New("snapshotting production nudge command source: repository is nil")
	}
	return s.repository.Snapshot(ctx, limit)
}

func (s *productionNudgeCommandSource) Get(ctx context.Context, commandID string) (nudgequeue.CommandIndexResolution, error) {
	if s == nil || s.repository == nil {
		return nudgequeue.CommandIndexResolution{}, errors.New("reading production nudge command source: repository is nil")
	}
	return s.repository.Get(ctx, commandID)
}

func (s *productionNudgeCommandSource) ClassifyNudgeCommandSourceError(err error) nudgeCommandSourceErrorClass {
	if errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, nudgequeue.ErrRestoreAnchorBusy) ||
		errors.Is(err, nudgequeue.ErrRestoreAnchorConflict) ||
		errors.Is(err, nudgequeue.ErrRestoreAnchorDurabilityUncertain) {
		return nudgeCommandSourceErrorTransient
	}
	return nudgeCommandSourceErrorInvariant
}

var (
	_ nudgeCommandSource                = (*productionNudgeCommandSource)(nil)
	_ nudgeCommandSourceErrorClassifier = (*productionNudgeCommandSource)(nil)
)
