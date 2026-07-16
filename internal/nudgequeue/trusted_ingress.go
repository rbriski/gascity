package nudgequeue

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"
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
	// CommandCreatedAt is the immutable command timestamp covered by
	// Reference.PayloadDigest. Authorities must retain it across idempotent
	// replays; it is distinct from the evidence issuance time.
	CommandCreatedAt time.Time
	Reference        TrustedIngressReference
}

// NudgeIngressAuthorizationRequest is the digest-covered request shown to
// authenticated ingress authority. IntentDigest covers stable caller intent
// independently of the authority-selected creation time; PayloadDigest covers
// the provisional exact command. An idempotent authority replay returns its
// original command creation time and payload digest, which ingress reconstructs
// and verifies before persistence. The request deliberately carries no
// requester, tenant, city, credential, policy, store, or restore identity.
type NudgeIngressAuthorizationRequest struct {
	RequestID         string
	Action            string
	Mode              DeliveryMode
	Target            CommandTarget
	IntentDigest      string
	PayloadDigest     string
	DeliverAtCreation bool
	DeliverAfter      time.Time
	ExpiresAt         time.Time
	RequestedAt       time.Time
}

// TrustedNudgeAuthority is injected only by an authenticated control boundary.
// Its resolver must recover the authority-owned reference, not trust the copy
// supplied by a durable command.
type TrustedNudgeAuthority interface {
	AuthorizeNudgeIngress(context.Context, NudgeIngressAuthorizationRequest) (NudgeAuthorization, error)
	ResolveTrustedNudgeIngress(context.Context, TrustedIngressReference) (NudgeAuthorization, error)
	TrustedCommandPartitionCoverageResolver
	TrustedCommandPartitionMembershipRecorder
	TrustedCommandPartitionTerminalIntentAuthority
	TrustedCommandClaimTransitionAuthority
	TrustedCommandAuthorityRecovery
	VerifyCommandRepositoryEffectFence(context.Context, CommandRepositoryState) error
	RecordCommandRepositoryEffectFence(context.Context, CommandRepositoryState) error
}

// NudgeIngressRequest is the complete caller-owned nudge payload. Authority and
// store-order fields are intentionally absent.
type NudgeIngressRequest struct {
	RequestID string
	Mode      DeliveryMode
	Target    CommandTarget
	Source    CommandSource
	Message   string
	Reference *Reference
	// DeliverAfter is an absolute scheduled time. Zero means deliver at the
	// authority-selected command creation time and is the canonical immediate
	// delivery form for callers that do not control the ingress clock.
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
		state, err = i.repository.RepairLineage(ctx)
		if err != nil {
			return NudgeIngressResult{}, err
		}
	}
	commandID := CommandIDForRequest(state.Store, request.RequestID)
	if commandID == "" {
		return NudgeIngressResult{}, fmt.Errorf("%w: request identity is invalid", ErrNudgeAuthorizationInvalid)
	}
	existing, err := i.repository.Get(ctx, commandID)
	if err != nil {
		if _, repairErr := i.repository.RepairLineage(ctx); repairErr != nil {
			return NudgeIngressResult{}, errors.Join(err, repairErr)
		}
		existing, err = i.repository.Get(ctx, commandID)
		if err != nil {
			return NudgeIngressResult{}, err
		}
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
	deliverAtCreation := request.DeliverAfter.IsZero()
	effectiveDeliverAfter := request.DeliverAfter
	if deliverAtCreation {
		effectiveDeliverAfter = createdAt
	}
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
		DeliverAfter: effectiveDeliverAfter,
		ExpiresAt:    request.ExpiresAt,
	}
	if request.Target.Policy == TargetPolicyExactLaunch {
		command.Binding = &CommandBinding{LaunchIdentity: request.Target.LaunchIdentity, BoundAt: createdAt}
	}
	if err := validateNudgeIngressCommandBeforeAuthorization(state.Store, request.RequestID, request, command); err != nil {
		return NudgeIngressResult{}, err
	}
	intentDigest := computeNudgeIngressIntentDigestForRequest(command, deliverAtCreation)
	payloadDigest := ComputeCommandPayloadDigest(command)
	authorization, err := i.authority.AuthorizeNudgeIngress(ctx, NudgeIngressAuthorizationRequest{
		RequestID:         request.RequestID,
		Action:            NudgeCommandAction,
		Mode:              request.Mode,
		Target:            request.Target,
		IntentDigest:      intentDigest,
		PayloadDigest:     payloadDigest,
		DeliverAtCreation: deliverAtCreation,
		DeliverAfter:      request.DeliverAfter,
		ExpiresAt:         request.ExpiresAt,
		RequestedAt:       createdAt,
	})
	if err != nil {
		if errors.Is(err, ErrNudgeAuthorizationInvalid) || errors.Is(err, ErrNudgeAuthorizationDenied) || errors.Is(err, ErrNudgeAuthorizationUnknown) {
			return NudgeIngressResult{}, err
		}
		if errors.Is(err, ErrLocalNudgeAuthorityConflict) {
			return NudgeIngressResult{}, err
		}
		return NudgeIngressResult{}, fmt.Errorf("%w: trusted ingress authority failed: %w", ErrNudgeAuthorizationUnknown, err)
	}
	if err := classifyNudgeAuthorization(authorization); err != nil {
		return NudgeIngressResult{}, err
	}
	if err := validateNudgeAuthorizationSchema(authorization); err != nil {
		return NudgeIngressResult{}, err
	}
	if err := validateCommandTime("authorized command created at", authorization.CommandCreatedAt); err != nil {
		return NudgeIngressResult{}, fmt.Errorf("%w: %w", ErrNudgeAuthorizationInvalid, err)
	}
	command.TrustedIngress = authorization.Reference
	command.CreatedAt = authorization.CommandCreatedAt.UTC()
	if deliverAtCreation {
		command.DeliverAfter = command.CreatedAt
	}
	if command.Binding != nil {
		command.Binding.BoundAt = command.CreatedAt
	}
	payloadDigest = ComputeCommandPayloadDigest(command)
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

// validateNudgeIngressCommandBeforeAuthorization applies the complete known-v1
// caller-payload contract before an authority may durably bind the request ID.
// The synthetic evidence exists only to reuse the total command validator and
// is never returned, persisted, or presented to an authority.
func validateNudgeIngressCommandBeforeAuthorization(binding CommandStoreBinding, requestID string, request NudgeIngressRequest, command Command) error {
	invalid := func(err error) error {
		return fmt.Errorf("%w: caller command is invalid before authorization: %w", ErrNudgeAuthorizationInvalid, err)
	}
	if err := validateCommandIdentity("request id", requestID); err != nil {
		return invalid(err)
	}
	if command.ID == "" || command.ID != CommandIDForRequest(binding, requestID) {
		return invalid(errors.New("command id does not match the request and repository binding"))
	}
	if command.Version != CommandVersion1 || command.State != CommandStatePending || command.Store != (CommandStoreBinding{}) ||
		command.Order != (CommandOrder{}) || command.Retry != nil || command.Claim != nil || command.Terminal != nil {
		return invalid(errors.New("command is not a pristine pending v1 ingress intent"))
	}
	if !knownDeliveryMode(request.Mode) {
		return invalid(fmt.Errorf("unknown delivery mode %q", request.Mode))
	}
	if err := validateCommandTarget(request.Mode, request.Target); err != nil {
		return invalid(err)
	}
	if err := validateCommandSourceReference(request.Source, request.Reference); err != nil {
		return invalid(err)
	}
	if request.Message == "" {
		return invalid(errors.New("message is empty"))
	}
	if !utf8.ValidString(request.Message) {
		return invalid(errors.New("message is not valid UTF-8"))
	}
	if strings.IndexByte(request.Message, 0) >= 0 {
		return invalid(errors.New("message contains a NUL byte"))
	}
	if err := validateCommandTime("requested command creation time", command.CreatedAt); err != nil {
		return invalid(err)
	}
	if !request.DeliverAfter.IsZero() {
		if err := validateCommandTime("deliver_after", request.DeliverAfter); err != nil {
			return invalid(err)
		}
	}
	if err := validateCommandTime("expires_at", request.ExpiresAt); err != nil {
		return invalid(err)
	}
	if !request.DeliverAfter.IsZero() && !request.ExpiresAt.After(request.DeliverAfter) {
		return invalid(errors.New("expires_at must be after deliver_after"))
	}
	return nil
}

func computeNudgeIngressIntentDigest(command Command) string {
	return computeNudgeIngressIntentDigestForRequest(command, false)
}

func computeNudgeIngressIntentDigestForRequest(command Command, deliverAtCreation bool) string {
	command.CreatedAt = time.Time{}
	if deliverAtCreation {
		command.DeliverAfter = time.Time{}
	}
	return ComputeCommandPayloadDigest(command)
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
		if errors.Is(err, ErrLocalNudgeAuthorityConflict) || errors.Is(err, ErrNudgeAuthorizationInvalid) || errors.Is(err, ErrNudgeAuthorizationDenied) {
			return fmt.Errorf("publishing trusted command partition admission: %w", err)
		}
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
		if errors.Is(err, ErrLocalNudgeAuthorityConflict) || errors.Is(err, ErrNudgeAuthorizationInvalid) || errors.Is(err, ErrNudgeAuthorizationDenied) {
			return TrustedCityPartition{}, fmt.Errorf("resolving trusted ingress: %w", err)
		}
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

// VerifyCommandRepositoryEffectFence delegates the monotonic pre-effect store
// fence to the independently retained authority journal.
func (i *TrustedNudgeIngress) VerifyCommandRepositoryEffectFence(ctx context.Context, state CommandRepositoryState) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress effect fence is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.VerifyCommandRepositoryEffectFence(ctx, state)
}

// RecordCommandRepositoryEffectFence delegates the post-claim, pre-provider
// monotonic watermark to the independently retained authority journal.
func (i *TrustedNudgeIngress) RecordCommandRepositoryEffectFence(ctx context.Context, state CommandRepositoryState) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress effect fence is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.RecordCommandRepositoryEffectFence(ctx, state)
}

// PrepareCommandClaimTransition delegates exact claim write-ahead persistence
// to the independently durable authority journal.
func (i *TrustedNudgeIngress) PrepareCommandClaimTransition(ctx context.Context, intent CommandClaimTransitionIntent) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress claim transition authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.PrepareCommandClaimTransition(ctx, intent)
}

// ReleaseCommandClaimTransitionWriter delegates in-process writer release
// without changing durable preparation evidence.
func (i *TrustedNudgeIngress) ReleaseCommandClaimTransitionWriter(ctx context.Context, intent CommandClaimTransitionIntent) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress claim transition authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.ReleaseCommandClaimTransitionWriter(ctx, intent)
}

// AbortCommandClaimTransition delegates exact rolled-back preparation removal
// to the independently durable authority journal.
func (i *TrustedNudgeIngress) AbortCommandClaimTransition(ctx context.Context, intent CommandClaimTransitionIntent) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress claim transition authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.AbortCommandClaimTransition(ctx, intent)
}

// FinalizeCommandClaimTransition delegates atomic exact-receipt and effect
// high-water publication to the independently durable authority journal.
func (i *TrustedNudgeIngress) FinalizeCommandClaimTransition(ctx context.Context, receipt CommandClaimTransitionReceipt) (CommandClaimReceiptDisposition, error) {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return "", fmt.Errorf("%w: trusted ingress claim transition authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.FinalizeCommandClaimTransition(ctx, receipt)
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

// PrepareCommandPartitionTerminal delegates an exact authority-owned
// write-ahead terminal intent before the command-store transition commits.
func (i *TrustedNudgeIngress) PrepareCommandPartitionTerminal(ctx context.Context, intent CommandPartitionTerminalIntent) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress terminal intent authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.PrepareCommandPartitionTerminal(ctx, intent)
}

// ReleaseCommandPartitionTerminalWriter delegates the in-memory-only release
// for a store writer that will not proceed to terminal membership publication.
func (i *TrustedNudgeIngress) ReleaseCommandPartitionTerminalWriter(ctx context.Context, intent CommandPartitionTerminalIntent) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress terminal intent authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.ReleaseCommandPartitionTerminalWriter(ctx, intent)
}

// VerifyCommandPartitionTerminal revalidates a durable write-ahead intent for
// an exact terminal command. It never derives intent from the command store.
func (i *TrustedNudgeIngress) VerifyCommandPartitionTerminal(ctx context.Context, resolution CommandPartitionTerminalResolution) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress terminal intent authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.VerifyCommandPartitionTerminal(ctx, resolution)
}

// AbortCommandPartitionTerminal removes an exact preparation only after the
// repository proves its command-store callback rolled back.
func (i *TrustedNudgeIngress) AbortCommandPartitionTerminal(ctx context.Context, intent CommandPartitionTerminalIntent) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress terminal intent authority is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	return i.authority.AbortCommandPartitionTerminal(ctx, intent)
}

// RecoverCommandAuthority delegates the sole ordered startup recovery gate to
// the trusted authority bound to this exact repository.
func (i *TrustedNudgeIngress) RecoverCommandAuthority(ctx context.Context, repository *CommandRepository) error {
	if i == nil || isNilRepositoryDependency(i.authority) {
		return fmt.Errorf("%w: trusted ingress command authority recovery is not fully bound", ErrNudgeAuthorizationUnknown)
	}
	if repository == nil || i.repository == nil {
		return fmt.Errorf("%w: recovery repository is required", ErrNudgeAuthorizationInvalid)
	}
	if err := validateRepositoryContext(ctx); err != nil {
		return err
	}
	ctx, budget := withCommandAuthorityRecoveryBudget(ctx)
	for {
		boundState, retry, err := trustedNudgeRecoveryBindingState(ctx, i.repository)
		if err != nil {
			return fmt.Errorf("%w: reading trusted ingress repository binding: %w", ErrNudgeAuthorizationUnknown, err)
		}
		if retry {
			if err := budget.takePass("repairing trusted ingress repository binding"); err != nil {
				return err
			}
			continue
		}
		recoveryState, retry, err := trustedNudgeRecoveryBindingState(ctx, repository)
		if err != nil {
			return fmt.Errorf("%w: reading recovery repository binding: %w", ErrNudgeAuthorizationUnknown, err)
		}
		if retry {
			if err := budget.takePass("repairing recovery repository binding"); err != nil {
				return err
			}
			continue
		}
		if boundState.Store != recoveryState.Store || boundState.SchemaVersion != recoveryState.SchemaVersion ||
			boundState.WriterVersion != recoveryState.WriterVersion {
			return fmt.Errorf("%w: recovery repository differs from trusted ingress durable binding", ErrNudgeAuthorizationInvalid)
		}
		return i.authority.RecoverCommandAuthority(ctx, repository)
	}
}

func trustedNudgeRecoveryBindingState(ctx context.Context, repository *CommandRepository) (CommandRepositoryState, bool, error) {
	state, err := repository.State(ctx)
	if err == nil {
		return state, false, nil
	}
	retry, repairErr := repairTrustedNudgeRecoveryBindingAdvance(ctx, repository, err)
	if repairErr != nil {
		return CommandRepositoryState{}, false, errors.Join(err, fmt.Errorf("repairing monotonic repository binding advance: %w", repairErr))
	}
	if retry {
		return CommandRepositoryState{}, true, nil
	}
	return CommandRepositoryState{}, false, err
}

func repairTrustedNudgeRecoveryBindingAdvance(ctx context.Context, repository *CommandRepository, stateErr error) (bool, error) {
	var lineageErr *CommandRepositoryLineageError
	var decisionErr *RestoreAnchorDecisionError
	if !errors.As(stateErr, &lineageErr) || !errors.As(stateErr, &decisionErr) ||
		decisionErr.Decision.Disposition != RestoreAnchorAdvanceRequired {
		return false, nil
	}
	state := lineageErr.State
	candidate := decisionErr.Decision.Candidate
	if state.SchemaVersion != CommandRepositorySchemaVersion || state.WriterVersion != CommandRepositoryWriterVersion ||
		candidate == nil || candidate.Store != state.Store || candidate.HighestAcceptedRevision != state.Revision ||
		candidate.HighestAcceptedSequence != state.SequenceHighWater {
		return false, nil
	}
	if err := ctx.Err(); err != nil {
		return false, err
	}
	repaired, err := repository.RepairLineage(ctx)
	if err != nil {
		if contextErr := ctx.Err(); contextErr != nil {
			return false, contextErr
		}
		return false, err
	}
	if repaired.Store != state.Store || repaired.SchemaVersion != state.SchemaVersion || repaired.WriterVersion != state.WriterVersion ||
		repaired.Revision < state.Revision || repaired.SequenceHighWater < state.SequenceHighWater {
		return false, fmt.Errorf("%w: repaired repository binding did not retain the observed same-store monotonic advance", ErrNudgeAuthorizationInvalid)
	}
	return true, nil
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
	return trustedCityPartitionFromIdentity(reference.Issuer, reference.TenantScope, reference.CityScope)
}

func trustedCityPartitionFromIdentity(issuer, tenantScope, cityScope string) TrustedCityPartition {
	digest := sha256.New()
	_, _ = io.WriteString(digest, trustedCityPartitionDomainV1)
	for _, field := range []string{issuer, tenantScope, cityScope} {
		_, _ = digest.Write([]byte{0})
		_, _ = io.WriteString(digest, field)
	}
	var identity [sha256.Size]byte
	copy(identity[:], digest.Sum(nil))
	return TrustedCityPartition{identity: identity}
}

func nudgeIngressRequestMatchesCommand(request NudgeIngressRequest, command Command) bool {
	deliveryMatches := request.DeliverAfter.Equal(command.DeliverAfter)
	if request.DeliverAfter.IsZero() {
		deliveryMatches = command.DeliverAfter.Equal(command.CreatedAt)
	}
	return command.ID != "" &&
		request.Mode == command.Mode &&
		request.Target == command.Target &&
		request.Source == command.Source &&
		request.Message == command.Message &&
		reflect.DeepEqual(request.Reference, command.Reference) &&
		deliveryMatches &&
		request.ExpiresAt.Equal(command.ExpiresAt)
}

func cloneCommandReference(reference *Reference) *Reference {
	if reference == nil {
		return nil
	}
	cloned := *reference
	return &cloned
}
