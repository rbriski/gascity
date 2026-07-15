package nudgequeue

import "time"

const (
	// CommandVersion1 is the first durable nudge command wire version.
	CommandVersion1 uint32 = 1
	// MaxCommandBytes is the largest encoded durable command accepted by the
	// codec. Unsupported newer commands are preserved only within this bound.
	MaxCommandBytes = 64 << 10
	// MaxCommandDeadLetterDetailBytes bounds diagnostic evidence derived from
	// untrusted command bytes. Raw command content is never copied into Detail.
	MaxCommandDeadLetterDetailBytes = 256
	// MaxCommandRetryErrorDetailBytes bounds controller-owned retry evidence.
	MaxCommandRetryErrorDetailBytes = 512
	// MaxCommandTerminalDetailBytes bounds sanitized controller-owned terminal
	// diagnostics. Detail is never an authority or state discriminator.
	MaxCommandTerminalDetailBytes = 512
	// MaxCommandJSONDepth bounds recursive JSON inspection before any command
	// envelope is allocated or decoded.
	MaxCommandJSONDepth = 32
	// MaxCommandJSONMembers bounds the total object members and array elements
	// inspected across one command envelope.
	MaxCommandJSONMembers = 2048
	// NudgeCommandAction is the exact action covered by a v1 ingress reference.
	NudgeCommandAction = "nudge"
	// CommandPayloadDigestDomainV1 separates requester fingerprints from every
	// other Gas City digest protocol.
	CommandPayloadDigestDomainV1 = "gascity.nudge-command.payload"
	// CommandPayloadDigestSchemaV1 pins the canonical requester fingerprint
	// schema independently from the durable command wire version.
	CommandPayloadDigestSchemaV1 = "v1"
)

// CommandState is the complete v1 durable nudge lifecycle vocabulary.
type CommandState string

const (
	// CommandStatePending is durable work that has not entered a provider.
	CommandStatePending CommandState = "pending"
	// CommandStateInFlight is work held by a bounded delivery claim.
	CommandStateInFlight CommandState = "in_flight"
	// CommandStateDelivered is confirmed delivered by a conforming provider.
	CommandStateDelivered CommandState = "delivered"
	// CommandStateInjectedUnconfirmed is transport success without proof that
	// the agent consumed the injected content.
	CommandStateInjectedUnconfirmed CommandState = "injected_unconfirmed"
	// CommandStateDeliveryUnknown is terminal ambiguity that must not be blindly
	// replayed through a non-deduplicating provider.
	CommandStateDeliveryUnknown CommandState = "delivery_unknown"
	// CommandStateExpired is a command whose delivery window elapsed.
	CommandStateExpired CommandState = "expired"
	// CommandStateSuperseded is a command invalidated by newer target intent.
	CommandStateSuperseded CommandState = "superseded"
	// CommandStateDeadLettered is a command rejected after bounded attempts or
	// an unrecoverable known-version failure.
	CommandStateDeadLettered CommandState = "dead_lettered"
	// CommandStateUpgradeRequired parks a known command that requires a newer
	// compatible owner. A newer opaque wire also decodes to the separate
	// CommandDecodeUpgradeRequired disposition.
	CommandStateUpgradeRequired CommandState = "upgrade_required"
)

// DeliveryMode specifies when a nudge command may attempt delivery.
type DeliveryMode string

const (
	// DeliveryModeQueue durably accepts work for asynchronous delivery.
	DeliveryModeQueue DeliveryMode = "queue"
	// DeliveryModeWaitIdle parks delivery until the exact target is quiescent.
	DeliveryModeWaitIdle DeliveryMode = "wait_idle"
	// DeliveryModeImmediate addresses the exact launch bound at commit time.
	DeliveryModeImmediate DeliveryMode = "immediate"
)

// TargetPolicy specifies the only two v1 target-binding policies.
type TargetPolicy string

const (
	// TargetPolicyContinuation binds a stable session and continuation identity;
	// an eligible exact launch is selected once at claim time.
	TargetPolicyContinuation TargetPolicy = "continuation"
	// TargetPolicyExactLaunch binds one immutable launch before commit.
	TargetPolicyExactLaunch TargetPolicy = "exact_launch"
)

// CommandSource is the closed v1 ingress-source vocabulary.
type CommandSource string

const (
	// CommandSourceSession is a direct session nudge without a reference.
	CommandSourceSession CommandSource = "session"
	// CommandSourceMail is a mail-triggered nudge referencing its bead.
	CommandSourceMail CommandSource = "mail"
	// CommandSourceWait is a wait-triggered nudge referencing the wait.
	CommandSourceWait CommandSource = "wait"
	// CommandSourceSling is a dispatch-triggered nudge referencing work.
	CommandSourceSling CommandSource = "sling"
	// CommandSourceQueue is an explicitly queued direct nudge.
	CommandSourceQueue CommandSource = "queue"
	// CommandSourceController is controller-generated infrastructure traffic.
	CommandSourceController CommandSource = "controller"
)

const (
	// CommandReferenceWait identifies one durable wait record.
	CommandReferenceWait = "wait"
	// CommandReferenceBead identifies one durable work or mail bead.
	CommandReferenceBead = "bead"
)

// CommandTarget is the exact durable target contract for one command.
type CommandTarget struct {
	SessionID            string       `json:"session_id"`
	IntentGeneration     uint64       `json:"intent_generation"`
	ContinuationIdentity string       `json:"continuation_identity,omitempty"`
	LaunchIdentity       string       `json:"launch_identity,omitempty"`
	Policy               TargetPolicy `json:"policy"`
}

// CommandStoreBinding fences a command to one store lineage and accepted
// restore epoch. The codec carries the binding but does not verify external
// restore authority.
type CommandStoreBinding struct {
	StoreUUID    string `json:"store_uuid"`
	RestoreEpoch uint64 `json:"restore_epoch"`
}

// CommandOrder identifies the command's durable ordering point.
type CommandOrder struct {
	Sequence uint64 `json:"sequence"`
	Revision uint64 `json:"revision"`
}

// CommandBinding is the immutable exact-launch selection retained after the
// active claim is released. A continuation target may acquire it once; it may
// never be cleared or retargeted by a later attempt.
type CommandBinding struct {
	LaunchIdentity string    `json:"launch_identity"`
	BoundAt        time.Time `json:"bound_at"`
}

// TrustedIngressReference records where trusted ingress provenance and its
// policy decision can be revalidated. Possessing or decoding this value is not
// authorization; claim-time authorization is a separate required gate.
type TrustedIngressReference struct {
	Issuer           string    `json:"issuer"`
	ReferenceID      string    `json:"reference_id"`
	PrincipalID      string    `json:"principal_id"`
	TenantScope      string    `json:"tenant_scope"`
	CityScope        string    `json:"city_scope"`
	CredentialClass  string    `json:"credential_class"`
	PolicyVersion    string    `json:"policy_version"`
	PolicyDecisionID string    `json:"policy_decision_id"`
	Action           string    `json:"action"`
	TargetSessionID  string    `json:"target_session_id"`
	PayloadDigest    string    `json:"payload_digest"`
	IssuedAt         time.Time `json:"issued_at"`
	ExpiresAt        time.Time `json:"expires_at"`
}

// CommandClaim is the durable bounded ownership record for an in-flight
// command.
type CommandClaim struct {
	ID                         string    `json:"id"`
	OwnerID                    string    `json:"owner_id"`
	OperationID                string    `json:"operation_id"`
	AttemptID                  string    `json:"attempt_id"`
	BoundLaunchIdentity        string    `json:"bound_launch_identity"`
	AuthorizationDecisionID    string    `json:"authorization_decision_id"`
	AuthorizationPolicyVersion string    `json:"authorization_policy_version"`
	ClaimedAt                  time.Time `json:"claimed_at"`
	LeaseUntil                 time.Time `json:"lease_until"`
}

// ProviderStage is closed v1 evidence about whether provider entry occurred.
type ProviderStage string

const (
	// ProviderStageNotEntered proves the provider was not entered.
	ProviderStageNotEntered ProviderStage = "not_entered"
	// ProviderStageAccepted proves the exact provider transport accepted work.
	ProviderStageAccepted ProviderStage = "accepted"
	// ProviderStageMayHaveEntered records irreducible provider-entry ambiguity.
	ProviderStageMayHaveEntered ProviderStage = "may_have_entered"
	// ProviderStageRejected records a definite provider rejection.
	ProviderStageRejected ProviderStage = "rejected"
)

// CompletionState is closed v1 evidence about transport completion.
type CompletionState string

const (
	// CompletionStateNotCompleted proves no successful transport completion.
	CompletionStateNotCompleted CompletionState = "not_completed"
	// CompletionStateCompleted records successful transport completion.
	CompletionStateCompleted CompletionState = "completed"
	// CompletionStateUnknown records an outcome that cannot safely be replayed.
	CompletionStateUnknown CompletionState = "unknown"
)

// CommandActionResult is the closed v1 action-specific terminal result.
type CommandActionResult string

const (
	// CommandActionResultDelivered records provider-confirmed delivery.
	CommandActionResultDelivered CommandActionResult = "delivered"
	// CommandActionResultDuplicate records provider-confirmed command-ID
	// deduplication and is a successful delivered result.
	CommandActionResultDuplicate CommandActionResult = "duplicate"
	// CommandActionResultInjectedUnconfirmed records proven injection without
	// observable agent consumption.
	CommandActionResultInjectedUnconfirmed CommandActionResult = "injected_unconfirmed"
	// CommandActionResultDeliveryUnknown records irreducible provider ambiguity.
	CommandActionResultDeliveryUnknown CommandActionResult = "delivery_unknown"
	// CommandActionResultExpired records expiry before provider effect.
	CommandActionResultExpired CommandActionResult = "expired"
	// CommandActionResultSuperseded records invalidation by newer target intent.
	CommandActionResultSuperseded CommandActionResult = "superseded"
	// CommandActionResultTargetMissing records a provider-proven absent exact
	// target and is distinct from a generic rejection.
	CommandActionResultTargetMissing CommandActionResult = "target_missing"
	// CommandActionResultRejected records a definite provider rejection.
	CommandActionResultRejected CommandActionResult = "rejected"
	// CommandActionResultAuthorizationDenied records a claim-time policy denial
	// before provider entry.
	CommandActionResultAuthorizationDenied CommandActionResult = "authorization_denied"
	// CommandActionResultRetryExhausted records bounded definite pre-effect
	// failures whose retry budget is exhausted.
	CommandActionResultRetryExhausted CommandActionResult = "retry_exhausted"
	// CommandActionResultDeadLettered records known command/reference poison.
	CommandActionResultDeadLettered CommandActionResult = "dead_lettered"
)

// CommandErrorClass is the closed v1 mechanical error vocabulary. Human-safe
// diagnostic text is carried separately and never drives retry or state.
type CommandErrorClass string

const (
	// CommandErrorClassNone is the required class for successful outcomes.
	CommandErrorClassNone CommandErrorClass = ""
	// CommandErrorClassProviderBusy is a definite pre-effect retryable refusal.
	CommandErrorClassProviderBusy CommandErrorClass = "provider_busy"
	// CommandErrorClassProviderUnavailable is a definite pre-effect provider
	// availability failure.
	CommandErrorClassProviderUnavailable CommandErrorClass = "provider_unavailable"
	// CommandErrorClassProviderRejected is a terminal definite rejection.
	CommandErrorClassProviderRejected CommandErrorClass = "provider_rejected"
	// CommandErrorClassProviderAmbiguous is a terminal may-have-entered result.
	CommandErrorClassProviderAmbiguous CommandErrorClass = "provider_ambiguous"
	// CommandErrorClassTargetMissing is a provider-proven missing exact target.
	CommandErrorClassTargetMissing CommandErrorClass = "target_missing"
	// CommandErrorClassAuthorizationDenied is a terminal claim-time denial.
	CommandErrorClassAuthorizationDenied CommandErrorClass = "authorization_denied"
	// CommandErrorClassRetryExhausted is bounded retry-budget exhaustion.
	CommandErrorClassRetryExhausted CommandErrorClass = "retry_exhausted"
	// CommandErrorClassExpired is mechanical delivery-window expiry.
	CommandErrorClassExpired CommandErrorClass = "expired"
	// CommandErrorClassSuperseded is mechanical target-intent replacement.
	CommandErrorClassSuperseded CommandErrorClass = "superseded"
	// CommandErrorClassInvalidReference is a malformed or unsupported reference.
	CommandErrorClassInvalidReference CommandErrorClass = "invalid_reference"
	// CommandErrorClassInvalidCommand is known-version command poison.
	CommandErrorClassInvalidCommand CommandErrorClass = "invalid_command"
)

// CommandTerminal records when and why a command entered a terminal state.
type CommandTerminal struct {
	At                         time.Time           `json:"at"`
	ActionResult               CommandActionResult `json:"action_result"`
	ErrorClass                 CommandErrorClass   `json:"error_class,omitempty"`
	Detail                     string              `json:"detail,omitempty"`
	ClaimID                    string              `json:"claim_id,omitempty"`
	OperationID                string              `json:"operation_id,omitempty"`
	AttemptID                  string              `json:"attempt_id,omitempty"`
	BoundLaunchIdentity        string              `json:"bound_launch_identity,omitempty"`
	AuthorizationDecisionID    string              `json:"authorization_decision_id,omitempty"`
	AuthorizationPolicyVersion string              `json:"authorization_policy_version,omitempty"`
	ProviderStage              ProviderStage       `json:"provider_stage"`
	Completion                 CompletionState     `json:"completion_state"`
}

// CommandRetry is controller-owned retry and attempt evidence. It is excluded
// from the trusted-ingress requester payload digest; request scheduling fields
// on Command remain immutable and digest-covered.
type CommandRetry struct {
	AttemptCount               uint32            `json:"attempt_count"`
	LastAttemptAt              time.Time         `json:"last_attempt_at"`
	ClaimID                    string            `json:"claim_id"`
	OperationID                string            `json:"operation_id"`
	AttemptID                  string            `json:"attempt_id"`
	BoundLaunchIdentity        string            `json:"bound_launch_identity"`
	AuthorizationDecisionID    string            `json:"authorization_decision_id"`
	AuthorizationPolicyVersion string            `json:"authorization_policy_version"`
	NextEligibleAt             *time.Time        `json:"next_eligible_at,omitempty"`
	ErrorClass                 CommandErrorClass `json:"error_class,omitempty"`
	ErrorDetail                string            `json:"error_detail,omitempty"`
}

// Command is the production-inert v1 durable nudge command domain value.
// Persistence, authorization, claiming, and provider effects are deliberately
// outside this type and codec slice.
type Command struct {
	Version        uint32                  `json:"version"`
	ID             string                  `json:"id"`
	State          CommandState            `json:"state"`
	Mode           DeliveryMode            `json:"mode"`
	Target         CommandTarget           `json:"target"`
	Store          CommandStoreBinding     `json:"store"`
	Order          CommandOrder            `json:"order"`
	TrustedIngress TrustedIngressReference `json:"trusted_ingress"`
	Source         CommandSource           `json:"source"`
	Message        string                  `json:"message"`
	Reference      *Reference              `json:"reference,omitempty"`
	CreatedAt      time.Time               `json:"created_at"`
	DeliverAfter   time.Time               `json:"deliver_after"`
	ExpiresAt      time.Time               `json:"expires_at"`
	Binding        *CommandBinding         `json:"binding,omitempty"`
	Retry          *CommandRetry           `json:"retry,omitempty"`
	Claim          *CommandClaim           `json:"claim,omitempty"`
	Terminal       *CommandTerminal        `json:"terminal,omitempty"`
}

// CommandRoutingHeader is the version-invariant, authority-free ordering
// header exposed for both known and opaque newer commands. TargetSessionID is
// the sole v1 ordering domain.
type CommandRoutingHeader struct {
	CommandID        string
	Store            CommandStoreBinding
	TargetSessionID  string
	IntentGeneration uint64
	Sequence         uint64
	Revision         uint64
}

// OpaqueCommand is one byte-preserved newer command together with only the
// version-invariant routing header that an older owner may safely inspect. The
// index re-decodes Raw before accepting this value; Version and Routing are
// never trusted independently from those bytes.
type OpaqueCommand struct {
	Version uint32
	Routing CommandRoutingHeader
	Raw     []byte
}

// CommandDecodeDisposition is the total decoder's tagged result kind.
type CommandDecodeDisposition string

const (
	// CommandDecodeDecoded contains one strictly validated v1 Command.
	CommandDecodeDecoded CommandDecodeDisposition = "decoded"
	// CommandDecodeDeadLetter classifies malformed, oversized, unsupported-old,
	// or invalid-known input without returning a partially trusted command.
	CommandDecodeDeadLetter CommandDecodeDisposition = "dead_letter"
	// CommandDecodeUpgradeRequired preserves a well-formed newer wire opaquely.
	CommandDecodeUpgradeRequired CommandDecodeDisposition = "upgrade_required"
)

// CommandDeadLetterReason is a stable codec failure classification.
type CommandDeadLetterReason string

const (
	// CommandDeadLetterPayloadTooLarge rejects input before parsing or copying.
	CommandDeadLetterPayloadTooLarge CommandDeadLetterReason = "payload_too_large"
	// CommandDeadLetterMalformed rejects an invalid or ambiguous JSON envelope.
	CommandDeadLetterMalformed CommandDeadLetterReason = "malformed"
	// CommandDeadLetterInvalidKnownVersion rejects a structurally or
	// semantically invalid v1 command.
	CommandDeadLetterInvalidKnownVersion CommandDeadLetterReason = "invalid_known_version"
	// CommandDeadLetterUnsupportedVersion rejects older/nonpositive versions.
	CommandDeadLetterUnsupportedVersion CommandDeadLetterReason = "unsupported_version"
)

// CommandDecodeResult is the total, non-panicking result of DecodeCommand.
// Routing is present for decoded and upgrade-required dispositions. Exactly
// one remaining payload is meaningful: Command for decoded v1, Raw for an
// upgrade-required newer input, or DeadLetterReason for dead-letter.
type CommandDecodeResult struct {
	Disposition      CommandDecodeDisposition
	Version          uint32
	Routing          CommandRoutingHeader
	Command          Command
	Raw              []byte
	DeadLetterReason CommandDeadLetterReason
	Detail           string
}
