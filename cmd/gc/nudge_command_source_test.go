package main

import (
	"context"
	"crypto/sha256"
	"errors"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

func TestOpenProductionNudgeCommandSourceProvisionsButRefusesUnverifiedCityPartition(t *testing.T) {
	store := newNudgeCommandSourceAtomicStore()
	cityPath := t.TempDir()

	first, err := openVerifiedProductionNudgeCommandSource(t.Context(), cityPath, store, nudgequeue.TrustedCityPartition{}, nil)
	if first != nil || !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryPartition) {
		t.Fatalf("first open = %T, err=%v; want unverified partition refusal", first, err)
	}
	if writes := store.metadataWriteCount(); writes != 7 {
		t.Fatalf("initial metadata writes = %d, want 7", writes)
	}
	if _, exists, err := nudgequeue.LoadRestoreAnchor(t.Context(), nudgequeue.RestoreAnchorPath(cityPath)); err != nil || !exists {
		t.Fatalf("independent restore anchor after first open: exists=%t err=%v", exists, err)
	}

	second, err := openVerifiedProductionNudgeCommandSource(t.Context(), cityPath, store, nudgequeue.TrustedCityPartition{}, nil)
	if second != nil || !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryPartition) {
		t.Fatalf("second open = %T, err=%v; want stable unverified partition refusal", second, err)
	}
	if writes := store.metadataWriteCount(); writes != 7 {
		t.Fatalf("unverified reopen metadata writes = %d, want 7", writes)
	}
}

func TestOpenProductionNudgeCommandSourceLeavesUnsupportedStoreLegacyOnly(t *testing.T) {
	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), t.TempDir(), beads.NewMemStore(), nudgequeue.TrustedCityPartition{}, nil)
	if source != nil {
		t.Fatalf("unsupported source = %T, want nil", source)
	}
	if !errors.Is(err, errNudgeCommandSourceUnverified) || !errors.Is(err, nudgequeue.ErrCommandRepositoryUnsupported) {
		t.Fatalf("unsupported error = %v, want unverified + repository unsupported", err)
	}
}

func TestOpenProductionNudgeCommandSourceWrapsKnownTransientProvisionFailure(t *testing.T) {
	store := newNudgeCommandSourceAtomicStore()
	store.failNext = context.DeadlineExceeded

	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), t.TempDir(), store, nudgequeue.TrustedCityPartition{}, nil)
	if source != nil {
		t.Fatalf("transient source = %T, want nil until retry", source)
	}
	var failure nudgeCommandSourceFailure
	if !errors.As(err, &failure) || failure.class != nudgeCommandSourceErrorTransient || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("transient open error = %#v (%v), want retryable deadline failure", failure, err)
	}
}

func TestOpenProductionNudgeCommandSourceUsesSparseAuthorityWithoutGlobalCheckpoint(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	claimed := fixture.claimDirect(t, "claim-before-reopen", "attempt-before-reopen")
	completed, err := fixture.repository.CompleteProviderAttempt(t.Context(), deliveredNudgeCompletion(claimed, fixture.now.Add(3*time.Second)), fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("CompleteProviderAttempt: %v", err)
	}
	if err := fixture.authority.RecordCommandPartitionTerminal(t.Context(), nudgequeue.CommandPartitionTerminal{
		Store: completed.Command.Store, RepositoryRevision: completed.Command.Order.Revision,
		CommandID: completed.Command.ID, Sequence: completed.Command.Order.Sequence, Partition: fixture.partition,
	}); err != nil {
		t.Fatalf("RecordCommandPartitionTerminal: %v", err)
	}
	if _, err := fixture.repository.Snapshot(t.Context(), 1); !errors.Is(err, nudgequeue.ErrCommandRepositoryCheckpointRequired) {
		t.Fatalf("Snapshot before reopen error = %v, want checkpoint-required", err)
	}

	source, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	snapshot, err := source.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot after sparse opener: %v", err)
	}
	if len(snapshot.Entries) != 0 || snapshot.Coverage != nil || len(snapshot.PartitionGaps) != 0 {
		t.Fatalf("sparse snapshot after terminal = %#v, want empty authority-proven partition", snapshot)
	}
}

func TestProductionNudgeCommandSourceInjectsBoundPartitionAndMaintainsMembership(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source, ok := opened.(*productionNudgeCommandSource)
	if !ok {
		t.Fatalf("opened source = %T, want *productionNudgeCommandSource", opened)
	}
	claimRequest := nudgeEffectClaimRequest{
		commandID:           fixture.command.ID,
		claimID:             "claim-through-bound-source",
		ownerID:             "owner-through-bound-source",
		attemptID:           "attempt-through-bound-source",
		boundLaunchIdentity: "production-launch",
		claimedAt:           fixture.now.Add(2 * time.Second),
		leaseUntil:          fixture.now.Add(time.Minute),
	}
	claim, err := source.ClaimAuthorized(t.Context(), claimRequest, fixture.authority)
	if err != nil || claim.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claim, err)
	}
	if got := fixture.authority.lastClaimPartition(); got != fixture.partition {
		t.Fatalf("claim partition = %#v, want opener-bound partition", got)
	}
	if _, err := source.CompleteProviderAttempt(t.Context(), deliveredNudgeCompletion(claim.Command, fixture.now.Add(3*time.Second))); err != nil {
		t.Fatalf("CompleteProviderAttempt: %v", err)
	}
	if _, err := source.Snapshot(t.Context(), 1); err != nil {
		t.Fatalf("Snapshot after terminal membership publication: %v", err)
	}
}

func TestProductionNudgeCommandSourceClaimTerminalMaintainsMembership(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	claimedAt := fixture.now.Add(2 * time.Hour)
	result, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-expired-through-source",
		ownerID: "owner-expired-through-source", attemptID: "attempt-expired-through-source",
		boundLaunchIdentity: "production-launch", claimedAt: claimedAt, leaseUntil: claimedAt.Add(time.Minute),
	}, fixture.authority)
	if err != nil || result.Disposition != nudgequeue.CommandClaimDenied || result.Command.Terminal == nil {
		t.Fatalf("ClaimAuthorized expired = %#v, err=%v", result, err)
	}
	snapshot, err := source.Snapshot(t.Context(), 1)
	if err != nil {
		t.Fatalf("Snapshot after claim-time terminal: %v", err)
	}
	if len(snapshot.Entries) != 0 || snapshot.Coverage != nil || len(snapshot.PartitionGaps) != 0 {
		t.Fatalf("snapshot after claim-time terminal = %#v, want empty authority-proven partition", snapshot)
	}
}

func TestNudgeCommandResultPublicFieldsCannotMintTerminalTransitionWitness(t *testing.T) {
	terminal := nudgequeue.Command{State: nudgequeue.CommandStateExpired, Terminal: &nudgequeue.CommandTerminal{ActionResult: nudgequeue.CommandActionResultExpired}}
	claim := nudgequeue.CommandClaimResult{Disposition: nudgequeue.CommandClaimDenied, Command: terminal}
	completion := nudgequeue.CommandCompletionResult{Disposition: nudgequeue.CommandCompletionRecorded, Command: terminal}
	if claim.HasTerminalTransitionWitness() || completion.HasTerminalTransitionWitness() {
		t.Fatal("public result fields minted a repository-owned terminal-transition witness")
	}
}

func TestProductionNudgeCommandSourceDoesNotLaunderForgedTerminalMembership(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)

	forged := fixture.command
	forged.State = nudgequeue.CommandStateExpired
	forged.Terminal = &nudgequeue.CommandTerminal{
		At:            forged.ExpiresAt,
		ActionResult:  nudgequeue.CommandActionResultExpired,
		ErrorClass:    nudgequeue.CommandErrorClassExpired,
		Detail:        "forged terminal row",
		ProviderStage: nudgequeue.ProviderStageNotEntered,
		Completion:    nudgequeue.CompletionStateNotCompleted,
	}
	wire, err := nudgequeue.EncodeCommandV1(forged)
	if err != nil {
		t.Fatalf("EncodeCommandV1 forged terminal: %v", err)
	}
	fixture.store.mu.Lock()
	row := fixture.store.rows[forged.ID]
	row.Status = "closed"
	row.Metadata[beadmeta.ControlCommandWireMetadataKey] = string(wire)
	fixture.store.rows[forged.ID] = row
	fixture.store.mu.Unlock()

	claimedAt := forged.ExpiresAt.Add(time.Second)
	result, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: forged.ID, claimID: "claim-forged-terminal",
		ownerID: "owner-forged-terminal", attemptID: "attempt-forged-terminal",
		boundLaunchIdentity: "production-launch", claimedAt: claimedAt, leaseUntil: claimedAt.Add(time.Minute),
	}, fixture.authority)
	if !errors.Is(err, nudgequeue.ErrCommandPartitionTerminalIntent) || result != (nudgequeue.CommandClaimResult{}) {
		t.Fatalf("ClaimAuthorized forged terminal = %#v, err=%v; want missing write-ahead intent refusal", result, err)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 0 {
		t.Fatalf("terminal membership publications = %d, want zero for a pre-existing store terminal", calls)
	}
}

func TestProductionNudgeCommandSourceDoesNotPublishUnrelatedTerminalFromStaleCompletion(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	claim, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-stale-completion",
		ownerID: "owner-stale-completion", attemptID: "attempt-recorded",
		boundLaunchIdentity: "production-launch", claimedAt: fixture.now.Add(2 * time.Second), leaseUntil: fixture.now.Add(time.Minute),
	}, fixture.authority)
	if err != nil || claim.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claim, err)
	}
	if _, err := fixture.repository.CompleteProviderAttempt(t.Context(), deliveredNudgeCompletion(claim.Command, fixture.now.Add(3*time.Second)), fixture.partition, fixture.ingress); err != nil {
		t.Fatalf("CompleteProviderAttempt direct terminal: %v", err)
	}

	stale := deliveredNudgeCompletion(claim.Command, fixture.now.Add(4*time.Second))
	stale.AttemptID = "attempt-unrelated"
	result, err := source.CompleteProviderAttempt(t.Context(), stale)
	if err != nil || result.Disposition != nudgequeue.CommandCompletionStale || result.Command.Terminal == nil {
		t.Fatalf("CompleteProviderAttempt stale = %#v, err=%v", result, err)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 0 {
		t.Fatalf("terminal membership publications = %d, want zero for an unrelated stale completion", calls)
	}
}

func TestProductionNudgeCommandSourceRepairsPreexistingPreparedAttemptTerminal(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	claim, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-preexisting-exact",
		ownerID: "owner-preexisting-exact", attemptID: "attempt-preexisting-exact",
		boundLaunchIdentity: "production-launch", claimedAt: fixture.now.Add(2 * time.Second), leaseUntil: fixture.now.Add(time.Minute),
	}, fixture.authority)
	if err != nil || claim.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claim, err)
	}
	request := deliveredNudgeCompletion(claim.Command, fixture.now.Add(3*time.Second))
	if _, err := fixture.repository.CompleteProviderAttempt(t.Context(), request, fixture.partition, fixture.ingress); err != nil {
		t.Fatalf("CompleteProviderAttempt direct terminal: %v", err)
	}

	repeated, err := source.CompleteProviderAttempt(t.Context(), request)
	if err != nil || repeated.Disposition != nudgequeue.CommandCompletionAlreadyRecorded || !repeated.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt preexisting exact = %#v, err=%v", repeated, err)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 1 {
		t.Fatalf("terminal membership publications = %d, want one prepared-terminal repair", calls)
	}
	if _, err := source.Get(t.Context(), fixture.command.ID); err != nil {
		t.Fatalf("Get repaired exact terminal: %v", err)
	}
}

func TestProductionNudgeCommandSourcePublishesRecoveredClaimTerminalIdempotently(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	fixture.store.mu.Lock()
	fixture.store.failAfterCommitNext = errors.New("lost claim terminal response")
	fixture.store.mu.Unlock()
	claimedAt := fixture.command.ExpiresAt
	request := nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-recovered-terminal",
		ownerID: "owner-recovered-terminal", attemptID: "attempt-recovered-terminal",
		boundLaunchIdentity: "production-launch", claimedAt: claimedAt, leaseUntil: claimedAt.Add(time.Minute),
	}

	first, err := source.ClaimAuthorized(t.Context(), request, fixture.authority)
	if err != nil || first.Disposition != nudgequeue.CommandClaimDenied || first.Command.Terminal == nil {
		t.Fatalf("ClaimAuthorized recovered terminal = %#v, err=%v", first, err)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 1 {
		t.Fatalf("terminal membership publications after recovery = %d, want one", calls)
	}
	repeated, err := source.ClaimAuthorized(t.Context(), request, fixture.authority)
	if err != nil || repeated.Disposition != nudgequeue.CommandClaimDenied || !reflect.DeepEqual(repeated.Command, first.Command) {
		t.Fatalf("ClaimAuthorized repeated terminal = %#v, err=%v; want %#v", repeated, err, first.Command)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 2 {
		t.Fatalf("terminal membership publication attempts after duplicate = %d, want two", calls)
	}
	if terminals := fixture.authority.terminalMembershipCount(); terminals != 1 {
		t.Fatalf("durable terminal memberships after duplicate = %d, want one", terminals)
	}
	if _, err := source.Snapshot(t.Context(), 1); err != nil {
		t.Fatalf("Snapshot after recovered claim terminal membership: %v", err)
	}
}

func TestProductionNudgeCommandSourceRepairsTerminalPublicationAfterRestart(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	claim, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-terminal-repair",
		ownerID: "owner-terminal-repair", attemptID: "attempt-terminal-repair",
		boundLaunchIdentity: "production-launch", claimedAt: fixture.now.Add(2 * time.Second), leaseUntil: fixture.now.Add(time.Minute),
	}, fixture.authority)
	if err != nil || claim.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claim, err)
	}
	request := deliveredNudgeCompletion(claim.Command, fixture.now.Add(3*time.Second))
	fixture.authority.failNextTerminalRecord(errors.New("terminal authority unavailable after command commit"))

	first, err := source.CompleteProviderAttempt(t.Context(), request)
	if err == nil || first.Command.Terminal == nil {
		t.Fatalf("CompleteProviderAttempt first = %#v, err=%v; want committed terminal plus publication failure", first, err)
	}

	reopened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("reopen production source: %v", err)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 2 {
		t.Fatalf("terminal membership publication attempts after reopen = %d, want failed attempt plus startup repair", calls)
	}
	if _, err := reopened.(*productionNudgeCommandSource).Get(t.Context(), fixture.command.ID); err != nil {
		t.Fatalf("Get after terminal publication repair: %v", err)
	}
	idempotent, err := reopened.(*productionNudgeCommandSource).CompleteProviderAttempt(t.Context(), request)
	if err != nil || idempotent.Disposition != nudgequeue.CommandCompletionAlreadyRecorded || !idempotent.HasTerminalTransitionWitness() {
		t.Fatalf("CompleteProviderAttempt finalized replay = %#v, err=%v", idempotent, err)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 3 {
		t.Fatalf("terminal membership publication attempts after finalized replay = %d, want three", calls)
	}
}

func TestProductionNudgeCommandSourceAbortsRolledBackPreparationOnRestart(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	claim, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-prepare-rollback",
		ownerID: "owner-prepare-rollback", attemptID: "attempt-prepare-rollback",
		boundLaunchIdentity: "production-launch", claimedAt: fixture.now.Add(2 * time.Second), leaseUntil: fixture.now.Add(time.Minute),
	}, fixture.authority)
	if err != nil || claim.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claim, err)
	}
	request := deliveredNudgeCompletion(claim.Command, fixture.now.Add(3*time.Second))
	fixture.authority.failNextPrepareAfterPut(errors.New("lost terminal prepare response"))

	if _, err := source.CompleteProviderAttempt(t.Context(), request); !errors.Is(err, nudgequeue.ErrCommandPartitionTerminalIntent) {
		t.Fatalf("CompleteProviderAttempt prepare error = %v, want terminal-intent uncertainty", err)
	}
	if intents := fixture.authority.terminalIntentCount(); intents != 1 {
		t.Fatalf("terminal intents after lost prepare response = %d, want one", intents)
	}

	reopened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("reopen production source: %v", err)
	}
	if intents := fixture.authority.terminalIntentCount(); intents != 0 {
		t.Fatalf("terminal intents after exact before-state recovery = %d, want zero", intents)
	}
	repaired, err := reopened.(*productionNudgeCommandSource).CompleteProviderAttempt(t.Context(), request)
	if err != nil || repaired.Disposition != nudgequeue.CommandCompletionRecorded {
		t.Fatalf("CompleteProviderAttempt after preparation abort = %#v, err=%v", repaired, err)
	}
}

func TestProductionNudgeCommandSourceRejectsStoreRewriteAfterTerminalFinalization(t *testing.T) {
	fixture := newProductionNudgeCommandFixture(t)
	opened, err := openVerifiedProductionNudgeCommandSource(t.Context(), fixture.cityPath, fixture.store, fixture.partition, fixture.ingress)
	if err != nil {
		t.Fatalf("openVerifiedProductionNudgeCommandSource: %v", err)
	}
	source := opened.(*productionNudgeCommandSource)
	claim, err := source.ClaimAuthorized(t.Context(), nudgeEffectClaimRequest{
		commandID: fixture.command.ID, claimID: "claim-terminal-rewrite",
		ownerID: "owner-terminal-rewrite", attemptID: "attempt-terminal-rewrite",
		boundLaunchIdentity: "production-launch", claimedAt: fixture.now.Add(2 * time.Second), leaseUntil: fixture.now.Add(time.Minute),
	}, fixture.authority)
	if err != nil || claim.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", claim, err)
	}
	request := deliveredNudgeCompletion(claim.Command, fixture.now.Add(3*time.Second))
	completed, err := source.CompleteProviderAttempt(t.Context(), request)
	if err != nil || completed.Command.Terminal == nil {
		t.Fatalf("CompleteProviderAttempt = %#v, err=%v", completed, err)
	}

	tampered := completed.Command
	tampered.Terminal.At = tampered.Terminal.At.Add(time.Nanosecond)
	wire, err := nudgequeue.EncodeCommandV1(tampered)
	if err != nil {
		t.Fatalf("EncodeCommandV1 tampered terminal: %v", err)
	}
	fixture.store.mu.Lock()
	row := fixture.store.rows[tampered.ID]
	row.Metadata[beadmeta.ControlCommandWireMetadataKey] = string(wire)
	fixture.store.rows[tampered.ID] = row
	fixture.store.mu.Unlock()

	result, err := source.CompleteProviderAttempt(t.Context(), request)
	if !errors.Is(err, nudgequeue.ErrCommandPartitionTerminalIntent) || result != (nudgequeue.CommandCompletionResult{}) {
		t.Fatalf("CompleteProviderAttempt after store rewrite = %#v, err=%v; want finalized digest mismatch", result, err)
	}
	if calls := fixture.authority.terminalRecordCount(); calls != 1 {
		t.Fatalf("terminal membership publications after store rewrite = %d, want one original finalization", calls)
	}
}

func TestProductionNudgeCommandSourceSeparatesReadCapabilityFromCityBoundWrites(t *testing.T) {
	typ := reflect.TypeOf(productionNudgeCommandSource{})
	for name, want := range map[string]reflect.Type{
		"repository": reflect.TypeOf((*nudgequeue.CommandRepository)(nil)),
		"reader":     reflect.TypeOf((*nudgequeue.CommandPartitionReader)(nil)),
		"partition":  reflect.TypeOf(nudgequeue.TrustedCityPartition{}),
	} {
		field, ok := typ.FieldByName(name)
		if !ok || field.Type != want {
			t.Errorf("production source %s field = %#v, want %v", name, field, want)
		}
	}
}

func TestProductionNudgeCommandSourceClassifiesOnlyKnownRetryableFailures(t *testing.T) {
	source := &productionNudgeCommandSource{}
	for _, err := range []error{context.DeadlineExceeded, nudgequeue.ErrRestoreAnchorBusy, nudgequeue.ErrRestoreAnchorConflict, nudgequeue.ErrRestoreAnchorDurabilityUncertain} {
		if got := source.ClassifyNudgeCommandSourceError(err); got != nudgeCommandSourceErrorTransient {
			t.Errorf("ClassifyNudgeCommandSourceError(%v) = %d, want transient", err, got)
		}
	}
	for _, err := range []error{errors.New("unknown"), nudgequeue.ErrCommandRepositoryLineage, nudgequeue.ErrCommandRepositorySchemaSkew, nudgequeue.ErrCommandRepositoryRecord} {
		if got := source.ClassifyNudgeCommandSourceError(err); got != nudgeCommandSourceErrorInvariant {
			t.Errorf("ClassifyNudgeCommandSourceError(%v) = %d, want invariant", err, got)
		}
	}
}

type productionNudgeCommandFixture struct {
	cityPath   string
	store      *nudgeCommandSourceAtomicStore
	repository *nudgequeue.CommandRepository
	authority  *productionNudgeTestAuthority
	ingress    *nudgequeue.TrustedNudgeIngress
	partition  nudgequeue.TrustedCityPartition
	command    nudgequeue.Command
	now        time.Time
}

func newProductionNudgeCommandFixture(t *testing.T) productionNudgeCommandFixture {
	t.Helper()
	cityPath := t.TempDir()
	store := newNudgeCommandSourceAtomicStore()
	repository, err := nudgequeue.NewCommandRepository(store, nudgequeue.NewRestoreAnchorRepositoryVerifier(cityPath))
	if err != nil {
		t.Fatalf("NewCommandRepository: %v", err)
	}
	if _, err := repository.Provision(t.Context()); err != nil {
		t.Fatalf("Provision: %v", err)
	}
	authority := &productionNudgeTestAuthority{
		references:        make(map[string]nudgequeue.NudgeAuthorization),
		admissions:        make(map[string]nudgequeue.CommandPartitionAdmission),
		terminals:         make(map[string]nudgequeue.CommandPartitionTerminal),
		intents:           make(map[nudgequeue.CommandPartitionTerminalIntent]struct{}),
		finalized:         make(map[string]nudgequeue.CommandPartitionTerminalResolution),
		claimPreparations: make(map[string]nudgequeue.CommandClaimTransitionIntent),
		claimReceipts:     make(map[string]nudgequeue.CommandClaimTransitionReceipt),
	}
	ingress, err := nudgequeue.NewTrustedNudgeIngress(repository, authority)
	if err != nil {
		t.Fatalf("NewTrustedNudgeIngress: %v", err)
	}
	now := time.Now().UTC()
	admitted, err := ingress.Admit(t.Context(), nudgequeue.NudgeIngressRequest{
		RequestID: "production-source-request",
		Mode:      nudgequeue.DeliveryModeQueue,
		Target: nudgequeue.CommandTarget{
			SessionID:            "production-session",
			IntentGeneration:     1,
			ContinuationIdentity: "production-continuation",
			Policy:               nudgequeue.TargetPolicyContinuation,
		},
		Source:       nudgequeue.CommandSourceSession,
		Message:      "production adapter proof",
		DeliverAfter: now.Add(time.Second),
		ExpiresAt:    now.Add(time.Hour),
	})
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	return productionNudgeCommandFixture{
		cityPath: cityPath, store: store, repository: repository, authority: authority,
		ingress: ingress, partition: admitted.Partition, command: *admitted.Entry.Command, now: now,
	}
}

func (f productionNudgeCommandFixture) claimDirect(t *testing.T, claimID, attemptID string) nudgequeue.Command {
	t.Helper()
	result, err := f.repository.ClaimAuthorized(t.Context(), nudgequeue.CommandClaimRequest{
		CommandID: f.command.ID, ClaimID: claimID, OwnerID: "direct-owner", AttemptID: attemptID,
		BoundLaunchIdentity: "production-launch", Partition: f.partition,
		ClaimedAt: f.now.Add(2 * time.Second), LeaseUntil: f.now.Add(time.Minute),
	}, f.authority, f.ingress)
	if err != nil || result.Disposition != nudgequeue.CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", result, err)
	}
	return result.Command
}

func deliveredNudgeCompletion(command nudgequeue.Command, completedAt time.Time) nudgequeue.CommandCompletionRequest {
	return nudgequeue.CommandCompletionRequest{
		CommandID: command.ID, ClaimID: command.Claim.ID, OperationID: command.Claim.OperationID,
		AttemptID: command.Claim.AttemptID, CompletedAt: completedAt,
		ActionResult:  nudgequeue.CommandActionResultDelivered,
		ProviderStage: nudgequeue.ProviderStageAccepted, Completion: nudgequeue.CompletionStateCompleted,
	}
}

type productionNudgeTestAuthority struct {
	mu                  sync.Mutex
	references          map[string]nudgequeue.NudgeAuthorization
	partition           nudgequeue.TrustedCityPartition
	admissions          map[string]nudgequeue.CommandPartitionAdmission
	terminals           map[string]nudgequeue.CommandPartitionTerminal
	intents             map[nudgequeue.CommandPartitionTerminalIntent]struct{}
	finalized           map[string]nudgequeue.CommandPartitionTerminalResolution
	claimPreparations   map[string]nudgequeue.CommandClaimTransitionIntent
	claimReceipts       map[string]nudgequeue.CommandClaimTransitionReceipt
	terminalRecordCalls int
	failTerminalRecord  error
	failPrepareAfterPut error
}

func (a *productionNudgeTestAuthority) AuthorizeNudgeIngress(_ context.Context, request nudgequeue.NudgeIngressAuthorizationRequest) (nudgequeue.NudgeAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	authorization := nudgequeue.NudgeAuthorization{
		Disposition: nudgequeue.NudgeAuthorizationAllowed, PrincipalSchemaVersion: nudgequeue.NudgePrincipalSchemaVersion,
		CommandCreatedAt: request.RequestedAt,
		Reference: nudgequeue.TrustedIngressReference{
			Issuer: "production-test-authority", ReferenceID: "authority/" + request.RequestID,
			PrincipalID: "principal-1", TenantScope: "tenant-1", CityScope: "city-1",
			CredentialClass: "controller", PolicyVersion: "policy-v1", PolicyDecisionID: "ingress-decision-1",
			Action: request.Action, TargetSessionID: request.Target.SessionID, PayloadDigest: request.PayloadDigest,
			IssuedAt: request.RequestedAt.Add(-time.Second), ExpiresAt: request.RequestedAt.Add(time.Hour),
		},
	}
	a.references[authorization.Reference.ReferenceID] = authorization
	return authorization, nil
}

func (a *productionNudgeTestAuthority) ResolveTrustedNudgeIngress(_ context.Context, reference nudgequeue.TrustedIngressReference) (nudgequeue.NudgeAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.references[reference.ReferenceID], nil
}

func (a *productionNudgeTestAuthority) RecordCommandPartitionAdmission(_ context.Context, admission nudgequeue.CommandPartitionAdmission) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, found := a.admissions[admission.CommandID]; found && existing != admission {
		return errors.New("conflicting production-test partition admission")
	}
	a.admissions[admission.CommandID] = admission
	return nil
}

func (a *productionNudgeTestAuthority) RecordCommandPartitionTerminal(_ context.Context, terminal nudgequeue.CommandPartitionTerminal) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.terminalRecordCalls++
	if a.failTerminalRecord != nil {
		err := a.failTerminalRecord
		a.failTerminalRecord = nil
		return err
	}
	admission, found := a.admissions[terminal.CommandID]
	if !found || admission.Store != terminal.Store || admission.Sequence != terminal.Sequence || admission.Partition != terminal.Partition {
		return errors.New("terminal has no matching production-test partition admission")
	}
	var prepared *nudgequeue.CommandPartitionTerminalIntent
	for intent := range a.intents {
		if intent.Store == terminal.Store && intent.RepositoryRevision == terminal.RepositoryRevision &&
			intent.CommandID == terminal.CommandID && intent.Sequence == terminal.Sequence && intent.Partition == terminal.Partition {
			preparedIntent := intent
			prepared = &preparedIntent
			break
		}
	}
	if prepared == nil {
		finalized, found := a.finalized[terminal.CommandID]
		if !found || finalized.Store != terminal.Store || finalized.RepositoryRevision != terminal.RepositoryRevision ||
			finalized.Sequence != terminal.Sequence || finalized.Partition != terminal.Partition {
			return errors.New("terminal has no matching production-test write-ahead intent")
		}
	} else {
		a.finalized[terminal.CommandID] = nudgequeue.CommandPartitionTerminalResolution{
			Store: prepared.Store, RepositoryRevision: prepared.RepositoryRevision, CommandID: prepared.CommandID,
			Sequence: prepared.Sequence, Partition: prepared.Partition, CommandDigest: prepared.CommandDigest,
		}
		delete(a.intents, *prepared)
	}
	if existing, found := a.terminals[terminal.CommandID]; found && existing != terminal {
		return errors.New("conflicting production-test partition terminal")
	}
	a.terminals[terminal.CommandID] = terminal
	return nil
}

func (a *productionNudgeTestAuthority) PrepareCommandPartitionTerminal(_ context.Context, intent nudgequeue.CommandPartitionTerminalIntent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	admission, found := a.admissions[intent.CommandID]
	if !found || admission.Store != intent.Store || admission.Sequence != intent.Sequence || admission.Partition != intent.Partition ||
		intent.RepositoryRevision <= admission.RepositoryRevision {
		return errors.New("terminal intent has no matching production-test partition admission")
	}
	for existing := range a.intents {
		if existing.CommandID == intent.CommandID {
			if existing == intent {
				return nil
			}
			return errors.New("conflicting production-test terminal intent")
		}
	}
	a.intents[intent] = struct{}{}
	if a.failPrepareAfterPut != nil {
		err := a.failPrepareAfterPut
		a.failPrepareAfterPut = nil
		return err
	}
	return nil
}

func (a *productionNudgeTestAuthority) ReleaseCommandPartitionTerminalWriter(_ context.Context, _ nudgequeue.CommandPartitionTerminalIntent) error {
	return nil
}

func (a *productionNudgeTestAuthority) AbortCommandPartitionTerminal(_ context.Context, intent nudgequeue.CommandPartitionTerminalIntent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, found := a.intents[intent]; found {
		delete(a.intents, intent)
		return nil
	}
	if _, finalized := a.finalized[intent.CommandID]; finalized {
		return errors.New("production-test terminal intent is already finalized")
	}
	return nil
}

func (a *productionNudgeTestAuthority) VerifyCommandPartitionTerminal(_ context.Context, resolution nudgequeue.CommandPartitionTerminalResolution) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for intent := range a.intents {
		if intent.Store == resolution.Store && intent.RepositoryRevision == resolution.RepositoryRevision &&
			intent.CommandID == resolution.CommandID && intent.Sequence == resolution.Sequence &&
			intent.Partition == resolution.Partition && intent.CommandDigest == resolution.CommandDigest {
			return nil
		}
	}
	if finalized, found := a.finalized[resolution.CommandID]; !found || finalized != resolution {
		return errors.New("production-test terminal intent or finalized digest is missing")
	}
	return nil
}

func (a *productionNudgeTestAuthority) PrepareCommandClaimTransition(ctx context.Context, intent nudgequeue.CommandClaimTransitionIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, finalized := a.claimReceipts[intent.CommandID]; finalized {
		return errors.New("production-test claim transition is already finalized")
	}
	if existing, found := a.claimPreparations[intent.CommandID]; found {
		if !sameProductionNudgeClaimTransitionIntent(existing, intent) {
			return errors.New("conflicting production-test claim transition")
		}
		return nil
	}
	if a.claimPreparations == nil {
		a.claimPreparations = make(map[string]nudgequeue.CommandClaimTransitionIntent)
	}
	a.claimPreparations[intent.CommandID] = intent
	return nil
}

func (a *productionNudgeTestAuthority) ReleaseCommandClaimTransitionWriter(ctx context.Context, intent nudgequeue.CommandClaimTransitionIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, found := a.claimPreparations[intent.CommandID]; found && !sameProductionNudgeClaimTransitionIntent(existing, intent) {
		return errors.New("conflicting production-test claim transition writer release")
	}
	if receipt, finalized := a.claimReceipts[intent.CommandID]; finalized && !productionNudgeClaimIntentMatchesReceipt(intent, receipt) {
		return errors.New("production-test finalized claim receipt differs from writer release")
	}
	return nil
}

func (a *productionNudgeTestAuthority) AbortCommandClaimTransition(ctx context.Context, intent nudgequeue.CommandClaimTransitionIntent) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, found := a.claimPreparations[intent.CommandID]; found {
		if !sameProductionNudgeClaimTransitionIntent(existing, intent) {
			return errors.New("conflicting production-test claim transition abort")
		}
		delete(a.claimPreparations, intent.CommandID)
		return nil
	}
	if _, finalized := a.claimReceipts[intent.CommandID]; finalized {
		return errors.New("production-test claim transition is already finalized")
	}
	return nil
}

func (a *productionNudgeTestAuthority) FinalizeCommandClaimTransition(ctx context.Context, receipt nudgequeue.CommandClaimTransitionReceipt) (nudgequeue.CommandClaimReceiptDisposition, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if existing, found := a.claimReceipts[receipt.CommandID]; found {
		if !sameProductionNudgeClaimTransitionReceipt(existing, receipt) {
			return "", errors.New("conflicting production-test claim receipt")
		}
		return nudgequeue.CommandClaimReceiptAlreadyFinalized, nil
	}
	intent, found := a.claimPreparations[receipt.CommandID]
	if !found || !productionNudgeClaimIntentMatchesReceipt(intent, receipt) {
		return "", errors.New("production-test claim receipt has no exact preparation")
	}
	delete(a.claimPreparations, receipt.CommandID)
	if a.claimReceipts == nil {
		a.claimReceipts = make(map[string]nudgequeue.CommandClaimTransitionReceipt)
	}
	a.claimReceipts[receipt.CommandID] = receipt
	return nudgequeue.CommandClaimReceiptFinalized, nil
}

func sameProductionNudgeClaimTransitionIntent(left, right nudgequeue.CommandClaimTransitionIntent) bool {
	return left.Store == right.Store &&
		left.RepositoryBeforeRevision == right.RepositoryBeforeRevision &&
		left.RepositoryRevision == right.RepositoryRevision &&
		left.RepositorySequenceHighWater == right.RepositorySequenceHighWater &&
		left.CommandID == right.CommandID && left.Sequence == right.Sequence && left.Partition == right.Partition &&
		left.BeforeCommandDigest == right.BeforeCommandDigest && left.AfterCommandDigest == right.AfterCommandDigest &&
		sameProductionNudgeCommandClaim(left.Claim, right.Claim)
}

func productionNudgeClaimIntentMatchesReceipt(intent nudgequeue.CommandClaimTransitionIntent, receipt nudgequeue.CommandClaimTransitionReceipt) bool {
	return intent.Store == receipt.Store && intent.RepositoryRevision == receipt.RepositoryRevision &&
		intent.CommandID == receipt.CommandID && intent.Sequence == receipt.Sequence && intent.Partition == receipt.Partition &&
		intent.AfterCommandDigest == receipt.AfterCommandDigest && sameProductionNudgeCommandClaim(intent.Claim, receipt.Claim) &&
		receipt.EffectRepositoryRevision >= intent.RepositoryRevision &&
		receipt.EffectSequenceHighWater >= intent.RepositorySequenceHighWater
}

func sameProductionNudgeClaimTransitionReceipt(left, right nudgequeue.CommandClaimTransitionReceipt) bool {
	return left.Store == right.Store && left.RepositoryRevision == right.RepositoryRevision &&
		left.CommandID == right.CommandID && left.Sequence == right.Sequence && left.Partition == right.Partition &&
		left.AfterCommandDigest == right.AfterCommandDigest && sameProductionNudgeCommandClaim(left.Claim, right.Claim) &&
		left.EffectRepositoryRevision == right.EffectRepositoryRevision &&
		left.EffectSequenceHighWater == right.EffectSequenceHighWater
}

func sameProductionNudgeCommandClaim(left, right nudgequeue.CommandClaim) bool {
	return left.ID == right.ID && left.OwnerID == right.OwnerID && left.OperationID == right.OperationID &&
		left.AttemptID == right.AttemptID && left.BoundLaunchIdentity == right.BoundLaunchIdentity &&
		left.AuthorizationDecisionID == right.AuthorizationDecisionID &&
		left.AuthorizationPolicyVersion == right.AuthorizationPolicyVersion &&
		left.ClaimedAt.Equal(right.ClaimedAt) && left.LeaseUntil.Equal(right.LeaseUntil)
}

func (a *productionNudgeTestAuthority) failNextTerminalRecord(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failTerminalRecord = err
}

func (a *productionNudgeTestAuthority) failNextPrepareAfterPut(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.failPrepareAfterPut = err
}

func (a *productionNudgeTestAuthority) terminalRecordCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.terminalRecordCalls
}

func (a *productionNudgeTestAuthority) terminalMembershipCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.terminals)
}

func (a *productionNudgeTestAuthority) terminalIntentCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.intents)
}

func (a *productionNudgeTestAuthority) RepairCommandPartitionTerminals(ctx context.Context, reader nudgequeue.CommandPartitionRecoveryReader) error {
	state, err := reader.State(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	intents := make([]nudgequeue.CommandPartitionTerminalIntent, 0, len(a.intents))
	for intent := range a.intents {
		intents = append(intents, intent)
	}
	a.mu.Unlock()
	for _, intent := range intents {
		resolution, err := reader.Get(ctx, intent.CommandID)
		if err != nil || !resolution.Found || resolution.Entry.Command == nil {
			return errors.New("production-test terminal recovery command is unavailable")
		}
		command := *resolution.Entry.Command
		wire, err := nudgequeue.EncodeCommandV1(command)
		if err != nil {
			return err
		}
		digest := sha256.Sum256(wire)
		if command.Terminal != nil {
			if command.Store != intent.Store || command.Order.Revision != intent.RepositoryRevision || command.ID != intent.CommandID ||
				command.Order.Sequence != intent.Sequence || digest != intent.CommandDigest || state.Revision < intent.RepositoryRevision {
				return errors.New("production-test terminal recovery after-state differs")
			}
			if err := a.RecordCommandPartitionTerminal(ctx, nudgequeue.CommandPartitionTerminal{
				Store: intent.Store, RepositoryRevision: intent.RepositoryRevision, CommandID: intent.CommandID,
				Sequence: intent.Sequence, Partition: intent.Partition,
			}); err != nil {
				return err
			}
			continue
		}
		if command.Store != intent.Store || command.ID != intent.CommandID || command.Order.Sequence != intent.Sequence ||
			digest != intent.BeforeCommandDigest || state.Revision != intent.RepositoryBeforeRevision {
			return errors.New("production-test terminal recovery before-state is not safely abortable")
		}
		if err := a.AbortCommandPartitionTerminal(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}

func (a *productionNudgeTestAuthority) RepairCommandPartitionAdmissions(context.Context, nudgequeue.CommandPartitionRecoveryReader) error {
	return nil
}

func (a *productionNudgeTestAuthority) ResolveCommandPartitionCoverage(_ context.Context, request nudgequeue.CommandPartitionCoverageRequest) (nudgequeue.CommandPartitionCoverage, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	var (
		highWater     uint64
		admittedCount uint64
		active        []nudgequeue.CommandPartitionCoverageEntry
	)
	for id, admission := range a.admissions {
		if admission.Store != request.Store || admission.RepositoryRevision > request.RepositoryRevision {
			continue
		}
		admittedCount++
		highWater = max(highWater, admission.Sequence)
		terminal, closed := a.terminals[id]
		if closed && terminal.RepositoryRevision <= request.RepositoryRevision {
			continue
		}
		if admission.Partition == request.Partition {
			active = append(active, nudgequeue.CommandPartitionCoverageEntry{CommandID: id, Sequence: admission.Sequence})
		}
	}
	sort.Slice(active, func(i, j int) bool { return active[i].Sequence < active[j].Sequence })
	if highWater != request.SequenceHighWater || admittedCount != request.SequenceHighWater {
		return nudgequeue.CommandPartitionCoverage{}, errors.New("production-test authority coverage differs from repository snapshot")
	}
	return nudgequeue.CommandPartitionCoverage{
		Store: request.Store, RepositoryRevision: request.RepositoryRevision, SequenceHighWater: request.SequenceHighWater, DecidedCount: admittedCount,
		Partition: request.Partition, ActiveEntries: active,
	}, nil
}

func (a *productionNudgeTestAuthority) ResolveCommandPartitionMembership(_ context.Context, request nudgequeue.CommandPartitionMembershipRequest) (nudgequeue.CommandPartitionMembership, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	result := nudgequeue.CommandPartitionMembership{
		Store: request.Store, RepositoryRevision: request.RepositoryRevision, SequenceHighWater: request.SequenceHighWater,
		CommandID: request.CommandID, Partition: request.Partition,
	}
	admission, found := a.admissions[request.CommandID]
	if !found || admission.Store != request.Store || admission.RepositoryRevision > request.RepositoryRevision || admission.Partition != request.Partition {
		return result, nil
	}
	result.Found = true
	result.Sequence = admission.Sequence
	terminal, closed := a.terminals[request.CommandID]
	result.Active = !closed || terminal.RepositoryRevision > request.RepositoryRevision
	return result, nil
}

func (a *productionNudgeTestAuthority) AuthorizeNudgeClaim(_ context.Context, request nudgequeue.NudgeClaimAuthorizationRequest) (nudgequeue.NudgeClaimAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.partition = request.Partition
	return nudgequeue.NudgeClaimAuthorization{
		Disposition: nudgequeue.NudgeAuthorizationAllowed, PrincipalSchemaVersion: nudgequeue.NudgePrincipalSchemaVersion,
		DecisionID: "claim-decision-1", PolicyVersion: "policy-v2", Reference: request.Command.TrustedIngress,
	}, nil
}

func (a *productionNudgeTestAuthority) lastClaimPartition() nudgequeue.TrustedCityPartition {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.partition
}

type nudgeCommandSourceAtomicStore struct {
	beads.Store

	mu                  sync.Mutex
	metadata            map[string]string
	rows                map[string]beads.Bead
	metadataWrites      int
	failNext            error
	failAfterCommitNext error
}

func newNudgeCommandSourceAtomicStore() *nudgeCommandSourceAtomicStore {
	return &nudgeCommandSourceAtomicStore{
		Store:    beads.NewMemStore(),
		metadata: make(map[string]string),
		rows:     make(map[string]beads.Bead),
	}
}

func (s *nudgeCommandSourceAtomicStore) AtomicReadWrite(ctx context.Context, _ string, fn func(beads.AtomicReadWriteTx) error) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failNext != nil {
		err := s.failNext
		s.failNext = nil
		return err
	}
	tx := &nudgeCommandSourceAtomicTx{
		metadata:       cloneNudgeCommandSourceStrings(s.metadata),
		rows:           cloneNudgeCommandSourceRows(s.rows),
		metadataWrites: s.metadataWrites,
	}
	if err := fn(tx); err != nil {
		return err
	}
	s.metadata = tx.metadata
	s.rows = tx.rows
	s.metadataWrites = tx.metadataWrites
	if s.failAfterCommitNext != nil {
		err := s.failAfterCommitNext
		s.failAfterCommitNext = nil
		return err
	}
	return nil
}

func (s *nudgeCommandSourceAtomicStore) AtomicReadSnapshot(ctx context.Context, fn func(beads.AtomicReadSnapshotTx) error) error {
	if ctx == nil {
		return errors.New("nil context")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return fn(&nudgeCommandSourceAtomicTx{
		metadata: cloneNudgeCommandSourceStrings(s.metadata),
		rows:     cloneNudgeCommandSourceRows(s.rows),
	})
}

func (s *nudgeCommandSourceAtomicStore) metadataWriteCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.metadataWrites
}

type nudgeCommandSourceAtomicTx struct {
	metadata       map[string]string
	rows           map[string]beads.Bead
	metadataWrites int
}

func (tx *nudgeCommandSourceAtomicTx) GetIssue(id string) (beads.Bead, error) {
	row, ok := tx.rows[id]
	if !ok {
		return beads.Bead{}, beads.ErrNotFound
	}
	return cloneNudgeCommandSourceRow(row), nil
}

func (tx *nudgeCommandSourceAtomicTx) ListHistory(query beads.AtomicReadWriteList) ([]beads.Bead, error) {
	ids := make(map[string]struct{}, len(query.IDs))
	for _, id := range query.IDs {
		ids[id] = struct{}{}
	}
	var rows []beads.Bead
	for _, row := range tx.rows {
		if len(ids) > 0 {
			if _, ok := ids[row.ID]; !ok {
				continue
			}
		}
		if query.IDPrefix != "" && !strings.HasPrefix(row.ID, query.IDPrefix) {
			continue
		}
		if query.IssueType != "" && row.Type != query.IssueType {
			continue
		}
		matches := true
		for key, value := range query.Metadata {
			if row.Metadata[key] != value {
				matches = false
				break
			}
		}
		if matches {
			rows = append(rows, cloneNudgeCommandSourceRow(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].ID < rows[j].ID })
	if len(rows) > query.Limit {
		rows = rows[:query.Limit]
	}
	return rows, nil
}

func (tx *nudgeCommandSourceAtomicTx) ListHistoryPage(query beads.AtomicReadSnapshotPageQuery) (beads.AtomicReadSnapshotPage, error) {
	rows := make([]beads.Bead, 0, len(tx.rows))
	for _, row := range tx.rows {
		if row.Status != query.Status || !strings.HasPrefix(row.ID, query.IDPrefix) || query.Assignee != "" && row.Assignee != query.Assignee {
			continue
		}
		after := query.After == (beads.AtomicReadSnapshotCursor{})
		switch query.Order {
		case beads.AtomicReadSnapshotOrderID:
			after = after || row.ID > query.After.ID
		case beads.AtomicReadSnapshotOrderUpdatedAtID:
			after = after || row.UpdatedAt.After(query.After.UpdatedAt) ||
				(row.UpdatedAt.Equal(query.After.UpdatedAt) && row.ID > query.After.ID)
		default:
			return beads.AtomicReadSnapshotPage{}, errors.New("unsupported snapshot order")
		}
		if after {
			rows = append(rows, cloneNudgeCommandSourceRow(row))
		}
	}
	sort.Slice(rows, func(i, j int) bool {
		if query.Order == beads.AtomicReadSnapshotOrderUpdatedAtID && !rows[i].UpdatedAt.Equal(rows[j].UpdatedAt) {
			return rows[i].UpdatedAt.Before(rows[j].UpdatedAt)
		}
		return rows[i].ID < rows[j].ID
	})
	page := beads.AtomicReadSnapshotPage{}
	if len(rows) <= query.Limit {
		page.Rows = rows
		return page, nil
	}
	page.Rows = rows[:query.Limit]
	last := page.Rows[len(page.Rows)-1]
	page.Next.ID = last.ID
	if query.Order == beads.AtomicReadSnapshotOrderUpdatedAtID {
		page.Next.UpdatedAt = last.UpdatedAt
	}
	return page, nil
}

func (tx *nudgeCommandSourceAtomicTx) Create(row beads.Bead) (beads.Bead, error) {
	if _, exists := tx.rows[row.ID]; exists {
		return beads.Bead{}, errors.New("duplicate row")
	}
	if row.CreatedAt.IsZero() {
		row.CreatedAt = time.Now().UTC()
	}
	row.UpdatedAt = row.CreatedAt
	tx.rows[row.ID] = cloneNudgeCommandSourceRow(row)
	return cloneNudgeCommandSourceRow(row), nil
}

func (tx *nudgeCommandSourceAtomicTx) Update(id string, opts beads.UpdateOpts) error {
	row, ok := tx.rows[id]
	if !ok {
		return beads.ErrNotFound
	}
	if opts.Status != nil {
		row.Status = *opts.Status
	}
	if opts.Metadata != nil {
		if row.Metadata == nil {
			row.Metadata = make(map[string]string)
		}
		for key, value := range opts.Metadata {
			row.Metadata[key] = value
		}
	}
	row.UpdatedAt = time.Now().UTC()
	tx.rows[id] = row
	return nil
}

func (tx *nudgeCommandSourceAtomicTx) GetMetadata(key string) (string, error) {
	return tx.metadata[key], nil
}

func (tx *nudgeCommandSourceAtomicTx) SetMetadata(key, value string) error {
	tx.metadata[key] = value
	tx.metadataWrites++
	return nil
}

func cloneNudgeCommandSourceStrings(source map[string]string) map[string]string {
	result := make(map[string]string, len(source))
	for key, value := range source {
		result[key] = value
	}
	return result
}

func cloneNudgeCommandSourceRows(source map[string]beads.Bead) map[string]beads.Bead {
	result := make(map[string]beads.Bead, len(source))
	for id, row := range source {
		result[id] = cloneNudgeCommandSourceRow(row)
	}
	return result
}

func cloneNudgeCommandSourceRow(row beads.Bead) beads.Bead {
	row.Metadata = cloneNudgeCommandSourceStrings(row.Metadata)
	return row
}
