package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestOpenProductionNudgeCommandSourceRecoversOnceBeforePublishingReader(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	authority := &startupFenceNudgeAuthority{ingress: fixture.ingress}

	source, err := openVerifiedProductionNudgeCommandSource(
		t.Context(),
		fixture.cityPath,
		fixture.store,
		fixture.partition,
		authority,
	)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	if source == nil {
		t.Fatal("openVerifiedProductionNudgeCommandSource returned a nil source")
	}
	if authority.recoveryCalls != 1 {
		t.Fatalf("unified authority recovery calls = %d, want exactly 1", authority.recoveryCalls)
	}
	if authority.readBeforeRecovery {
		t.Fatal("partition reader consulted authority before unified recovery completed")
	}

	if _, err := source.Snapshot(t.Context(), 1); err != nil {
		t.Fatalf("Snapshot after recovery fence: %v", err)
	}
	if authority.readBeforeRecovery {
		t.Fatal("published source reached partition authority without completed recovery")
	}
}

func TestOpenProductionNudgeCommandSourceRecoveryFailurePublishesNothing(t *testing.T) {
	tests := []struct {
		name          string
		recoveryErr   error
		wantTransient bool
	}{
		{
			name:        "schema skew",
			recoveryErr: errors.Join(nudgequeue.ErrLocalNudgeAuthorityConflict, errors.New("authority schema manifest differs")),
		},
		{
			name:        "incomplete recovery fence",
			recoveryErr: errors.New("authority decision fence remains incomplete"),
		},
		{
			name:          "transient authority outage",
			recoveryErr:   context.DeadlineExceeded,
			wantTransient: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fixture := newProductionNudgeCommandFixture(t)
			authority := &startupFenceNudgeAuthority{ingress: fixture.ingress, recoveryErr: test.recoveryErr}

			source, err := openVerifiedProductionNudgeCommandSource(
				t.Context(), fixture.cityPath, fixture.store, fixture.partition, authority,
			)
			if source != nil {
				t.Fatalf("source after failed recovery = %T, want nil", source)
			}
			if !errors.Is(err, test.recoveryErr) {
				t.Fatalf("recovery error = %v, want cause %v", err, test.recoveryErr)
			}
			if authority.recoveryCalls != 1 {
				t.Fatalf("unified authority recovery calls = %d, want exactly 1", authority.recoveryCalls)
			}
			if authority.readCalls != 0 || authority.readBeforeRecovery {
				t.Fatalf("partition authority reads before failed recovery = %d/%t, want none", authority.readCalls, authority.readBeforeRecovery)
			}
			if got := nudgeCommandSourceFailureIsTransient(err); got != test.wantTransient {
				t.Fatalf("transient classification = %t, want %t for %v", got, test.wantTransient, err)
			}
			if !test.wantTransient && !errors.Is(err, errNudgeCommandSourceUnverified) {
				t.Fatalf("invariant recovery error = %v, want unverified-source refusal", err)
			}
		})
	}
}

// startupFenceNudgeAuthority deliberately exposes the single startup-recovery
// contract and not the superseded per-stage recovery methods. The remaining
// methods are the independently required partition and terminal capabilities.
type startupFenceNudgeAuthority struct {
	ingress            *nudgequeue.TrustedNudgeIngress
	recoveryErr        error
	recoveryCalls      int
	recovered          bool
	readBeforeRecovery bool
	readCalls          int
}

func (a *startupFenceNudgeAuthority) RecoverCommandAuthority(_ context.Context, repository *nudgequeue.CommandRepository) error {
	a.recoveryCalls++
	if repository == nil {
		return errors.New("startup-fence recovery received a nil repository")
	}
	if a.recoveryErr != nil {
		return a.recoveryErr
	}
	a.recovered = true
	return nil
}

func (a *startupFenceNudgeAuthority) ResolveCommandPartition(ctx context.Context, reference nudgequeue.TrustedIngressReference) (nudgequeue.TrustedCityPartition, error) {
	a.noteRead()
	return a.ingress.ResolveCommandPartition(ctx, reference)
}

func (a *startupFenceNudgeAuthority) ResolveCommandPartitionCoverage(ctx context.Context, request nudgequeue.CommandPartitionCoverageRequest) (nudgequeue.CommandPartitionCoverage, error) {
	a.noteRead()
	return a.ingress.ResolveCommandPartitionCoverage(ctx, request)
}

func (a *startupFenceNudgeAuthority) ResolveCommandPartitionMembership(ctx context.Context, request nudgequeue.CommandPartitionMembershipRequest) (nudgequeue.CommandPartitionMembership, error) {
	a.noteRead()
	return a.ingress.ResolveCommandPartitionMembership(ctx, request)
}

func (a *startupFenceNudgeAuthority) VerifyCommandRepositoryEffectFence(ctx context.Context, state nudgequeue.CommandRepositoryState) error {
	a.noteRead()
	return a.ingress.VerifyCommandRepositoryEffectFence(ctx, state)
}

func (a *startupFenceNudgeAuthority) RecordCommandRepositoryEffectFence(ctx context.Context, state nudgequeue.CommandRepositoryState) error {
	return a.ingress.RecordCommandRepositoryEffectFence(ctx, state)
}

func (a *startupFenceNudgeAuthority) VerifyCommandRetryClaim(ctx context.Context, verification nudgequeue.CommandRetryClaimVerification) error {
	a.noteRead()
	return a.ingress.VerifyCommandRetryClaim(ctx, verification)
}

func (a *startupFenceNudgeAuthority) RecordCommandPartitionAdmission(ctx context.Context, admission nudgequeue.CommandPartitionAdmission) error {
	return a.ingress.RecordCommandPartitionAdmission(ctx, admission)
}

func (a *startupFenceNudgeAuthority) RecordCommandPartitionTerminal(ctx context.Context, terminal nudgequeue.CommandPartitionTerminal) error {
	return a.ingress.RecordCommandPartitionTerminal(ctx, terminal)
}

func (a *startupFenceNudgeAuthority) PrepareCommandPartitionTerminal(ctx context.Context, intent nudgequeue.CommandPartitionTerminalIntent) error {
	return a.ingress.PrepareCommandPartitionTerminal(ctx, intent)
}

func (a *startupFenceNudgeAuthority) ReleaseCommandPartitionTerminalWriter(ctx context.Context, intent nudgequeue.CommandPartitionTerminalIntent) error {
	return a.ingress.ReleaseCommandPartitionTerminalWriter(ctx, intent)
}

func (a *startupFenceNudgeAuthority) AbortCommandPartitionTerminal(ctx context.Context, intent nudgequeue.CommandPartitionTerminalIntent) error {
	return a.ingress.AbortCommandPartitionTerminal(ctx, intent)
}

func (a *startupFenceNudgeAuthority) VerifyCommandPartitionTerminal(ctx context.Context, resolution nudgequeue.CommandPartitionTerminalResolution) error {
	return a.ingress.VerifyCommandPartitionTerminal(ctx, resolution)
}

func (a *startupFenceNudgeAuthority) PrepareCommandClaimTransition(ctx context.Context, intent nudgequeue.CommandClaimTransitionIntent) error {
	return a.ingress.PrepareCommandClaimTransition(ctx, intent)
}

func (a *startupFenceNudgeAuthority) ReleaseCommandClaimTransitionWriter(ctx context.Context, intent nudgequeue.CommandClaimTransitionIntent) error {
	return a.ingress.ReleaseCommandClaimTransitionWriter(ctx, intent)
}

func (a *startupFenceNudgeAuthority) AbortCommandClaimTransition(ctx context.Context, intent nudgequeue.CommandClaimTransitionIntent) error {
	return a.ingress.AbortCommandClaimTransition(ctx, intent)
}

func (a *startupFenceNudgeAuthority) FinalizeCommandClaimTransition(ctx context.Context, receipt nudgequeue.CommandClaimTransitionReceipt) (nudgequeue.CommandClaimReceiptDisposition, error) {
	return a.ingress.FinalizeCommandClaimTransition(ctx, receipt)
}

func (a *startupFenceNudgeAuthority) noteRead() {
	a.readCalls++
	if !a.recovered {
		a.readBeforeRecovery = true
	}
}

func (a *productionNudgeTestAuthority) RecoverCommandAuthority(ctx context.Context, repository *nudgequeue.CommandRepository) error {
	if err := a.RepairCommandPartitionAdmissions(ctx, repository); err != nil {
		return err
	}
	return a.RepairCommandPartitionTerminals(ctx, repository)
}
