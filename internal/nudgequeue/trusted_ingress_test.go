package nudgequeue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"reflect"
	"sync"
	"testing"
	"time"
)

func TestNudgeIngressRequestHasNoSelfAssertedAuthorityFields(t *testing.T) {
	typ := reflect.TypeOf(NudgeIngressRequest{})
	forbidden := map[string]struct{}{
		"Issuer": {}, "PrincipalID": {}, "TenantScope": {}, "CityScope": {},
		"CredentialClass": {}, "PolicyVersion": {}, "PolicyDecisionID": {},
		"TrustedIngress": {}, "StoreUUID": {}, "RestoreEpoch": {},
	}
	for i := 0; i < typ.NumField(); i++ {
		if _, found := forbidden[typ.Field(i).Name]; found {
			t.Fatalf("NudgeIngressRequest exposes caller-populatable authority field %q", typ.Field(i).Name)
		}
	}
}

func TestTrustedNudgeIngressStampsNonSelfAssertedAuthorityAndRetriesIdempotently(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(now)

	first, err := ingress.Admit(t.Context(), request)
	if err != nil {
		t.Fatalf("Admit first: %v", err)
	}
	if !first.Created || first.Entry.Command == nil || !first.Partition.valid() {
		t.Fatalf("first admission = %#v, want created command and opaque partition", first)
	}
	command := *first.Entry.Command
	if !command.CreatedAt.Equal(now) || !command.TrustedIngress.IssuedAt.Equal(now.Add(-time.Second)) {
		t.Fatalf("command and authority times = created %v issued %v, want independent %v and %v", command.CreatedAt, command.TrustedIngress.IssuedAt, now, now.Add(-time.Second))
	}
	if command.TrustedIngress.PrincipalID != testNudgePrincipalID ||
		command.TrustedIngress.CityScope != testNudgeCityScope ||
		command.TrustedIngress.PayloadDigest != ComputeCommandPayloadDigest(command) {
		t.Fatalf("trusted ingress = %#v, want authority-stamped exact coverage", command.TrustedIngress)
	}
	resolved, err := ingress.ResolveCommandPartition(t.Context(), command.TrustedIngress)
	if err != nil || resolved != first.Partition {
		t.Fatalf("ResolveCommandPartition = %#v, err=%v; want admission partition", resolved, err)
	}

	second, err := ingress.Admit(t.Context(), request)
	if err != nil {
		t.Fatalf("Admit retry: %v", err)
	}
	if second.Created || second.Entry.Command == nil || !reflect.DeepEqual(second.Entry.Command, first.Entry.Command) || second.Partition != first.Partition {
		t.Fatalf("retry admission = %#v, want existing exact command %#v", second, first)
	}
	if got := authority.authorizeCalls(); got != 1 {
		t.Fatalf("authority ingress calls = %d, want one for idempotent retry", got)
	}
}

func TestTrustedNudgeIngressNormalizesDeliverAtCreationWithAdvancingClock(t *testing.T) {
	requestBuiltAt := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	admittedAt := requestBuiltAt.Add(time.Second)
	for _, mode := range []DeliveryMode{DeliveryModeQueue, DeliveryModeImmediate} {
		t.Run(string(mode), func(t *testing.T) {
			repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
			authority := newTestNudgeAuthority()
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return admittedAt })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			request := validNudgeIngressRequest(requestBuiltAt)
			request.DeliverAfter = time.Time{}
			if mode == DeliveryModeImmediate {
				request.Mode = DeliveryModeImmediate
				request.Target = CommandTarget{SessionID: "session-123", IntentGeneration: 7, LaunchIdentity: "launch-123", Policy: TargetPolicyExactLaunch}
			}

			result, err := ingress.Admit(t.Context(), request)
			if err != nil || !result.Created || result.Entry.Command == nil {
				t.Fatalf("Admit deliver-at-creation = %#v, err=%v", result, err)
			}
			if !result.Entry.Command.CreatedAt.Equal(admittedAt) || !result.Entry.Command.DeliverAfter.Equal(admittedAt) {
				t.Fatalf("effective times = created %v deliver %v, want %v", result.Entry.Command.CreatedAt, result.Entry.Command.DeliverAfter, admittedAt)
			}
			replayed, err := ingress.Admit(t.Context(), request)
			if err != nil || replayed.Created || replayed.Entry.Command == nil || !reflect.DeepEqual(replayed.Entry.Command, result.Entry.Command) {
				t.Fatalf("Admit zero-time replay = %#v, err=%v; want existing %#v", replayed, err, result.Entry.Command)
			}
		})
	}
}

func TestTrustedNudgeIngressRejectsAuthorityCoverageSubstitution(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*NudgeAuthorization){
		"missing command creation time": func(a *NudgeAuthorization) { a.CommandCreatedAt = time.Time{} },
		"action":                        func(a *NudgeAuthorization) { a.Reference.Action = "stop" },
		"target":                        func(a *NudgeAuthorization) { a.Reference.TargetSessionID = "another-session" },
		"payload": func(a *NudgeAuthorization) {
			a.Reference.PayloadDigest = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		},
		"expired": func(a *NudgeAuthorization) { a.Reference.ExpiresAt = now },
	} {
		t.Run(name, func(t *testing.T) {
			repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
			authority := newTestNudgeAuthority()
			authority.mutate = mutate
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			request := validNudgeIngressRequest(now)
			result, err := ingress.Admit(t.Context(), request)
			if !errors.Is(err, ErrNudgeAuthorizationInvalid) {
				t.Fatalf("Admit error = %v, want invalid authority coverage", err)
			}
			if result != (NudgeIngressResult{}) {
				t.Fatalf("Admit result = %#v, want empty", result)
			}
			state, stateErr := repository.State(t.Context())
			if stateErr != nil {
				t.Fatalf("State: %v", stateErr)
			}
			resolution, getErr := repository.Get(t.Context(), CommandIDForRequest(state.Store, request.RequestID))
			if getErr != nil || resolution.Found {
				t.Fatalf("invalid coverage persisted command: %#v, err=%v", resolution, getErr)
			}
		})
	}
}

func TestTrustedNudgeIngressRejectsMalformedCallerPayloadBeforeAuthority(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*NudgeIngressRequest){
		"empty message":            func(request *NudgeIngressRequest) { request.Message = "" },
		"invalid mode and target":  func(request *NudgeIngressRequest) { request.Mode = DeliveryModeImmediate },
		"invalid source reference": func(request *NudgeIngressRequest) { request.Source = CommandSourceMail },
		"invalid delivery window":  func(request *NudgeIngressRequest) { request.ExpiresAt = request.DeliverAfter },
	} {
		t.Run(name, func(t *testing.T) {
			repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
			authority := newTestNudgeAuthority()
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			request := validNudgeIngressRequest(now)
			mutate(&request)

			if _, err := ingress.Admit(t.Context(), request); !errors.Is(err, ErrNudgeAuthorizationInvalid) {
				t.Fatalf("Admit malformed request error = %v, want invalid authorization request", err)
			}
			if got := authority.authorizeCalls(); got != 0 {
				t.Fatalf("authority calls = %d, want zero before caller payload validation", got)
			}
		})
	}
}

func TestTrustedNudgeIngressAcceptsOnlyCurrentAndPreviousPrincipalSchemas(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	for _, schema := range []uint32{NudgePrincipalSchemaVersion, NudgePrincipalSchemaVersion - 1} {
		t.Run("accepted", func(t *testing.T) {
			repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
			authority := newTestNudgeAuthority()
			authority.schema = schema
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			if _, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now)); err != nil {
				t.Fatalf("Admit schema %d: %v", schema, err)
			}
		})
	}
	for _, schema := range []uint32{0, NudgePrincipalSchemaVersion + 1, NudgePrincipalSchemaVersion - 2} {
		t.Run("rejected", func(t *testing.T) {
			repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
			authority := newTestNudgeAuthority()
			authority.schema = schema
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			if _, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now)); !errors.Is(err, ErrNudgeAuthorizationInvalid) {
				t.Fatalf("Admit schema %d error = %v, want schema refusal", schema, err)
			}
		})
	}
}

func TestTrustedNudgeIngressDenialAndOutageNeverPersist(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	for _, disposition := range []NudgeAuthorizationDisposition{NudgeAuthorizationDenied, NudgeAuthorizationUnknown} {
		t.Run(string(disposition), func(t *testing.T) {
			repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
			authority := newTestNudgeAuthority()
			authority.disposition = disposition
			ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
			if err != nil {
				t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
			}
			_, err = ingress.Admit(t.Context(), validNudgeIngressRequest(now))
			want := ErrNudgeAuthorizationDenied
			if disposition == NudgeAuthorizationUnknown {
				want = ErrNudgeAuthorizationUnknown
			}
			if !errors.Is(err, want) {
				t.Fatalf("Admit error = %v, want %v", err, want)
			}
			snapshot, snapErr := repository.Snapshot(t.Context(), 1)
			if snapErr != nil || len(snapshot.Entries) != 0 {
				t.Fatalf("Snapshot after %s = %#v, err=%v; want empty", disposition, snapshot, snapErr)
			}
		})
	}
}

func TestTrustedNudgeIngressResolvesCommitResponseLossWithoutSecondCommand(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	store.mu.Lock()
	store.failAfterCommitNext = errors.New("lost commit response")
	store.mu.Unlock()

	result, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || result.Entry.Command == nil {
		t.Fatalf("Admit after lost response = %#v, err=%v", result, err)
	}
	snapshot, err := repository.Snapshot(t.Context(), 1)
	if err != nil || len(snapshot.Entries) != 1 {
		t.Fatalf("Snapshot = %#v, err=%v; want one command", snapshot, err)
	}
}

const (
	testNudgePrincipalID   = "principal-123"
	testNudgeCityScope     = "tenant-123/city-456"
	testClaimDecisionID    = "claim-decision-current"
	testClaimPolicyVersion = "policy-v2"
)

type testNudgeAuthority struct {
	mu               sync.Mutex
	references       map[string]NudgeAuthorization
	mutate           func(*NudgeAuthorization)
	disposition      NudgeAuthorizationDisposition
	schema           uint32
	calls            int
	claimDisposition NudgeAuthorizationDisposition
	claimSchema      uint32
	claimErr         error
	claimRequests    []NudgeClaimAuthorizationRequest
	coverage         *testCommandPartitionCoverageLedger
	terminalIntents  map[CommandPartitionTerminalIntent]struct{}
	finalized        map[string]CommandPartitionTerminalResolution
}

func newTestNudgeAuthority() *testNudgeAuthority {
	return &testNudgeAuthority{
		references:       make(map[string]NudgeAuthorization),
		disposition:      NudgeAuthorizationAllowed,
		schema:           NudgePrincipalSchemaVersion,
		claimDisposition: NudgeAuthorizationAllowed,
		claimSchema:      NudgePrincipalSchemaVersion,
		coverage:         newTestCommandPartitionCoverageLedger(),
		terminalIntents:  make(map[CommandPartitionTerminalIntent]struct{}),
		finalized:        make(map[string]CommandPartitionTerminalResolution),
	}
}

func (a *testNudgeAuthority) AuthorizeNudgeIngress(_ context.Context, request NudgeIngressAuthorizationRequest) (NudgeAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	authorization := NudgeAuthorization{
		Disposition:            a.disposition,
		PrincipalSchemaVersion: a.schema,
		CommandCreatedAt:       request.RequestedAt,
		Reference: TrustedIngressReference{
			Issuer:           "test-authority",
			ReferenceID:      "authority/" + request.RequestID,
			PrincipalID:      testNudgePrincipalID,
			TenantScope:      "tenant-123",
			CityScope:        testNudgeCityScope,
			CredentialClass:  "test-control-credential",
			PolicyVersion:    "policy-v1",
			PolicyDecisionID: "ingress-decision/" + request.RequestID,
			Action:           request.Action,
			TargetSessionID:  request.Target.SessionID,
			PayloadDigest:    request.PayloadDigest,
			IssuedAt:         request.RequestedAt.Add(-time.Second),
			ExpiresAt:        request.RequestedAt.Add(10 * time.Minute),
		},
	}
	if a.mutate != nil {
		a.mutate(&authorization)
	}
	if authorization.Disposition == NudgeAuthorizationAllowed {
		a.references[authorization.Reference.ReferenceID] = authorization
	}
	return authorization, nil
}

func (a *testNudgeAuthority) ResolveTrustedNudgeIngress(_ context.Context, reference TrustedIngressReference) (NudgeAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	authorization, found := a.references[reference.ReferenceID]
	if !found {
		return NudgeAuthorization{Disposition: NudgeAuthorizationDenied}, nil
	}
	return authorization, nil
}

func (a *testNudgeAuthority) RecordCommandPartitionAdmission(ctx context.Context, admission CommandPartitionAdmission) error {
	return a.coverage.RecordCommandPartitionAdmission(ctx, admission)
}

func (a *testNudgeAuthority) RecordCommandPartitionTerminal(ctx context.Context, terminal CommandPartitionTerminal) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	var prepared *CommandPartitionTerminalIntent
	for intent := range a.terminalIntents {
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
			return errors.New("test terminal has no exact prepared intent")
		}
		return a.coverage.RecordCommandPartitionTerminal(ctx, terminal)
	}
	if err := a.coverage.RecordCommandPartitionTerminal(ctx, terminal); err != nil {
		return err
	}
	a.finalized[terminal.CommandID] = CommandPartitionTerminalResolution{
		Store: prepared.Store, RepositoryRevision: prepared.RepositoryRevision, CommandID: prepared.CommandID,
		Sequence: prepared.Sequence, Partition: prepared.Partition, CommandDigest: prepared.CommandDigest,
	}
	delete(a.terminalIntents, *prepared)
	return nil
}

func (a *testNudgeAuthority) PrepareCommandPartitionTerminal(_ context.Context, intent CommandPartitionTerminalIntent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for existing := range a.terminalIntents {
		if existing.CommandID == intent.CommandID {
			if existing == intent {
				return nil
			}
			return errors.New("conflicting test terminal intent")
		}
	}
	a.terminalIntents[intent] = struct{}{}
	return nil
}

func (a *testNudgeAuthority) AbortCommandPartitionTerminal(_ context.Context, intent CommandPartitionTerminalIntent) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, found := a.terminalIntents[intent]; found {
		delete(a.terminalIntents, intent)
		return nil
	}
	if _, found := a.finalized[intent.CommandID]; found {
		return errors.New("test terminal intent already finalized")
	}
	return nil
}

func (a *testNudgeAuthority) VerifyCommandPartitionTerminal(_ context.Context, resolution CommandPartitionTerminalResolution) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	for intent := range a.terminalIntents {
		if intent.Store == resolution.Store && intent.RepositoryRevision == resolution.RepositoryRevision &&
			intent.CommandID == resolution.CommandID && intent.Sequence == resolution.Sequence &&
			intent.Partition == resolution.Partition && intent.CommandDigest == resolution.CommandDigest {
			return nil
		}
	}
	if finalized, found := a.finalized[resolution.CommandID]; !found || finalized != resolution {
		return errors.New("test terminal intent or finalized digest is missing")
	}
	return nil
}

func (a *testNudgeAuthority) terminalIntentCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.terminalIntents)
}

func (a *testNudgeAuthority) RepairCommandPartitionTerminals(ctx context.Context, reader CommandPartitionRecoveryReader) error {
	state, err := reader.State(ctx)
	if err != nil {
		return err
	}
	a.mu.Lock()
	intents := make([]CommandPartitionTerminalIntent, 0, len(a.terminalIntents))
	for intent := range a.terminalIntents {
		intents = append(intents, intent)
	}
	a.mu.Unlock()
	for _, intent := range intents {
		resolution, err := reader.Get(ctx, intent.CommandID)
		if err != nil || !resolution.Found || resolution.Entry.Command == nil {
			return fmt.Errorf("repairing test terminal intent %q: command unavailable: %w", intent.CommandID, err)
		}
		command := *resolution.Entry.Command
		if command.Terminal != nil && commandIsTerminalState(command.State) {
			after, err := terminalResolutionForCommand(command, intent.Partition)
			if err != nil || after.Store != intent.Store || after.RepositoryRevision != intent.RepositoryRevision ||
				after.CommandID != intent.CommandID || after.Sequence != intent.Sequence || after.CommandDigest != intent.CommandDigest ||
				state.Revision < intent.RepositoryRevision {
				return fmt.Errorf("repairing test terminal intent %q: terminal after-state differs", intent.CommandID)
			}
			if err := a.RecordCommandPartitionTerminal(ctx, CommandPartitionTerminal{
				Store: intent.Store, RepositoryRevision: intent.RepositoryRevision, CommandID: intent.CommandID,
				Sequence: intent.Sequence, Partition: intent.Partition,
			}); err != nil {
				return err
			}
			continue
		}
		wire, err := EncodeCommandV1(command)
		if err != nil || command.Store != intent.Store || command.ID != intent.CommandID || command.Order.Sequence != intent.Sequence ||
			sha256.Sum256(wire) != intent.BeforeCommandDigest || state.Revision != intent.RepositoryBeforeRevision {
			return fmt.Errorf("repairing test terminal intent %q: before-state is not safely abortable", intent.CommandID)
		}
		if err := a.AbortCommandPartitionTerminal(ctx, intent); err != nil {
			return err
		}
	}
	return nil
}

func (a *testNudgeAuthority) RepairCommandPartitionAdmissions(context.Context, CommandPartitionRecoveryReader) error {
	return nil
}

func (a *testNudgeAuthority) ResolveCommandPartitionCoverage(ctx context.Context, request CommandPartitionCoverageRequest) (CommandPartitionCoverage, error) {
	return a.coverage.ResolveCommandPartitionCoverage(ctx, request)
}

func (a *testNudgeAuthority) ResolveCommandPartitionMembership(ctx context.Context, request CommandPartitionMembershipRequest) (CommandPartitionMembership, error) {
	return a.coverage.ResolveCommandPartitionMembership(ctx, request)
}

func (a *testNudgeAuthority) authorizeCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.calls
}

func (a *testNudgeAuthority) AuthorizeNudgeClaim(_ context.Context, request NudgeClaimAuthorizationRequest) (NudgeClaimAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claimRequests = append(a.claimRequests, request)
	if a.claimErr != nil {
		return NudgeClaimAuthorization{}, a.claimErr
	}
	disposition := a.claimDisposition
	stored, found := a.references[request.Command.TrustedIngress.ReferenceID]
	if !found || stored.Reference != request.Command.TrustedIngress {
		disposition = NudgeAuthorizationDenied
	}
	if disposition == NudgeAuthorizationUnknown {
		return NudgeClaimAuthorization{Disposition: disposition}, nil
	}
	return NudgeClaimAuthorization{
		Disposition:            disposition,
		PrincipalSchemaVersion: a.claimSchema,
		DecisionID:             testClaimDecisionID,
		PolicyVersion:          testClaimPolicyVersion,
		Reference:              request.Command.TrustedIngress,
	}, nil
}

func (a *testNudgeAuthority) setClaimDisposition(disposition NudgeAuthorizationDisposition) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claimDisposition = disposition
}

func (a *testNudgeAuthority) setClaimError(err error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claimErr = err
}

func (a *testNudgeAuthority) setClaimSchema(schema uint32) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.claimSchema = schema
}

func (a *testNudgeAuthority) forgetReference(referenceID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.references, referenceID)
}

func (a *testNudgeAuthority) expireReference(referenceID string, expiresAt time.Time) {
	a.mu.Lock()
	defer a.mu.Unlock()
	authorization := a.references[referenceID]
	authorization.Reference.ExpiresAt = expiresAt
	a.references[referenceID] = authorization
}

func (a *testNudgeAuthority) claimCalls() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.claimRequests)
}

func (a *testNudgeAuthority) lastClaimRequest() NudgeClaimAuthorizationRequest {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.claimRequests) == 0 {
		return NudgeClaimAuthorizationRequest{}
	}
	return a.claimRequests[len(a.claimRequests)-1]
}

func validNudgeIngressRequest(now time.Time) NudgeIngressRequest {
	return NudgeIngressRequest{
		RequestID: "request-123",
		Mode:      DeliveryModeQueue,
		Target: CommandTarget{
			SessionID:            "session-123",
			IntentGeneration:     7,
			ContinuationIdentity: "continuation-123",
			Policy:               TargetPolicyContinuation,
		},
		Source:       CommandSourceQueue,
		Message:      "wake up",
		DeliverAfter: now,
		ExpiresAt:    now.Add(5 * time.Minute),
	}
}

var (
	_ TrustedCityPartitionResolver              = (*TrustedNudgeIngress)(nil)
	_ TrustedCommandPartitionCoverageResolver   = (*TrustedNudgeIngress)(nil)
	_ TrustedCommandPartitionMembershipRecorder = (*TrustedNudgeIngress)(nil)
	_ TrustedCommandPartitionAdmissionRecovery  = (*TrustedNudgeIngress)(nil)
	_ TrustedCommandPartitionTerminalRecovery   = (*TrustedNudgeIngress)(nil)
)
