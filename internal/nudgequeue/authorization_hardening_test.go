package nudgequeue

import (
	"context"
	"errors"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
)

func TestClaimAuthorizedDirectStoreWriterCannotSelfAuthorize(t *testing.T) {
	repository := newVerifiedCommandRepository(t, newRepositoryAtomicTestStore())
	state, err := repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	command := repositoryCommandForRequest(t, state.Store, "direct-store-request", "self-asserted authority")
	entry, created, err := repository.create(t.Context(), "direct-store-request", command, trustedCityPartitionFromAuthority(command.TrustedIngress))
	if err != nil || !created || entry.Command == nil {
		t.Fatalf("direct store Create = %#v, created=%t err=%v", entry, created, err)
	}
	command = *entry.Command
	authority := newTestNudgeAuthority() // This authority never admitted the row.
	request := CommandClaimRequest{
		CommandID:           command.ID,
		ClaimID:             "claim-direct-store",
		OwnerID:             "owner-direct-store",
		AttemptID:           "attempt-direct-store",
		BoundLaunchIdentity: "launch-direct-store",
		Partition:           trustedCityPartitionFromAuthority(command.TrustedIngress),
		ClaimedAt:           command.DeliverAfter,
		LeaseUntil:          command.DeliverAfter.Add(time.Second),
	}

	result, err := repository.ClaimAuthorized(t.Context(), request, authority, authority)
	if !errors.Is(err, ErrNudgeAuthorizationUnknown) || !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("ClaimAuthorized error = %v, want parked missing-membership invariant", err)
	}
	if result.Disposition != CommandClaimAuthorizationUnknown || result.Command.State != CommandStatePending {
		t.Fatalf("ClaimAuthorized = %#v, want unchanged pending command", result)
	}
}

func TestClaimAuthorizedCopiedStampCannotAuthorizeDifferentPayload(t *testing.T) {
	fixture := newAuthorizedClaimFixture(t)
	state, err := fixture.repository.State(t.Context())
	if err != nil {
		t.Fatalf("State: %v", err)
	}
	forged := repositoryCommandForRequest(t, state.Store, "copied-stamp-request", "different payload")
	forged.CreatedAt = fixture.command.CreatedAt
	forged.DeliverAfter = fixture.command.DeliverAfter
	forged.ExpiresAt = fixture.command.ExpiresAt
	forged.TrustedIngress = fixture.command.TrustedIngress
	forged.TrustedIngress.TargetSessionID = forged.Target.SessionID
	forged.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(forged)
	entry, created, err := fixture.repository.create(t.Context(), "copied-stamp-request", forged, fixture.partition)
	if err != nil || !created || entry.Command == nil {
		t.Fatalf("Create copied stamp = %#v, created=%t err=%v", entry, created, err)
	}
	forged = *entry.Command
	request := CommandClaimRequest{
		CommandID:           forged.ID,
		ClaimID:             "claim-copied-stamp",
		OwnerID:             "owner-copied-stamp",
		AttemptID:           "attempt-copied-stamp",
		BoundLaunchIdentity: "launch-copied-stamp",
		Partition:           fixture.partition,
		ClaimedAt:           forged.DeliverAfter,
		LeaseUntil:          forged.DeliverAfter.Add(time.Second),
	}

	result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
	if !errors.Is(err, ErrNudgeAuthorizationUnknown) || !errors.Is(err, ErrCommandRepositoryPartition) {
		t.Fatalf("ClaimAuthorized error = %v, want parked missing-membership invariant", err)
	}
	if result.Disposition != CommandClaimAuthorizationUnknown || result.Command.State != CommandStatePending {
		t.Fatalf("ClaimAuthorized = %#v, want unchanged pending command", result)
	}
}

func TestClaimAuthorizedRejectsUnsupportedClaimPrincipalSchemaWithoutMutation(t *testing.T) {
	for _, schema := range []uint32{0, NudgePrincipalSchemaVersion + 1} {
		fixture := newAuthorizedClaimFixture(t)
		fixture.authority.setClaimSchema(schema)
		request := fixture.claimRequest("claim-schema-skew", "owner-schema", "attempt-schema", fixture.now.Add(time.Second))

		result, err := fixture.repository.ClaimAuthorized(t.Context(), request, fixture.authority, fixture.authority)
		if !errors.Is(err, ErrNudgeAuthorizationInvalid) || result != (CommandClaimResult{}) {
			t.Fatalf("schema %d result = %#v, err=%v; want fail-closed invalid evidence", schema, result, err)
		}
		resolution, getErr := fixture.repository.Get(t.Context(), fixture.command.ID)
		if getErr != nil || resolution.Entry.Command == nil || resolution.Entry.Command.State != CommandStatePending {
			t.Fatalf("schema %d mutated command: %#v err=%v", schema, resolution, getErr)
		}
	}
}

func TestClaimAuthorizedExactLaunchSubstitutionDenies(t *testing.T) {
	store := newRepositoryAtomicTestStore()
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	request := validNudgeIngressRequest(now)
	request.Mode = DeliveryModeImmediate
	request.Target = CommandTarget{
		SessionID:        request.Target.SessionID,
		IntentGeneration: request.Target.IntentGeneration,
		LaunchIdentity:   "launch-authorized",
		Policy:           TargetPolicyExactLaunch,
	}
	result, err := ingress.Admit(t.Context(), request)
	if err != nil || result.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", result, err)
	}
	command := *result.Entry.Command
	claim := CommandClaimRequest{
		CommandID:           command.ID,
		ClaimID:             "claim-launch-substitution",
		OwnerID:             "owner-launch-substitution",
		AttemptID:           "attempt-launch-substitution",
		BoundLaunchIdentity: "launch-substituted",
		Partition:           result.Partition,
		ClaimedAt:           now.Add(time.Second),
		LeaseUntil:          now.Add(2 * time.Second),
	}

	claimed, err := repository.ClaimAuthorized(t.Context(), claim, authority, authority)
	if err != nil {
		t.Fatalf("ClaimAuthorized: %v", err)
	}
	assertAuthorizationDeniedCommand(t, claimed)
}

func TestClaimAuthorizedLocalPeerSubstitutionIsPolicyDenied(t *testing.T) {
	for _, peer := range []string{"verified-peer", "substituted-peer"} {
		fixture := newAuthorizedClaimFixture(t)
		authorizer := peerBoundClaimAuthorizer{delegate: fixture.authority, required: "verified-peer"}
		ctx := context.WithValue(t.Context(), peerContextKey{}, peer)
		result, err := fixture.repository.ClaimAuthorized(ctx, fixture.claimRequest("claim-peer", "owner-peer", "attempt-peer", fixture.now.Add(time.Second)), authorizer, fixture.authority)
		if err != nil {
			t.Fatalf("peer %q ClaimAuthorized: %v", peer, err)
		}
		if peer == "verified-peer" && result.Disposition != CommandClaimAllowed {
			t.Fatalf("verified peer result = %#v, want allowed", result)
		}
		if peer == "substituted-peer" {
			assertAuthorizationDeniedCommand(t, result)
		}
	}
}

func TestClaimAuthorizationRunsInsideDurableReadWriteTransaction(t *testing.T) {
	store := &observedRepositoryAtomicStore{repositoryAtomicTestStore: newRepositoryAtomicTestStore()}
	repository := newVerifiedCommandRepository(t, store)
	authority := newTestNudgeAuthority()
	now := time.Date(2026, 7, 15, 12, 30, 0, 0, time.UTC)
	ingress, err := newTrustedNudgeIngressWithClock(repository, authority, func() time.Time { return now })
	if err != nil {
		t.Fatalf("newTrustedNudgeIngressWithClock: %v", err)
	}
	admitted, err := ingress.Admit(t.Context(), validNudgeIngressRequest(now))
	if err != nil || admitted.Entry.Command == nil {
		t.Fatalf("Admit = %#v, err=%v", admitted, err)
	}
	command := *admitted.Entry.Command
	authorizer := transactionObservingAuthorizer{delegate: authority, active: &store.active}
	claim := CommandClaimRequest{
		CommandID: command.ID, ClaimID: "claim-atomic", OwnerID: "owner-atomic", AttemptID: "attempt-atomic",
		BoundLaunchIdentity: "launch-atomic", Partition: admitted.Partition,
		ClaimedAt: now.Add(time.Second), LeaseUntil: now.Add(2 * time.Second),
	}

	result, err := repository.ClaimAuthorized(t.Context(), claim, authorizer, authority)
	if err != nil || result.Disposition != CommandClaimAllowed {
		t.Fatalf("ClaimAuthorized = %#v, err=%v", result, err)
	}
}

func TestTrustedIngressAndClaimKernelHaveNoEffectReachabilityImports(t *testing.T) {
	for _, path := range []string{"trusted_ingress.go", "claim_authorization.go", "command_security_profile.go"} {
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatalf("ParseFile(%s): %v", path, err)
		}
		for _, spec := range file.Imports {
			importPath, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				t.Fatalf("unquote import %s in %s: %v", spec.Path.Value, path, err)
			}
			for _, forbidden := range []string{"/internal/runtime", "/internal/worker", "tmux", "os/exec"} {
				if strings.Contains(importPath, forbidden) {
					t.Fatalf("%s imports effect capability %q", path, importPath)
				}
			}
		}
	}
}

type peerContextKey struct{}

type peerBoundClaimAuthorizer struct {
	delegate *testNudgeAuthority
	required string
}

func (a peerBoundClaimAuthorizer) AuthorizeNudgeClaim(ctx context.Context, request NudgeClaimAuthorizationRequest) (NudgeClaimAuthorization, error) {
	authorization, err := a.delegate.AuthorizeNudgeClaim(ctx, request)
	if err == nil && ctx.Value(peerContextKey{}) != a.required {
		authorization.Disposition = NudgeAuthorizationDenied
	}
	return authorization, err
}

type observedRepositoryAtomicStore struct {
	*repositoryAtomicTestStore
	active atomic.Bool
}

func (s *observedRepositoryAtomicStore) AtomicReadWrite(ctx context.Context, commitMessage string, fn func(beads.AtomicReadWriteTx) error) error {
	return s.repositoryAtomicTestStore.AtomicReadWrite(ctx, commitMessage, func(tx beads.AtomicReadWriteTx) error {
		s.active.Store(true)
		defer s.active.Store(false)
		return fn(tx)
	})
}

type transactionObservingAuthorizer struct {
	delegate *testNudgeAuthority
	active   *atomic.Bool
}

func (a transactionObservingAuthorizer) AuthorizeNudgeClaim(ctx context.Context, request NudgeClaimAuthorizationRequest) (NudgeClaimAuthorization, error) {
	if !a.active.Load() {
		return NudgeClaimAuthorization{}, errors.New("claim authorization ran outside atomic transaction")
	}
	return a.delegate.AuthorizeNudgeClaim(ctx, request)
}
