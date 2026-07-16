package nudgequeue

import (
	"crypto/sha256"
	"errors"
	"testing"
	"time"
)

func TestLocalNudgeAuthorityAdmissionCannotRacePreparedProvenanceRejection(t *testing.T) {
	state := localAuthorityRepositoryState()
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	request := localAuthorityIngressRequest()
	request.RequestID = "admission-rejection-preparation-race"
	commandID := CommandIDForRequest(state.Store, request.RequestID)
	command := repositoryCommandForRequest(t, state.Store, request.RequestID, "prepared rejection")
	command.Store = state.Store
	command.Order = CommandOrder{Sequence: 1, Revision: 1}
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	intent := CommandProvenanceRejectionIntent{
		Store:                 state.Store,
		CommandID:             commandID,
		Sequence:              1,
		AllocationRevision:    1,
		BeforeCommandRevision: 1,
		IdentityDigest:        commandProvenanceIdentityDigest(state.Store, commandID, 1, 1),
		BeforeCommandDigest:   sha256.Sum256(wire),
		Reason:                CommandProvenanceRejectionReasonUnauthorized,
		RejectedAt:            time.Date(2026, 7, 15, 19, 0, 0, 0, time.UTC),
	}
	if err := authority.PrepareCommandProvenanceRejection(t.Context(), intent); err != nil {
		t.Fatalf("PrepareCommandProvenanceRejection: %v", err)
	}

	authorization, err := authority.AuthorizeNudgeIngress(
		WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()),
		request,
	)
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("AuthorizeNudgeIngress after rejection preparation = %#v, err=%v; want conflict", authorization, err)
	}
	if _, found, err := authority.grantByRequestID(t.Context(), request.RequestID); err != nil || found {
		t.Fatalf("grant after preparation race found=%t err=%v, want absent", found, err)
	}
	if prepared, err := localAuthorityAdmissionPreparationExists(t.Context(), authority.db, commandID); err != nil || prepared {
		t.Fatalf("admission preparation after race found=%t err=%v, want absent", prepared, err)
	}
	if count, err := authority.localAuthorityRejectionPreparationCount(t.Context()); err != nil || count != 1 {
		t.Fatalf("rejection preparations after race = %d, err=%v; want one", count, err)
	}
}

func TestLocalNudgeAuthorityRecoversExactGrantAfterAbsentPreparationRace(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	const requestID = "exact-grant-after-absent-preparation"
	now := time.Date(2026, 7, 15, 20, 0, 0, 0, time.UTC)
	command, partition := prepareCommandAuthorityRecoveryGrant(t, authority, state.Store, requestID, now)
	if err := authority.consumeAbsentLocalNudgeAdmissionPreparation(t.Context(), command.ID); err != nil {
		t.Fatalf("consumeAbsentLocalNudgeAdmissionPreparation: %v", err)
	}
	entry, created, err := repository.create(t.Context(), requestID, command, partition)
	if err != nil || !created || entry.Command == nil {
		t.Fatalf("create exact delayed command = %#v, created=%t err=%v", entry, created, err)
	}

	if err := authority.RepairCommandProvenanceRejections(t.Context(), repository); err != nil {
		t.Fatalf("RepairCommandProvenanceRejections: %v", err)
	}
	resolved, err := repository.Get(t.Context(), command.ID)
	if err != nil || !resolved.Found || resolved.Entry.Command == nil {
		t.Fatalf("Get exact delayed command = %#v, err=%v", resolved, err)
	}
	if resolved.Entry.Command.State != CommandStatePending || resolved.Entry.Command.Terminal != nil {
		t.Fatalf("exact granted command was rejected: %#v", resolved.Entry.Command)
	}
	membership, err := authority.ResolveCommandPartitionMembership(t.Context(), CommandPartitionMembershipRequest{
		Store: state.Store, RepositoryRevision: resolved.Revision, SequenceHighWater: resolved.SequenceHighWater,
		CommandID: command.ID, Partition: partition,
	})
	if err != nil || !membership.Found || membership.Rejected || !membership.Active || membership.Sequence != entry.Command.Order.Sequence {
		t.Fatalf("recovered exact admission membership = %#v, err=%v", membership, err)
	}
}

func TestLocalNudgeAuthorityAdmissionCannotFollowFinalizedProvenanceRejection(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	authority, err := OpenLocalNudgeAuthority(t.Context(), t.TempDir(), state, localAuthorityOptions())
	if err != nil {
		t.Fatalf("OpenLocalNudgeAuthority: %v", err)
	}
	t.Cleanup(func() { _ = authority.Close() })

	request := localAuthorityIngressRequest()
	request.RequestID = "admission-after-finalized-rejection"
	forged := repositoryCommandForRequest(t, state.Store, request.RequestID, "finalized rejection")
	if _, created, err := repository.createForTest(t.Context(), request.RequestID, forged); err != nil || !created {
		t.Fatalf("create forged command: created=%t err=%v", created, err)
	}
	if err := authority.RepairCommandProvenanceRejections(t.Context(), repository); err != nil {
		t.Fatalf("RepairCommandProvenanceRejections: %v", err)
	}

	authorization, err := authority.AuthorizeNudgeIngress(
		WithAuthenticatedNudgeRequester(t.Context(), localAuthorityRequester()),
		request,
	)
	if !errors.Is(err, ErrLocalNudgeAuthorityConflict) {
		t.Fatalf("AuthorizeNudgeIngress after finalized rejection = %#v, err=%v; want conflict", authorization, err)
	}
	if _, found, err := authority.grantByRequestID(t.Context(), request.RequestID); err != nil || found {
		t.Fatalf("grant after finalized rejection found=%t err=%v, want absent", found, err)
	}
	if prepared, err := localAuthorityAdmissionPreparationExists(t.Context(), authority.db, forged.ID); err != nil || prepared {
		t.Fatalf("admission preparation after finalized rejection found=%t err=%v, want absent", prepared, err)
	}
}
