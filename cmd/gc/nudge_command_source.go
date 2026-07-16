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
	repository   *nudgequeue.CommandRepository
	reader       *nudgequeue.CommandPartitionReader
	partition    nudgequeue.TrustedCityPartition
	membership   nudgequeue.TrustedCommandPartitionMembershipRecorder
	terminal     nudgequeue.TrustedCommandClaimStateAuthority
	recovery     nudgequeue.TrustedCommandAuthorityRecovery
	recoveryGate chan struct{}
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
	source := &productionNudgeCommandSource{
		repository:   repository,
		partition:    partition,
		recoveryGate: make(chan struct{}, 1),
	}
	source.recoveryGate <- struct{}{}
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
	terminal, ok := resolver.(nudgequeue.TrustedCommandClaimStateAuthority)
	if !ok || terminal == nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, nudgequeue.ErrCommandPartitionTerminalIntent, nudgequeue.ErrCommandRepositoryPartition, errors.New("trusted command membership and terminal authority is required"))
	}
	recovery, ok := resolver.(nudgequeue.TrustedCommandAuthorityRecovery)
	if !ok || recovery == nil {
		return nil, errors.Join(errNudgeCommandSourceUnverified, nudgequeue.ErrCommandRepositoryPartition, errors.New("trusted command authority recovery is required"))
	}
	source.recovery = recovery
	if err := source.recovery.RecoverCommandAuthority(ctx, repository); err != nil {
		failure := fmt.Errorf("recovering durable nudge command authority: %w", err)
		if source.ClassifyNudgeCommandSourceError(failure) == nudgeCommandSourceErrorTransient {
			return nil, retryableNudgeCommandSourceFailure(failure)
		}
		return nil, errors.Join(errNudgeCommandSourceUnverified, failure)
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
	if s == nil || s.repository == nil || s.reader == nil || s.recovery == nil || s.recoveryGate == nil {
		return nudgequeue.CommandIndexSnapshot{}, errors.New("snapshotting production nudge command source: source is not fully bound")
	}
	var snapshot nudgequeue.CommandIndexSnapshot
	read := func() error {
		var err error
		snapshot, err = s.reader.Snapshot(ctx, limit)
		return err
	}
	err := read()
	if err == nil || !nudgeCommandReadNeedsAuthorityRecovery(err) {
		return snapshot, err
	}
	return snapshot, s.recoverLiveRead(ctx, "snapshot", err, read)
}

func (s *productionNudgeCommandSource) Get(ctx context.Context, commandID string) (nudgequeue.CommandIndexResolution, error) {
	if s == nil || s.repository == nil || s.reader == nil || s.recovery == nil || s.recoveryGate == nil {
		return nudgequeue.CommandIndexResolution{}, errors.New("reading production nudge command source: source is not fully bound")
	}
	var resolution nudgequeue.CommandIndexResolution
	read := func() error {
		var err error
		resolution, err = s.reader.Get(ctx, commandID)
		return err
	}
	err := read()
	if err == nil || !nudgeCommandReadNeedsAuthorityRecovery(err) {
		return resolution, err
	}
	return resolution, s.recoverLiveRead(ctx, "exact read", err, read)
}

func nudgeCommandReadNeedsAuthorityRecovery(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, nudgequeue.ErrCommandProvenanceRejected) {
		return false
	}
	if errors.Is(err, nudgequeue.ErrCommandRepositoryPartition) {
		return true
	}
	var lineage *nudgequeue.CommandRepositoryLineageError
	return errors.As(err, &lineage) && lineage != nil
}

func (s *productionNudgeCommandSource) recoverLiveRead(ctx context.Context, operation string, initialErr error, read func() error) error {
	if ctx == nil {
		return errors.New("recovering durable nudge command authority: context is nil")
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-s.recoveryGate:
	}
	defer func() { s.recoveryGate <- struct{}{} }()

	// Another failed reader may have repaired the same authority gap while this
	// caller waited. Recheck while holding the recovery gate before doing durable work.
	if err := ctx.Err(); err != nil {
		return err
	}
	readErr := read()
	if readErr == nil {
		return nil
	}
	if !nudgeCommandReadNeedsAuthorityRecovery(readErr) {
		return readErr
	}
	if recoveryErr := s.recovery.RecoverCommandAuthority(ctx, s.repository); recoveryErr != nil {
		return errors.Join(
			initialErr,
			readErr,
			fmt.Errorf("recovering durable nudge command authority after live %s failure: %w", operation, recoveryErr),
		)
	}
	if err := read(); err != nil {
		return fmt.Errorf("%s after durable nudge command authority recovery: %w", operation, err)
	}
	return nil
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
	// A claim receipt mismatch is durable contradictory evidence even if a
	// lower layer also reported availability noise while unwinding it.
	if errors.Is(err, nudgequeue.ErrCommandClaimTransition) && errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityConflict) {
		return nudgeCommandSourceErrorInvariant
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, context.DeadlineExceeded) ||
		errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityUnavailable) ||
		errors.Is(err, nudgequeue.ErrCommandAuthorityRecoveryYield) ||
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
