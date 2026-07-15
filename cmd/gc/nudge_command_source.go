package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// productionNudgeCommandSource keeps read projection and write authority as
// separate capabilities. Its opaque partition is captured only by the trusted
// opener and injected into claims internally; an effect callback cannot choose
// or forge city authority.
type productionNudgeCommandSource struct {
	repository *nudgequeue.CommandRepository
	reader     *nudgequeue.CommandPartitionReader
	partition  nudgequeue.TrustedCityPartition
	membership nudgequeue.TrustedCommandPartitionMembershipRecorder
	terminal   nudgequeue.TrustedCommandPartitionTerminalIntentAuthority
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
	source := &productionNudgeCommandSource{repository: repository, partition: partition}
	if _, err := repository.Provision(ctx); err != nil {
		// A command commit can become durable before its independent anchor
		// advance is acknowledged. This is an explicit writer path, so it may
		// repair only an existing same-store/same-epoch anchor; missing, foreign,
		// rewound, or epoch-changed lineage still fails closed.
		if _, repairErr := repository.RepairLineage(ctx); repairErr != nil {
			failure := errors.Join(
				fmt.Errorf("provisioning durable nudge command repository: %w", err),
				fmt.Errorf("repairing durable nudge command repository lineage: %w", repairErr),
			)
			if source.ClassifyNudgeCommandSourceError(failure) == nudgeCommandSourceErrorTransient {
				return nil, retryableNudgeCommandSourceFailure(failure)
			}
			return nil, failure
		}
	}
	membership, ok := resolver.(nudgequeue.TrustedCommandPartitionMembershipRecorder)
	if !ok || membership == nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, nudgequeue.ErrCommandRepositoryPartition, errors.New("trusted partition membership recorder is required"))
	}
	terminal, ok := resolver.(nudgequeue.TrustedCommandPartitionTerminalIntentAuthority)
	if !ok || terminal == nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, nudgequeue.ErrCommandPartitionTerminalIntent, errors.New("trusted terminal intent authority is required"))
	}
	recovery, ok := resolver.(nudgequeue.TrustedCommandPartitionTerminalRecovery)
	if !ok || recovery == nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, nudgequeue.ErrCommandPartitionTerminalIntent, errors.New("trusted terminal recovery authority is required"))
	}
	if err := recovery.RepairCommandPartitionTerminals(ctx, repository); err != nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, nudgequeue.ErrCommandPartitionTerminalIntent, fmt.Errorf("repairing prepared terminal transitions: %w", err))
	}
	partitioned, err := nudgequeue.NewCommandPartitionReader(repository, partition, resolver)
	if err != nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, err)
	}
	source.reader = partitioned
	source.membership = membership
	source.terminal = terminal
	return source, nil
}

func (s *productionNudgeCommandSource) Snapshot(ctx context.Context, limit int) (nudgequeue.CommandIndexSnapshot, error) {
	if s == nil || s.reader == nil {
		return nudgequeue.CommandIndexSnapshot{}, errors.New("snapshotting production nudge command source: partition reader is nil")
	}
	return s.reader.Snapshot(ctx, limit)
}

func (s *productionNudgeCommandSource) Get(ctx context.Context, commandID string) (nudgequeue.CommandIndexResolution, error) {
	if s == nil || s.reader == nil {
		return nudgequeue.CommandIndexResolution{}, errors.New("reading production nudge command source: partition reader is nil")
	}
	return s.reader.Get(ctx, commandID)
}

func (s *productionNudgeCommandSource) ClaimAuthorized(ctx context.Context, request nudgeEffectClaimRequest, authorizer nudgequeue.NudgeClaimAuthorizer) (nudgequeue.CommandClaimResult, error) {
	if s == nil || s.repository == nil || s.reader == nil || s.membership == nil || s.terminal == nil {
		return nudgequeue.CommandClaimResult{}, errors.New("claiming production nudge command: source is not fully bound")
	}
	result, err := s.repository.ClaimAuthorized(ctx, nudgequeue.CommandClaimRequest{
		CommandID:           request.commandID,
		ClaimID:             request.claimID,
		OwnerID:             request.ownerID,
		AttemptID:           request.attemptID,
		BoundLaunchIdentity: request.boundLaunchIdentity,
		Partition:           s.partition,
		ClaimedAt:           request.claimedAt,
		LeaseUntil:          request.leaseUntil,
	}, authorizer, s.terminal)
	if err != nil {
		return result, err
	}
	if !result.HasTerminalTransitionWitness() {
		return result, nil
	}
	if err := s.recordTerminalMembership(ctx, result.Command); err != nil {
		return result, err
	}
	return result, nil
}

func (s *productionNudgeCommandSource) CompleteProviderAttempt(ctx context.Context, request nudgequeue.CommandCompletionRequest) (nudgequeue.CommandCompletionResult, error) {
	if s == nil || s.repository == nil || s.reader == nil || s.membership == nil || s.terminal == nil {
		return nudgequeue.CommandCompletionResult{}, errors.New("completing production nudge command: source is not fully bound")
	}
	result, err := s.repository.CompleteProviderAttempt(ctx, request, s.partition, s.terminal)
	if err != nil {
		return result, err
	}
	if !result.HasTerminalTransitionWitness() {
		return result, nil
	}
	if err := s.recordTerminalMembership(ctx, result.Command); err != nil {
		return result, err
	}
	return result, nil
}

func (s *productionNudgeCommandSource) recordTerminalMembership(ctx context.Context, command nudgequeue.Command) error {
	if err := s.membership.RecordCommandPartitionTerminal(ctx, nudgequeue.CommandPartitionTerminal{
		Store:              command.Store,
		RepositoryRevision: command.Order.Revision,
		CommandID:          command.ID,
		Sequence:           command.Order.Sequence,
		Partition:          s.partition,
	}); err != nil {
		return fmt.Errorf("publishing trusted terminal command membership: %w", err)
	}
	return nil
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
	_ nudgeCommandEffectSource          = (*productionNudgeCommandSource)(nil)
	_ nudgeCommandSourceErrorClassifier = (*productionNudgeCommandSource)(nil)
)
