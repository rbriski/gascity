package nudgequeue

import (
	"context"
	"errors"
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

func TestTrustedNudgeIngressRejectsAuthorityCoverageSubstitution(t *testing.T) {
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	for name, mutate := range map[string]func(*NudgeAuthorization){
		"action": func(a *NudgeAuthorization) { a.Reference.Action = "stop" },
		"target": func(a *NudgeAuthorization) { a.Reference.TargetSessionID = "another-session" },
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
}

func newTestNudgeAuthority() *testNudgeAuthority {
	return &testNudgeAuthority{
		references:       make(map[string]NudgeAuthorization),
		disposition:      NudgeAuthorizationAllowed,
		schema:           NudgePrincipalSchemaVersion,
		claimDisposition: NudgeAuthorizationAllowed,
		claimSchema:      NudgePrincipalSchemaVersion,
		coverage:         newTestCommandPartitionCoverageLedger(),
	}
}

func (a *testNudgeAuthority) AuthorizeNudgeIngress(_ context.Context, request NudgeIngressAuthorizationRequest) (NudgeAuthorization, error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.calls++
	authorization := NudgeAuthorization{
		Disposition:            a.disposition,
		PrincipalSchemaVersion: a.schema,
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
	return a.coverage.RecordCommandPartitionTerminal(ctx, terminal)
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
)
