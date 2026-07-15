package nudgequeue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"time"
)

const (
	// NudgePrincipalSchemaVersion is the current authenticated-principal
	// contract understood by the nudge authorization boundary. Exactly N and
	// N-1 are accepted during rolling upgrades.
	NudgePrincipalSchemaVersion uint32 = 2

	trustedCityPartitionDomainV1 = "gascity.nudge-command.trusted-city-partition.v1"
)

var (
	// ErrNudgeAuthorizationDenied is a definitive policy refusal. At ingress no
	// command is created; at claim the command is durably terminalized.
	ErrNudgeAuthorizationDenied = errors.New("nudge authorization denied")
	// ErrNudgeAuthorizationUnknown is a fail-closed transient inability to
	// determine authority. It never authorizes or terminalizes a command.
	ErrNudgeAuthorizationUnknown = errors.New("nudge authorization unknown")
	// ErrNudgeAuthorizationInvalid reports malformed, substituted, or
	// unsupported evidence returned by an authority implementation.
	ErrNudgeAuthorizationInvalid = errors.New("invalid nudge authorization evidence")
)

// NudgeAuthorizationDisposition is the total result vocabulary shared by
// trusted ingress and claim-time policy checks.
type NudgeAuthorizationDisposition string

const (
	// NudgeAuthorizationAllowed admits the exact action and bounded payload.
	NudgeAuthorizationAllowed NudgeAuthorizationDisposition = "allowed"
	// NudgeAuthorizationDenied is a definitive current-policy refusal.
	NudgeAuthorizationDenied NudgeAuthorizationDisposition = "denied"
	// NudgeAuthorizationUnknown parks on unavailable or incomplete authority.
	NudgeAuthorizationUnknown NudgeAuthorizationDisposition = "unknown"
)

// NudgeAuthorization is authority-owned evidence. Callers may request an
// action, but cannot populate this value through NudgeIngressRequest.
type NudgeAuthorization struct {
	Disposition            NudgeAuthorizationDisposition
	PrincipalSchemaVersion uint32
	Reference              TrustedIngressReference
}

// NudgeIngressAuthorizationRequest is the exact, digest-covered request shown
// to authenticated ingress authority. It deliberately carries no requester,
// tenant, city, credential, policy, store, or restore identity.
type NudgeIngressAuthorizationRequest struct {
	RequestID     string
	Action        string
	Target        CommandTarget
	PayloadDigest string
	RequestedAt   time.Time
}

// TrustedNudgeAuthority is injected only by an authenticated control boundary.
// Its resolver must recover the authority-owned reference, not trust the copy
// supplied by a durable command.
type TrustedNudgeAuthority interface {
	AuthorizeNudgeIngress(context.Context, NudgeIngressAuthorizationRequest) (NudgeAuthorization, error)
	ResolveTrustedNudgeIngress(context.Context, TrustedIngressReference) (NudgeAuthorization, error)
	TrustedCommandPartitionCoverageResolver
	TrustedCommandPartitionMembershipRecorder
}

// NudgeIngressRequest is the complete caller-owned nudge payload. Authority and
// store-order fields are intentionally absent.
type NudgeIngressRequest struct {
	RequestID    string
	Mode         DeliveryMode
	Target       CommandTarget
	Source       CommandSource
	Message      string
	Reference    *Reference
	DeliverAfter time.Time
	ExpiresAt    time.Time
}

// NudgeIngressResult reports durable idempotent admission and the opaque city
// partition independently derived from authority evidence.
type NudgeIngressResult struct {
	Entry     CommandIndexEntry
	Partition TrustedCityPartition
	Created   bool
}

// TrustedNudgeIngress is the sole command constructor that can stamp requester
// provenance. It owns no provider, runtime, worker, or tmux capability.
type TrustedNudgeIngress struct {
	repository *CommandRepository
	authority  TrustedNudgeAuthority
	now        func() time.Time
}

// NewTrustedNudgeIngress constructs an authenticated, idempotent nudge front
// door using the process wall clock. Missing and typed-nil dependencies fail
// closed.
func NewTrustedNudgeIngress(repository *CommandRepository, authority TrustedNudgeAuthority) (*TrustedNudgeIngress, error) {
	return newTrustedNudgeIngressWithClock(repository, authority, time.Now)
}

func newTrustedNudgeIngressWithClock(repository *CommandRepository, authority TrustedNudgeAuthority, now func() time.Time) (*TrustedNudgeIngress, error) {
	if repository == nil {
		return nil, fmt.Errorf("%w: command repository is required", ErrNudgeAuthorizationInvalid)
	}
	if isNilRepositoryDependency(authority) {
		return nil, fmt.Errorf("%w: trusted authority is required", ErrNudgeAuthorizationInvalid)
	}
	if now == nil {
		return nil, fmt.Errorf("%w: ingress clock is required", ErrNudgeAuthorizationInvalid)
	}
	return &TrustedNudgeIngress{repository: repository, authority: authority, now: now}, nil
}

// Admit obtains authority-owned requester provenance and atomically creates one
// pending command. Repeating a committed request returns the exact stored
// command and never allocates another identity or restamps caller data.
func (i *TrustedNudgeIngress) Admit(ctx context.Context, request NudgeIngressRequest) (NudgeIngressResult, error) {
	if i == nil || i.repository == nil || isNilRepositoryDependency(i.authority) || i.now == nil {
		return NudgeIngressResult{}, fmt.Errorf("%w: trusted ingress is not fully bound", ErrNudgeAuthorizationInvalid)
	}
	if err := validateRepositoryContext(ctx); err != nil {
		return NudgeIngressResult{}, err
	}
	state, err := i.repository.State(ctx)
	if err != nil {
		return NudgeIngressResult{}, err
	}
	commandID := CommandIDForRequest(state.Store, request.RequestID)
	if commandID == "" {
		return NudgeIngressResult{}, fmt.Errorf("%w: request identity is invalid", ErrNudgeAuthorizationInvalid)
	}
	existing, err := i.repository.Get(ctx, commandID)
	if err != nil {
		return NudgeIngressResult{}, err
	}
	if existing.Found {
		if existing.Entry.Command == nil || !nudgeIngressRequestMatchesCommand(request, *existing.Entry.Command) {
			return NudgeIngressResult{}, fmt.Errorf("%w: request id is already bound to different immutable content", ErrCommandRepositoryIdempotencyConflict)
		}
		partition, err := i.ResolveCommandPartition(ctx, existing.Entry.Command.TrustedIngress)
		if err != nil {
			return NudgeIngressResult{}, err
		}
		if commandIsPristinePending(*existing.Entry.Command) {
			if err := i.recordAdmission(ctx, *existing.Entry.Command, partition); err != nil {
				return NudgeIngressResult{}, err
			}
		}
		reader, err := NewCommandPartitionReader(i.repository, partition, i)
		if err != nil {
			return NudgeIngressResult{}, err
		}
		verified, err := reader.Get(ctx, commandID)
		if err != nil {
			return NudgeIngressResult{}, err
		}
		if !verified.Found {
			return NudgeIngressResult{}, &CommandRepositoryPartitionError{Operation: "idempotent ingress", CommandID: commandID, Err: errors.New("trusted command is absent from its indexed partition")}
		}
		return NudgeIngressResult{Entry: verified.Entry, Partition: partition}, nil
	}

	createdAt := i.now().UTC()
	command := Command{
		Version:      CommandVersion1,
		ID:           commandID,
		State:        CommandStatePending,
		Mode:         request.Mode,
		Target:       request.Target,
		Source:       request.Source,
		Message:      request.Message,
		Reference:    cloneCommandReference(request.Reference),
		CreatedAt:    createdAt,
		DeliverAfter: request.DeliverAfter,
		ExpiresAt:    request.ExpiresAt,
	}
	if request.Target.Policy == TargetPolicyExactLaunch {
		command.Binding = &CommandBinding{LaunchIdentity: request.Target.LaunchIdentity, BoundAt: createdAt}
	}
	payloadDigest := ComputeCommandPayloadDigest(command)
	authorization, err := i.authority.AuthorizeNudgeIngress(ctx, NudgeIngressAuthorizationRequest{
		RequestID:     request.RequestID,
		Action:        NudgeCommandAction,
		Target:        request.Target,
		PayloadDigest: payloadDigest,
		RequestedAt:   createdAt,
	})
	if err != nil {
		return NudgeIngressResult{}, fmt.Errorf("%w: trusted ingress authority failed: %w", ErrNudgeAuthorizationUnknown, err)
	}
	if err := classifyNudgeAuthorization(authorization); err != nil {
		return NudgeIngressResult{}, err
	}
	if err := validateNudgeAuthorizationSchema(authorization); err != nil {
		return NudgeIngressResult{}, err
	}
	command.TrustedIngress = authorization.Reference
	if command.TrustedIngress.Action != NudgeCommandAction ||
		command.TrustedIngress.TargetSessionID != command.Target.SessionID ||
		command.TrustedIngress.PayloadDigest != payloadDigest {
		return NudgeIngressResult{}, fmt.Errorf("%w: authority response does not cover the exact action, target, and payload", ErrNudgeAuthorizationInvalid)
	}
	if err := validateCommandCreateRequest(state.Store, request.RequestID, command); err != nil {
		return NudgeIngressResult{}, fmt.Errorf("%w: %w", ErrNudgeAuthorizationInvalid, err)
	}
	partition := trustedCityPartitionFromAuthority(command.TrustedIngress)
	entry, created, err := i.repository.create(ctx, request.RequestID, command, partition)
	if err != nil {
		return NudgeIngressResult{}, err
	}
	if entry.Command == nil {
		return NudgeIngressResult{}, fmt.Errorf("%w: admitted command has no supported membership identity", ErrNudgeAuthorizationInvalid)
	}
	if created || commandIsPristinePending(*entry.Command) {
		if err := i.recordAdmission(ctx, *entry.Command, partition); err != nil {
			return NudgeIngressResult{}, err
		}
	}
	return NudgeIngressResult{Entry: entry, Partition: partition, Created: created}, nil
}

func commandIsPristinePending(command Command) bool {
	return command.State == CommandStatePending && command.Claim == nil && command.Retry == nil && command.Terminal == nil
}

func (i *TrustedNudgeIngress) recordAdmission(ctx context.Context, command Command, partition TrustedCityPartition) error {
	if err := i.authority.RecordCommandPartitionAdmission(ctx, CommandPartitionAdmission{
		Store:              command.Store,
		RepositoryRevision: command.Order.Revision,
		CommandID:          command.ID,
		Sequence:           command.Order.Sequence,
		Partition:          partition,
	}); err != nil {
		return fmt.Errorf("%w: publishing trusted command partition admission: %w", ErrNudgeAuthorizationUnknown, err)
	}
	return nil
}

// ResolveCommandPartition revalidates a command's copied ingress reference
// against independent authority before deriving its opaque city partition.
func (i *TrustedNudgeIngress) ResolveCommandPartition(ctx context.Context, reference TrustedIngressReference) (TrustedCityPartition, error) {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return TrustedCityPartition{}, fmt.Errorf("%w: trusted ingress resolver is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	authorization, err := i.authority.ResolveTrustedNudgeIngress(ctx, reference)
	if err != nil {
		return TrustedCityPartition{}, fmt.Errorf("%w: resolving trusted ingress: %w", ErrNudgeAuthorizationUnknown, err)
	}
	if err := classifyNudgeAuthorization(authorization); err != nil {
		return TrustedCityPartition{}, err
	}
	if err := validateNudgeAuthorizationSchema(authorization); err != nil {
		return TrustedCityPartition{}, err
	}
	if authorization.Reference != reference {
		return TrustedCityPartition{}, fmt.Errorf("%w: durable ingress reference differs from authority", ErrNudgeAuthorizationDenied)
	}
	return trustedCityPartitionFromAuthority(authorization.Reference), nil
}

// ResolveCommandPartitionCoverage delegates the revision-bound completeness
// proof to the independently retained ingress authority.
func (i *TrustedNudgeIngress) ResolveCommandPartitionCoverage(ctx context.Context, request CommandPartitionCoverageRequest) (CommandPartitionCoverage, error) {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return CommandPartitionCoverage{}, fmt.Errorf("%w: trusted ingress coverage resolver is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.ResolveCommandPartitionCoverage(ctx, request)
}

// ResolveCommandPartitionMembership delegates one revision-bound exact
// membership proof to the independently retained ingress authority.
func (i *TrustedNudgeIngress) ResolveCommandPartitionMembership(ctx context.Context, request CommandPartitionMembershipRequest) (CommandPartitionMembership, error) {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return CommandPartitionMembership{}, fmt.Errorf("%w: trusted ingress membership resolver is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.ResolveCommandPartitionMembership(ctx, request)
}

// RecordCommandPartitionAdmission delegates an idempotent admission
// publication to the independently retained ingress authority.
func (i *TrustedNudgeIngress) RecordCommandPartitionAdmission(ctx context.Context, admission CommandPartitionAdmission) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress membership recorder is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.RecordCommandPartitionAdmission(ctx, admission)
}

// RecordCommandPartitionTerminal delegates an idempotent terminal membership
// publication to the independently retained ingress authority.
func (i *TrustedNudgeIngress) RecordCommandPartitionTerminal(ctx context.Context, terminal CommandPartitionTerminal) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress membership recorder is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.RecordCommandPartitionTerminal(ctx, terminal)
}

func classifyNudgeAuthorization(authorization NudgeAuthorization) error {
	switch authorization.Disposition {
	case NudgeAuthorizationAllowed:
		return nil
	case NudgeAuthorizationDenied:
		return ErrNudgeAuthorizationDenied
	case NudgeAuthorizationUnknown:
		return ErrNudgeAuthorizationUnknown
	default:
		return fmt.Errorf("%w: unknown disposition %q", ErrNudgeAuthorizationInvalid, authorization.Disposition)
	}
}

func validateNudgeAuthorizationSchema(authorization NudgeAuthorization) error {
	if authorization.PrincipalSchemaVersion != NudgePrincipalSchemaVersion &&
		authorization.PrincipalSchemaVersion != NudgePrincipalSchemaVersion-1 {
		return fmt.Errorf("%w: principal schema %d is outside supported versions %d and %d",
			ErrNudgeAuthorizationInvalid,
			authorization.PrincipalSchemaVersion,
			NudgePrincipalSchemaVersion,
			NudgePrincipalSchemaVersion-1,
		)
	}
	return nil
}

func trustedCityPartitionFromAuthority(reference TrustedIngressReference) TrustedCityPartition {
	digest := sha256.New()
	_, _ = io.WriteString(digest, trustedCityPartitionDomainV1)
	for _, field := range []string{reference.Issuer, reference.TenantScope, reference.CityScope} {
		_, _ = digest.Write([]byte{0})
		_, _ = io.WriteString(digest, field)
	}
	var identity [sha256.Size]byte
	copy(identity[:], digest.Sum(nil))
	return TrustedCityPartition{identity: identity}
}

func nudgeIngressRequestMatchesCommand(request NudgeIngressRequest, command Command) bool {
	return command.ID != "" &&
		request.Mode == command.Mode &&
		request.Target == command.Target &&
		request.Source == command.Source &&
		request.Message == command.Message &&
		reflect.DeepEqual(request.Reference, command.Reference) &&
		request.DeliverAfter.Equal(command.DeliverAfter) &&
		request.ExpiresAt.Equal(command.ExpiresAt)
}

func cloneCommandReference(reference *Reference) *Reference {
	if reference == nil {
		return nil
	}
	cloned := *reference
	return &cloned
}
