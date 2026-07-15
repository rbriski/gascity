package nudgequeue

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

const (
	maxCommandIdentityBytes = 256
)

// ValidateCommandStoreBinding validates the canonical store incarnation used
// by command persistence, indexes, and reconcile keys.
func ValidateCommandStoreBinding(store CommandStoreBinding) error {
	if err := validateCommandIdentity("store uuid", store.StoreUUID); err != nil {
		return err
	}
	if store.RestoreEpoch == 0 {
		return errors.New("restore epoch must be positive")
	}
	return nil
}

// ComputeCommandPayloadDigest returns the canonical SHA-256 coverage digest
// for the exact nudge action, command identity, mode, target, content,
// reference, and delivery window. Store lineage and ordering are controller
// envelope fields validated separately, not requester payload. Trusted ingress
// stamps this value; decoding it still grants no authority.
func ComputeCommandPayloadDigest(command Command) string {
	payload := struct {
		Domain       string        `json:"domain"`
		Schema       string        `json:"schema"`
		Version      uint32        `json:"version"`
		Action       string        `json:"action"`
		CommandID    string        `json:"command_id"`
		Mode         DeliveryMode  `json:"mode"`
		Target       CommandTarget `json:"target"`
		Source       CommandSource `json:"source"`
		Message      string        `json:"message"`
		Reference    *Reference    `json:"reference,omitempty"`
		CreatedAt    time.Time     `json:"created_at"`
		DeliverAfter time.Time     `json:"deliver_after"`
		ExpiresAt    time.Time     `json:"expires_at"`
	}{
		Domain:       CommandPayloadDigestDomainV1,
		Schema:       CommandPayloadDigestSchemaV1,
		Version:      command.Version,
		Action:       NudgeCommandAction,
		CommandID:    command.ID,
		Mode:         command.Mode,
		Target:       command.Target,
		Source:       command.Source,
		Message:      command.Message,
		Reference:    command.Reference,
		CreatedAt:    command.CreatedAt,
		DeliverAfter: command.DeliverAfter,
		ExpiresAt:    command.ExpiresAt,
	}
	wire, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(wire)
	return hex.EncodeToString(digest[:])
}

// EncodeCommandV1 validates command against the complete known-v1 contract and
// returns its canonical bounded JSON representation.
func EncodeCommandV1(command Command) ([]byte, error) {
	if err := validateCommandV1(command); err != nil {
		return nil, fmt.Errorf("encoding nudge command v1: %w", err)
	}
	wire, err := json.Marshal(command)
	if err != nil {
		return nil, fmt.Errorf("encoding nudge command v1: %w", err)
	}
	if len(wire) > MaxCommandBytes {
		return nil, fmt.Errorf("encoding nudge command v1: payload exceeds %d bytes", MaxCommandBytes)
	}
	return wire, nil
}

// DecodeCommand totally classifies arbitrary bytes. It returns a validated v1
// command, a typed dead-letter, or an opaque byte-identical newer command that
// requires an upgraded owner. It never treats ingress references as authority.
func DecodeCommand(wire []byte) CommandDecodeResult {
	if len(wire) > MaxCommandBytes {
		return deadLetterCommand(0, CommandDeadLetterPayloadTooLarge)
	}
	if len(wire) == 0 || !utf8.Valid(wire) {
		return deadLetterCommand(0, CommandDeadLetterMalformed)
	}
	trimmed := bytes.TrimSpace(wire)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return deadLetterCommand(0, CommandDeadLetterMalformed)
	}
	if err := validateCommandJSONUnicodeEscapes(trimmed); err != nil {
		return deadLetterCommand(0, CommandDeadLetterMalformed)
	}
	if err := validateCommandJSONStructure(trimmed); err != nil {
		return deadLetterCommand(0, CommandDeadLetterMalformed)
	}

	var envelope map[string]json.RawMessage
	if err := json.Unmarshal(trimmed, &envelope); err != nil {
		return deadLetterCommand(0, CommandDeadLetterMalformed)
	}
	versionRaw, ok := envelope["version"]
	if !ok {
		return deadLetterCommand(0, CommandDeadLetterMalformed)
	}
	versionValue, err := strconv.ParseUint(string(bytes.TrimSpace(versionRaw)), 10, 32)
	if err != nil {
		return deadLetterCommand(0, CommandDeadLetterMalformed)
	}
	version := uint32(versionValue)

	if version == 0 {
		return deadLetterCommand(version, CommandDeadLetterUnsupportedVersion)
	}
	if version > CommandVersion1 {
		routing, err := decodeCommandRoutingHeader(envelope)
		if err != nil {
			return deadLetterCommand(version, CommandDeadLetterMalformed)
		}
		return CommandDecodeResult{
			Disposition: CommandDecodeUpgradeRequired,
			Version:     version,
			Routing:     routing,
			Raw:         append([]byte(nil), wire...),
		}
	}
	if err := validateKnownCommandJSONFieldCase(trimmed); err != nil {
		return deadLetterCommand(version, CommandDeadLetterInvalidKnownVersion)
	}

	decoder := json.NewDecoder(bytes.NewReader(trimmed))
	decoder.DisallowUnknownFields()
	var command Command
	if err := decoder.Decode(&command); err != nil {
		return deadLetterCommand(version, CommandDeadLetterInvalidKnownVersion)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return deadLetterCommand(version, CommandDeadLetterInvalidKnownVersion)
	}
	if err := validateCommandV1(command); err != nil {
		return deadLetterCommand(version, CommandDeadLetterInvalidKnownVersion)
	}
	return CommandDecodeResult{
		Disposition: CommandDecodeDecoded,
		Version:     version,
		Routing:     commandRoutingHeader(command),
		Command:     command,
	}
}

func deadLetterCommand(version uint32, reason CommandDeadLetterReason) CommandDecodeResult {
	// Diagnostics cross an untrusted codec boundary. Keep only a fixed
	// classification-specific sentence: JSON errors and validation values may
	// otherwise echo attacker-controlled field names or command content.
	return CommandDecodeResult{
		Disposition:      CommandDecodeDeadLetter,
		Version:          version,
		DeadLetterReason: reason,
		Detail:           safeCommandDeadLetterDetail(reason),
	}
}

func safeCommandDeadLetterDetail(reason CommandDeadLetterReason) string {
	var detail string
	switch reason {
	case CommandDeadLetterPayloadTooLarge:
		detail = "nudge command payload exceeds the codec byte limit"
	case CommandDeadLetterMalformed:
		detail = "nudge command envelope is malformed"
	case CommandDeadLetterInvalidKnownVersion:
		detail = "nudge command version 1 failed strict validation"
	case CommandDeadLetterUnsupportedVersion:
		detail = "nudge command version is unsupported"
	default:
		detail = "nudge command was rejected by the codec"
	}
	if len(detail) > MaxCommandDeadLetterDetailBytes {
		detail = detail[:MaxCommandDeadLetterDetailBytes]
	}
	return detail
}

func commandRoutingHeader(command Command) CommandRoutingHeader {
	return CommandRoutingHeader{
		CommandID:        command.ID,
		Store:            command.Store,
		TargetSessionID:  command.Target.SessionID,
		IntentGeneration: command.Target.IntentGeneration,
		Sequence:         command.Order.Sequence,
		Revision:         command.Order.Revision,
	}
}

func decodeCommandRoutingHeader(envelope map[string]json.RawMessage) (CommandRoutingHeader, error) {
	commandID, err := decodeRequiredCommandString(envelope, "id", "command id")
	if err != nil {
		return CommandRoutingHeader{}, err
	}
	store, err := decodeRequiredCommandObject(envelope, "store")
	if err != nil {
		return CommandRoutingHeader{}, err
	}
	storeUUID, err := decodeRequiredCommandString(store, "store_uuid", "store uuid")
	if err != nil {
		return CommandRoutingHeader{}, err
	}
	restoreEpoch, err := decodeRequiredCommandUint(store, "restore_epoch")
	if err != nil || restoreEpoch == 0 {
		return CommandRoutingHeader{}, errors.New("restore epoch must be positive")
	}
	target, err := decodeRequiredCommandObject(envelope, "target")
	if err != nil {
		return CommandRoutingHeader{}, err
	}
	targetSessionID, err := decodeRequiredCommandString(target, "session_id", "target session id")
	if err != nil {
		return CommandRoutingHeader{}, err
	}
	intentGeneration, err := decodeRequiredCommandUint(target, "intent_generation")
	if err != nil || intentGeneration == 0 {
		return CommandRoutingHeader{}, errors.New("intent generation must be positive")
	}
	order, err := decodeRequiredCommandObject(envelope, "order")
	if err != nil {
		return CommandRoutingHeader{}, err
	}
	sequence, err := decodeRequiredCommandUint(order, "sequence")
	if err != nil || sequence == 0 {
		return CommandRoutingHeader{}, errors.New("sequence must be positive")
	}
	revision, err := decodeRequiredCommandUint(order, "revision")
	if err != nil || revision == 0 {
		return CommandRoutingHeader{}, errors.New("revision must be positive")
	}
	return CommandRoutingHeader{
		CommandID:        commandID,
		Store:            CommandStoreBinding{StoreUUID: storeUUID, RestoreEpoch: restoreEpoch},
		TargetSessionID:  targetSessionID,
		IntentGeneration: intentGeneration,
		Sequence:         sequence,
		Revision:         revision,
	}, nil
}

func decodeRequiredCommandObject(parent map[string]json.RawMessage, name string) (map[string]json.RawMessage, error) {
	raw, ok := parent[name]
	if !ok {
		return nil, fmt.Errorf("required command object %q is missing", name)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return nil, fmt.Errorf("required command object %q is invalid", name)
	}
	return object, nil
}

func decodeRequiredCommandString(parent map[string]json.RawMessage, key, fieldName string) (string, error) {
	raw, ok := parent[key]
	if !ok {
		return "", fmt.Errorf("required command field %q is missing", key)
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return "", fmt.Errorf("required command field %q is not a string", key)
	}
	if err := validateCommandIdentity(fieldName, value); err != nil {
		return "", err
	}
	return value, nil
}

func decodeRequiredCommandUint(parent map[string]json.RawMessage, key string) (uint64, error) {
	raw, ok := parent[key]
	if !ok {
		return 0, fmt.Errorf("required command field %q is missing", key)
	}
	value, err := strconv.ParseUint(string(bytes.TrimSpace(raw)), 10, 64)
	if err != nil {
		return 0, fmt.Errorf("required command field %q is not an unsigned integer", key)
	}
	return value, nil
}

func validateKnownCommandJSONFieldCase(wire []byte) error {
	return validateKnownCommandJSONValueCase(json.RawMessage(wire), reflect.TypeOf(Command{}))
}

func validateKnownCommandJSONValueCase(raw json.RawMessage, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
			return errors.New("known command pointer field must be omitted instead of null")
		}
		typ = typ.Elem()
	}
	if typ == reflect.TypeOf(time.Time{}) || typ.Kind() != reflect.Struct {
		return nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil || object == nil {
		return errors.New("known command struct field must be a JSON object")
	}
	exact := make(map[string]reflect.Type, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		field := typ.Field(i)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		if name == "-" {
			continue
		}
		if name == "" {
			name = field.Name
		}
		exact[name] = field.Type
	}
	for key, value := range object {
		fieldType, ok := exact[key]
		if ok {
			if err := validateKnownCommandJSONValueCase(value, fieldType); err != nil {
				return err
			}
			continue
		}
		for canonical := range exact {
			if strings.EqualFold(key, canonical) {
				return fmt.Errorf("known command field %q does not use canonical case", key)
			}
		}
	}
	return nil
}

func validateCommandV1(command Command) error {
	if command.Version != CommandVersion1 {
		return fmt.Errorf("version = %d, want %d", command.Version, CommandVersion1)
	}
	if err := validateCommandIdentity("command id", command.ID); err != nil {
		return err
	}
	if !knownCommandState(command.State) {
		return fmt.Errorf("unknown command state %q", command.State)
	}
	if !knownDeliveryMode(command.Mode) {
		return fmt.Errorf("unknown delivery mode %q", command.Mode)
	}
	if err := validateCommandTarget(command.Mode, command.Target); err != nil {
		return err
	}
	if err := ValidateCommandStoreBinding(command.Store); err != nil {
		return err
	}
	if command.Order.Sequence == 0 {
		return errors.New("order sequence must be positive")
	}
	if command.Order.Revision == 0 {
		return errors.New("order revision must be positive")
	}
	if err := validateTrustedIngressReference(command); err != nil {
		return err
	}
	if err := validateCommandSourceReference(command.Source, command.Reference); err != nil {
		return err
	}
	if command.Message == "" {
		return errors.New("message is empty")
	}
	if !utf8.ValidString(command.Message) {
		return errors.New("message is not valid UTF-8")
	}
	if strings.IndexByte(command.Message, 0) >= 0 {
		return errors.New("message contains a NUL byte")
	}
	if err := validateCommandTime("created_at", command.CreatedAt); err != nil {
		return err
	}
	if err := validateCommandTime("deliver_after", command.DeliverAfter); err != nil {
		return err
	}
	if err := validateCommandTime("expires_at", command.ExpiresAt); err != nil {
		return err
	}
	if command.DeliverAfter.Before(command.CreatedAt) {
		return errors.New("deliver_after precedes created_at")
	}
	if command.Mode == DeliveryModeImmediate && !command.DeliverAfter.Equal(command.CreatedAt) {
		return errors.New("immediate command must be deliverable at creation")
	}
	if !command.ExpiresAt.After(command.DeliverAfter) {
		return errors.New("expires_at must be after deliver_after")
	}
	if command.CreatedAt.Before(command.TrustedIngress.IssuedAt) {
		return errors.New("created_at precedes trusted ingress issuance")
	}
	if !command.CreatedAt.Before(command.TrustedIngress.ExpiresAt) {
		return errors.New("created_at is not before trusted ingress expiry")
	}
	if err := validateCommandBinding(command); err != nil {
		return err
	}

	if command.Retry != nil {
		if err := validateCommandRetry(command, *command.Retry); err != nil {
			return err
		}
	}
	if command.Claim != nil {
		if err := validateCommandClaim(command, *command.Claim); err != nil {
			return err
		}
	}
	if command.Terminal != nil {
		if err := validateCommandTerminal(command, *command.Terminal); err != nil {
			return err
		}
	}

	switch command.State {
	case CommandStatePending, CommandStateUpgradeRequired:
		if command.Claim != nil || command.Terminal != nil {
			return fmt.Errorf("state %q cannot carry claim or terminal data", command.State)
		}
		if command.State == CommandStatePending && command.Retry != nil && command.Retry.NextEligibleAt == nil {
			return errors.New("retry-pending command is missing next eligibility")
		}
		if command.State == CommandStatePending && command.Retry != nil && command.Retry.ErrorClass == "" {
			return errors.New("retry-pending command is missing error evidence")
		}
		if command.State == CommandStateUpgradeRequired && command.Retry != nil {
			return errors.New("upgrade_required command cannot carry retry data")
		}
	case CommandStateInFlight:
		if command.Claim == nil {
			return errors.New("in_flight command is missing claim")
		}
		if command.Retry == nil {
			return errors.New("in_flight command is missing attempt evidence")
		}
		if command.Terminal != nil {
			return errors.New("in_flight command carries terminal data")
		}
		if command.Retry != nil && command.Retry.NextEligibleAt != nil {
			return errors.New("in_flight command carries retry eligibility")
		}
		if command.Retry != nil && !command.Retry.LastAttemptAt.Equal(command.Claim.ClaimedAt) {
			return errors.New("in_flight attempt time does not match claim time")
		}
		if command.Retry != nil && command.Retry.ErrorClass != CommandErrorClassNone {
			return errors.New("in_flight current attempt carries a concluded error")
		}
	default:
		if command.Claim != nil {
			return fmt.Errorf("terminal state %q carries an active claim", command.State)
		}
		if command.Terminal == nil {
			return fmt.Errorf("terminal state %q is missing terminal data", command.State)
		}
		if command.Retry != nil && command.Retry.NextEligibleAt != nil {
			return fmt.Errorf("terminal state %q carries retry eligibility", command.State)
		}
	}
	return nil
}

func knownCommandState(state CommandState) bool {
	switch state {
	case CommandStatePending,
		CommandStateInFlight,
		CommandStateDelivered,
		CommandStateInjectedUnconfirmed,
		CommandStateDeliveryUnknown,
		CommandStateExpired,
		CommandStateSuperseded,
		CommandStateDeadLettered,
		CommandStateUpgradeRequired:
		return true
	default:
		return false
	}
}

func knownDeliveryMode(mode DeliveryMode) bool {
	switch mode {
	case DeliveryModeQueue, DeliveryModeWaitIdle, DeliveryModeImmediate:
		return true
	default:
		return false
	}
}

func validateCommandSourceReference(source CommandSource, reference *Reference) error {
	if err := validateCommandIdentity("source", string(source)); err != nil {
		return err
	}
	if reference != nil {
		if err := validateCommandIdentity("reference kind", reference.Kind); err != nil {
			return err
		}
		if err := validateCommandIdentity("reference id", reference.ID); err != nil {
			return err
		}
	}
	switch source {
	case CommandSourceSession, CommandSourceQueue, CommandSourceController:
		if reference != nil {
			return fmt.Errorf("source %q cannot carry a reference", source)
		}
	case CommandSourceMail, CommandSourceSling:
		if reference == nil || reference.Kind != CommandReferenceBead {
			return fmt.Errorf("source %q requires a bead reference", source)
		}
	case CommandSourceWait:
		if reference == nil || reference.Kind != CommandReferenceWait {
			return errors.New("wait source requires a wait reference")
		}
	default:
		return fmt.Errorf("unknown command source %q", source)
	}
	return nil
}

func validateCommandBinding(command Command) error {
	if command.Binding == nil {
		if command.Target.Policy == TargetPolicyExactLaunch {
			return errors.New("exact_launch target is missing its commit-time binding")
		}
		return nil
	}
	binding := *command.Binding
	if err := validateCommandIdentity("binding launch identity", binding.LaunchIdentity); err != nil {
		return err
	}
	if err := validateCommandTime("binding bound_at", binding.BoundAt); err != nil {
		return err
	}
	if binding.BoundAt.Before(command.CreatedAt) {
		return errors.New("binding bound_at precedes created_at")
	}
	if !binding.BoundAt.Before(command.ExpiresAt) {
		return errors.New("binding bound_at is not before expires_at")
	}
	if command.Target.Policy == TargetPolicyExactLaunch && binding.LaunchIdentity != command.Target.LaunchIdentity {
		return errors.New("binding launch does not match exact target launch")
	}
	if command.Target.Policy == TargetPolicyExactLaunch && !binding.BoundAt.Equal(command.CreatedAt) {
		return errors.New("exact_launch binding must be captured at command creation")
	}
	return nil
}

func validateCommandTarget(mode DeliveryMode, target CommandTarget) error {
	if err := validateCommandIdentity("target session id", target.SessionID); err != nil {
		return err
	}
	if target.IntentGeneration == 0 {
		return errors.New("target intent generation must be positive")
	}
	switch target.Policy {
	case TargetPolicyContinuation:
		if mode == DeliveryModeImmediate {
			return errors.New("immediate mode requires exact_launch target policy")
		}
		if err := validateCommandIdentity("continuation identity", target.ContinuationIdentity); err != nil {
			return err
		}
		if target.LaunchIdentity != "" {
			return errors.New("continuation target carries launch identity")
		}
	case TargetPolicyExactLaunch:
		if mode != DeliveryModeImmediate {
			return fmt.Errorf("mode %q requires continuation target policy", mode)
		}
		if target.ContinuationIdentity != "" {
			return errors.New("exact_launch target carries continuation identity")
		}
		if err := validateCommandIdentity("launch identity", target.LaunchIdentity); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown target policy %q", target.Policy)
	}
	return nil
}

func validateTrustedIngressReference(command Command) error {
	ref := command.TrustedIngress
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "trusted ingress issuer", value: ref.Issuer},
		{name: "trusted ingress reference id", value: ref.ReferenceID},
		{name: "trusted ingress principal id", value: ref.PrincipalID},
		{name: "trusted ingress tenant scope", value: ref.TenantScope},
		{name: "trusted ingress city scope", value: ref.CityScope},
		{name: "trusted ingress credential class", value: ref.CredentialClass},
		{name: "trusted ingress policy version", value: ref.PolicyVersion},
		{name: "trusted ingress policy decision id", value: ref.PolicyDecisionID},
		{name: "trusted ingress action", value: ref.Action},
		{name: "trusted ingress target session id", value: ref.TargetSessionID},
		{name: "trusted ingress payload digest", value: ref.PayloadDigest},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return err
		}
	}
	if ref.Action != NudgeCommandAction {
		return fmt.Errorf("trusted ingress action must be %q", NudgeCommandAction)
	}
	if ref.TargetSessionID != command.Target.SessionID {
		return errors.New("trusted ingress target session does not match command target")
	}
	if len(ref.PayloadDigest) != sha256.Size*2 {
		return errors.New("trusted ingress payload digest is not a SHA-256 hex digest")
	}
	if _, err := hex.DecodeString(ref.PayloadDigest); err != nil || strings.ToLower(ref.PayloadDigest) != ref.PayloadDigest {
		return errors.New("trusted ingress payload digest is not canonical lowercase hex")
	}
	if ref.PayloadDigest != ComputeCommandPayloadDigest(command) {
		return errors.New("trusted ingress payload digest does not cover the command")
	}
	if err := validateCommandTime("trusted ingress issued_at", ref.IssuedAt); err != nil {
		return err
	}
	if err := validateCommandTime("trusted ingress expires_at", ref.ExpiresAt); err != nil {
		return err
	}
	if !ref.ExpiresAt.After(ref.IssuedAt) {
		return errors.New("trusted ingress expires_at must be after issued_at")
	}
	return nil
}

func validateCommandRetry(command Command, retry CommandRetry) error {
	if retry.AttemptCount == 0 {
		return errors.New("retry attempt count must be positive")
	}
	if err := validateCommandTime("retry last_attempt_at", retry.LastAttemptAt); err != nil {
		return err
	}
	if err := validateCommandAttemptEvidence(
		command,
		"retry",
		retry.ClaimID,
		retry.OperationID,
		retry.AttemptID,
		retry.BoundLaunchIdentity,
		retry.AuthorizationDecisionID,
		retry.AuthorizationPolicyVersion,
		retry.LastAttemptAt,
	); err != nil {
		return err
	}
	if retry.LastAttemptAt.Before(command.CreatedAt) {
		return errors.New("retry last_attempt_at precedes created_at")
	}
	if retry.LastAttemptAt.Before(command.DeliverAfter) {
		return errors.New("retry last_attempt_at precedes deliver_after")
	}
	if !retry.LastAttemptAt.Before(command.ExpiresAt) {
		return errors.New("retry last_attempt_at is not before expires_at")
	}
	if command.Claim != nil && retry.LastAttemptAt.After(command.Claim.ClaimedAt) {
		return errors.New("retry last_attempt_at follows the current claim")
	}
	if command.Terminal != nil && retry.LastAttemptAt.After(command.Terminal.At) {
		return errors.New("retry last_attempt_at follows terminal time")
	}
	if retry.NextEligibleAt != nil {
		if err := validateCommandTime("retry next_eligible_at", *retry.NextEligibleAt); err != nil {
			return err
		}
		if !retry.NextEligibleAt.After(retry.LastAttemptAt) {
			return errors.New("retry next_eligible_at must be after last_attempt_at")
		}
		if retry.NextEligibleAt.Before(command.DeliverAfter) {
			return errors.New("retry next_eligible_at precedes deliver_after")
		}
		if !retry.NextEligibleAt.Before(command.ExpiresAt) {
			return errors.New("retry next_eligible_at is not before expires_at")
		}
	}
	if !knownCommandErrorClass(retry.ErrorClass) {
		return fmt.Errorf("unknown retry error class %q", retry.ErrorClass)
	}
	if (retry.ErrorClass == CommandErrorClassNone) != (retry.ErrorDetail == "") {
		return errors.New("retry error class and detail must be both present or both absent")
	}
	if retry.ErrorClass != CommandErrorClassNone {
		if retry.ErrorClass != CommandErrorClassProviderBusy && retry.ErrorClass != CommandErrorClassProviderUnavailable {
			return fmt.Errorf("retry error class %q is not retryable", retry.ErrorClass)
		}
		if err := validateCommandDetail("retry error detail", retry.ErrorDetail, MaxCommandRetryErrorDetailBytes); err != nil {
			return err
		}
	}
	return nil
}

func validateCommandAttemptEvidence(
	command Command,
	prefix string,
	claimID string,
	operationID string,
	attemptID string,
	boundLaunchIdentity string,
	authorizationDecisionID string,
	authorizationPolicyVersion string,
	attemptedAt time.Time,
) error {
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: prefix + " claim id", value: claimID},
		{name: prefix + " operation id", value: operationID},
		{name: prefix + " attempt id", value: attemptID},
		{name: prefix + " bound launch identity", value: boundLaunchIdentity},
		{name: prefix + " authorization decision id", value: authorizationDecisionID},
		{name: prefix + " authorization policy version", value: authorizationPolicyVersion},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return err
		}
	}
	if operationID != command.ID {
		return fmt.Errorf("%s operation id does not match command id", prefix)
	}
	if command.Binding == nil {
		return fmt.Errorf("%s evidence is missing durable launch binding", prefix)
	}
	if boundLaunchIdentity != command.Binding.LaunchIdentity {
		return fmt.Errorf("%s bound launch does not match durable binding", prefix)
	}
	if attemptedAt.Before(command.Binding.BoundAt) {
		return fmt.Errorf("%s attempt precedes durable binding", prefix)
	}
	return nil
}

func validateCommandClaim(command Command, claim CommandClaim) error {
	if err := validateCommandIdentity("claim id", claim.ID); err != nil {
		return err
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "claim owner id", value: claim.OwnerID},
		{name: "claim operation id", value: claim.OperationID},
		{name: "claim attempt id", value: claim.AttemptID},
		{name: "claim bound launch identity", value: claim.BoundLaunchIdentity},
		{name: "claim authorization decision id", value: claim.AuthorizationDecisionID},
		{name: "claim authorization policy version", value: claim.AuthorizationPolicyVersion},
	} {
		if err := validateCommandIdentity(field.name, field.value); err != nil {
			return err
		}
	}
	if claim.OperationID != command.ID {
		return errors.New("claim operation id does not match command id")
	}
	if command.Binding == nil {
		return errors.New("claim is missing durable launch binding")
	}
	if claim.BoundLaunchIdentity != command.Binding.LaunchIdentity {
		return errors.New("claim bound launch does not match durable binding")
	}
	if err := validateCommandTime("claimed_at", claim.ClaimedAt); err != nil {
		return err
	}
	if err := validateCommandTime("lease_until", claim.LeaseUntil); err != nil {
		return err
	}
	if claim.ClaimedAt.Before(command.CreatedAt) {
		return errors.New("claimed_at precedes created_at")
	}
	if claim.ClaimedAt.Before(command.DeliverAfter) {
		return errors.New("claimed_at precedes deliver_after")
	}
	if !claim.ClaimedAt.Before(command.ExpiresAt) {
		return errors.New("claimed_at is not before expires_at")
	}
	if !claim.LeaseUntil.After(claim.ClaimedAt) {
		return errors.New("lease_until must be after claimed_at")
	}
	if claim.ClaimedAt.Before(command.Binding.BoundAt) {
		return errors.New("claimed_at precedes durable binding")
	}
	if command.Retry != nil &&
		(claim.ID != command.Retry.ClaimID ||
			claim.OperationID != command.Retry.OperationID ||
			claim.AttemptID != command.Retry.AttemptID ||
			claim.BoundLaunchIdentity != command.Retry.BoundLaunchIdentity ||
			claim.AuthorizationDecisionID != command.Retry.AuthorizationDecisionID ||
			claim.AuthorizationPolicyVersion != command.Retry.AuthorizationPolicyVersion) {
		return errors.New("claim and current attempt evidence do not match")
	}
	return nil
}

func validateCommandTerminal(command Command, terminal CommandTerminal) error {
	if err := validateCommandTime("terminal at", terminal.At); err != nil {
		return err
	}
	if terminal.At.Before(command.CreatedAt) {
		return errors.New("terminal at precedes created_at")
	}
	rule, ok := commandTerminalRuleFor(terminal.ActionResult)
	if !ok {
		return fmt.Errorf("unknown terminal action result %q", terminal.ActionResult)
	}
	if command.State != rule.state || terminal.ProviderStage != rule.providerStage || terminal.Completion != rule.completion {
		return fmt.Errorf(
			"action result %q requires state/stage/completion %q/%q/%q",
			terminal.ActionResult,
			rule.state,
			rule.providerStage,
			rule.completion,
		)
	}
	if terminal.ActionResult == CommandActionResultExpired && terminal.At.Before(command.ExpiresAt) {
		return errors.New("expired terminal result precedes command expiry")
	}
	if !rule.acceptsErrorClass(terminal.ErrorClass) {
		return fmt.Errorf("action result %q rejects error class %q", terminal.ActionResult, terminal.ErrorClass)
	}
	if terminal.ErrorClass == CommandErrorClassNone {
		if terminal.Detail != "" {
			return errors.New("successful terminal result cannot carry error detail")
		}
	} else {
		if terminal.Detail == "" {
			return errors.New("terminal error class is missing sanitized detail")
		}
		if err := validateCommandDetail("terminal detail", terminal.Detail, MaxCommandTerminalDetailBytes); err != nil {
			return err
		}
	}
	switch rule.evidence {
	case commandTerminalEvidenceAttempt:
		return validateTerminalAttemptEvidence(command, terminal)
	case commandTerminalEvidenceAuthorization:
		return validateTerminalAuthorizationEvidence(command, terminal)
	case commandTerminalEvidenceOptionalAttempt:
		if command.Retry != nil {
			return validateTerminalAttemptEvidence(command, terminal)
		}
		return validateTerminalWithoutAttempt(command, terminal)
	case commandTerminalEvidenceNone:
		return validateTerminalWithoutAttempt(command, terminal)
	default:
		return errors.New("terminal action result has no evidence rule")
	}
}

func knownCommandErrorClass(errorClass CommandErrorClass) bool {
	switch errorClass {
	case CommandErrorClassNone,
		CommandErrorClassProviderBusy,
		CommandErrorClassProviderUnavailable,
		CommandErrorClassProviderRejected,
		CommandErrorClassProviderAmbiguous,
		CommandErrorClassTargetMissing,
		CommandErrorClassAuthorizationDenied,
		CommandErrorClassRetryExhausted,
		CommandErrorClassExpired,
		CommandErrorClassSuperseded,
		CommandErrorClassInvalidReference,
		CommandErrorClassInvalidCommand:
		return true
	default:
		return false
	}
}

func validateCommandDetail(name, detail string, maxBytes int) error {
	if !utf8.ValidString(detail) || len(detail) > maxBytes {
		return fmt.Errorf("%s must be valid UTF-8 and at most %d bytes", name, maxBytes)
	}
	for _, r := range detail {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

type commandTerminalEvidence uint8

const (
	commandTerminalEvidenceNone commandTerminalEvidence = iota
	commandTerminalEvidenceAttempt
	commandTerminalEvidenceAuthorization
	commandTerminalEvidenceOptionalAttempt
)

type commandTerminalRule struct {
	result        CommandActionResult
	state         CommandState
	providerStage ProviderStage
	completion    CompletionState
	errorClasses  [2]CommandErrorClass
	evidence      commandTerminalEvidence
}

func commandTerminalRuleFor(result CommandActionResult) (commandTerminalRule, bool) {
	rules := [...]commandTerminalRule{
		{
			result:        CommandActionResultDelivered,
			state:         CommandStateDelivered,
			providerStage: ProviderStageAccepted,
			completion:    CompletionStateCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassNone},
			evidence:      commandTerminalEvidenceAttempt,
		},
		{
			result:        CommandActionResultDuplicate,
			state:         CommandStateDelivered,
			providerStage: ProviderStageAccepted,
			completion:    CompletionStateCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassNone},
			evidence:      commandTerminalEvidenceAttempt,
		},
		{
			result:        CommandActionResultInjectedUnconfirmed,
			state:         CommandStateInjectedUnconfirmed,
			providerStage: ProviderStageAccepted,
			completion:    CompletionStateCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassNone},
			evidence:      commandTerminalEvidenceAttempt,
		},
		{
			result:        CommandActionResultDeliveryUnknown,
			state:         CommandStateDeliveryUnknown,
			providerStage: ProviderStageMayHaveEntered,
			completion:    CompletionStateUnknown,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassProviderAmbiguous},
			evidence:      commandTerminalEvidenceAttempt,
		},
		{
			result:        CommandActionResultExpired,
			state:         CommandStateExpired,
			providerStage: ProviderStageNotEntered,
			completion:    CompletionStateNotCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassExpired},
			evidence:      commandTerminalEvidenceOptionalAttempt,
		},
		{
			result:        CommandActionResultSuperseded,
			state:         CommandStateSuperseded,
			providerStage: ProviderStageNotEntered,
			completion:    CompletionStateNotCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassSuperseded},
			evidence:      commandTerminalEvidenceOptionalAttempt,
		},
		{
			result:        CommandActionResultTargetMissing,
			state:         CommandStateSuperseded,
			providerStage: ProviderStageRejected,
			completion:    CompletionStateNotCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassTargetMissing},
			evidence:      commandTerminalEvidenceAttempt,
		},
		{
			result:        CommandActionResultRejected,
			state:         CommandStateDeadLettered,
			providerStage: ProviderStageRejected,
			completion:    CompletionStateNotCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassProviderRejected},
			evidence:      commandTerminalEvidenceAttempt,
		},
		{
			result:        CommandActionResultAuthorizationDenied,
			state:         CommandStateDeadLettered,
			providerStage: ProviderStageNotEntered,
			completion:    CompletionStateNotCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassAuthorizationDenied},
			evidence:      commandTerminalEvidenceAuthorization,
		},
		{
			result:        CommandActionResultRetryExhausted,
			state:         CommandStateDeadLettered,
			providerStage: ProviderStageRejected,
			completion:    CompletionStateNotCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassRetryExhausted},
			evidence:      commandTerminalEvidenceAttempt,
		},
		{
			result:        CommandActionResultDeadLettered,
			state:         CommandStateDeadLettered,
			providerStage: ProviderStageNotEntered,
			completion:    CompletionStateNotCompleted,
			errorClasses:  [2]CommandErrorClass{CommandErrorClassInvalidReference, CommandErrorClassInvalidCommand},
			evidence:      commandTerminalEvidenceNone,
		},
	}
	for _, rule := range rules {
		if rule.result == result {
			return rule, true
		}
	}
	return commandTerminalRule{}, false
}

func (rule commandTerminalRule) acceptsErrorClass(errorClass CommandErrorClass) bool {
	return errorClass == rule.errorClasses[0] || (rule.errorClasses[1] != CommandErrorClassNone && errorClass == rule.errorClasses[1])
}

func validateTerminalAttemptEvidence(command Command, terminal CommandTerminal) error {
	if command.Retry == nil {
		return errors.New("terminal attempt result is missing attempt evidence")
	}
	if err := validateCommandAttemptEvidence(
		command,
		"terminal",
		terminal.ClaimID,
		terminal.OperationID,
		terminal.AttemptID,
		terminal.BoundLaunchIdentity,
		terminal.AuthorizationDecisionID,
		terminal.AuthorizationPolicyVersion,
		terminal.At,
	); err != nil {
		return err
	}
	retry := command.Retry
	if terminal.ActionResult == CommandActionResultRetryExhausted {
		if retry.ErrorClass != CommandErrorClassProviderBusy && retry.ErrorClass != CommandErrorClassProviderUnavailable {
			return errors.New("retry_exhausted terminal result lacks a retryable last error")
		}
	} else if terminal.ActionResult != CommandActionResultExpired && terminal.ActionResult != CommandActionResultSuperseded && retry.ErrorClass != CommandErrorClassNone {
		return errors.New("terminal provider result carries stale retry-error evidence")
	}
	if terminal.At.Before(retry.LastAttemptAt) {
		return errors.New("terminal time precedes last attempt")
	}
	if terminal.ClaimID != retry.ClaimID ||
		terminal.OperationID != retry.OperationID ||
		terminal.AttemptID != retry.AttemptID ||
		terminal.BoundLaunchIdentity != retry.BoundLaunchIdentity ||
		terminal.AuthorizationDecisionID != retry.AuthorizationDecisionID ||
		terminal.AuthorizationPolicyVersion != retry.AuthorizationPolicyVersion {
		return errors.New("terminal and last-attempt evidence do not match")
	}
	return nil
}

func validateTerminalAuthorizationEvidence(command Command, terminal CommandTerminal) error {
	if command.Retry != nil {
		return errors.New("authorization denial cannot carry provider attempt evidence")
	}
	if terminal.ClaimID != "" || terminal.OperationID != "" || terminal.AttemptID != "" || terminal.BoundLaunchIdentity != "" {
		return errors.New("authorization denial cannot carry claim or provider-attempt correlation")
	}
	if err := validateCommandIdentity("terminal authorization decision id", terminal.AuthorizationDecisionID); err != nil {
		return err
	}
	if err := validateCommandIdentity("terminal authorization policy version", terminal.AuthorizationPolicyVersion); err != nil {
		return err
	}
	if command.Target.Policy == TargetPolicyContinuation && command.Binding != nil {
		return errors.New("continuation authorization denial cannot carry a claim-time binding")
	}
	if terminal.At.Before(command.DeliverAfter) || !terminal.At.Before(command.ExpiresAt) {
		return errors.New("authorization denial must occur inside the delivery window")
	}
	return nil
}

func validateTerminalWithoutAttempt(command Command, terminal CommandTerminal) error {
	if command.Retry != nil {
		return errors.New("non-attempt terminal result cannot carry attempt evidence")
	}
	if terminal.ClaimID != "" || terminal.OperationID != "" || terminal.AttemptID != "" || terminal.BoundLaunchIdentity != "" ||
		terminal.AuthorizationDecisionID != "" || terminal.AuthorizationPolicyVersion != "" {
		return errors.New("non-attempt terminal result carries claim, attempt, binding, or authorization evidence")
	}
	return nil
}

func validateCommandIdentity(name, value string) error {
	if value == "" {
		return fmt.Errorf("%s is empty", name)
	}
	if len(value) > maxCommandIdentityBytes {
		return fmt.Errorf("%s exceeds %d bytes", name, maxCommandIdentityBytes)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", name)
	}
	if strings.TrimSpace(value) != value {
		return fmt.Errorf("%s is not canonical", name)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("%s contains a control character", name)
		}
	}
	return nil
}

func validateCommandTime(name string, value time.Time) error {
	if value.IsZero() {
		return fmt.Errorf("%s is zero", name)
	}
	if value.Location() != time.UTC {
		return fmt.Errorf("%s is not canonical UTC", name)
	}
	if value != value.Round(0) {
		return fmt.Errorf("%s contains process-local monotonic time", name)
	}
	if value.Year() < 0 || value.Year() > 9999 {
		return fmt.Errorf("%s cannot be represented as RFC 3339 JSON", name)
	}
	return nil
}

func validateCommandJSONStructure(wire []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(wire))
	decoder.UseNumber()
	budget := commandJSONBudget{}
	if err := validateCommandJSONValue(decoder, 1, &budget); err != nil {
		return err
	}
	return requireJSONEOF(decoder)
}

func validateCommandJSONUnicodeEscapes(wire []byte) error {
	for i := 0; i < len(wire); {
		if wire[i] != '"' {
			i++
			continue
		}
		i++
		closed := false
		for i < len(wire) {
			switch wire[i] {
			case '"':
				i++
				closed = true
			case '\\':
				if i+1 >= len(wire) {
					return errors.New("unterminated JSON string escape")
				}
				if wire[i+1] != 'u' {
					i += 2
					continue
				}
				codeUnit, ok := decodeCommandJSONHexCodeUnit(wire, i+2)
				if !ok {
					return errors.New("invalid JSON Unicode escape")
				}
				i += 6
				switch {
				case codeUnit >= 0xd800 && codeUnit <= 0xdbff:
					if i+6 > len(wire) || wire[i] != '\\' || wire[i+1] != 'u' {
						return errors.New("unpaired high surrogate in JSON string")
					}
					low, ok := decodeCommandJSONHexCodeUnit(wire, i+2)
					if !ok || low < 0xdc00 || low > 0xdfff {
						return errors.New("unpaired high surrogate in JSON string")
					}
					i += 6
				case codeUnit >= 0xdc00 && codeUnit <= 0xdfff:
					return errors.New("unpaired low surrogate in JSON string")
				}
			default:
				i++
			}
			if closed {
				break
			}
		}
		if !closed {
			return errors.New("unterminated JSON string")
		}
	}
	return nil
}

func decodeCommandJSONHexCodeUnit(wire []byte, start int) (uint16, bool) {
	if start+4 > len(wire) {
		return 0, false
	}
	var value uint16
	for _, b := range wire[start : start+4] {
		value <<= 4
		switch {
		case b >= '0' && b <= '9':
			value |= uint16(b - '0')
		case b >= 'a' && b <= 'f':
			value |= uint16(b-'a') + 10
		case b >= 'A' && b <= 'F':
			value |= uint16(b-'A') + 10
		default:
			return 0, false
		}
	}
	return value, true
}

type commandJSONBudget struct {
	members int
}

func validateCommandJSONValue(decoder *json.Decoder, depth int, budget *commandJSONBudget) error {
	if depth > MaxCommandJSONDepth {
		return fmt.Errorf("command JSON depth exceeds %d", MaxCommandJSONDepth)
	}
	token, err := decoder.Token()
	if err != nil {
		return fmt.Errorf("reading JSON token: %w", err)
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		seen := make(map[string]struct{})
		for decoder.More() {
			if err := consumeCommandJSONMember(budget); err != nil {
				return err
			}
			keyToken, err := decoder.Token()
			if err != nil {
				return fmt.Errorf("reading JSON object key: %w", err)
			}
			key, ok := keyToken.(string)
			if !ok {
				return errors.New("JSON object key is not a string")
			}
			folded := commandJSONFoldKey(key)
			if _, duplicate := seen[folded]; duplicate {
				return errors.New("duplicate or case-fold-colliding JSON object key")
			}
			seen[folded] = struct{}{}
			if err := validateCommandJSONValue(decoder, depth+1, budget); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("closing JSON object: %w", err)
		}
	case '[':
		for decoder.More() {
			if err := consumeCommandJSONMember(budget); err != nil {
				return err
			}
			if err := validateCommandJSONValue(decoder, depth+1, budget); err != nil {
				return err
			}
		}
		if _, err := decoder.Token(); err != nil {
			return fmt.Errorf("closing JSON array: %w", err)
		}
	default:
		return fmt.Errorf("unexpected JSON delimiter %q", delim)
	}
	return nil
}

func commandJSONFoldKey(key string) string {
	var folded strings.Builder
	folded.Grow(len(key))
	for _, r := range key {
		canonical := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < canonical {
				canonical = next
			}
		}
		folded.WriteRune(canonical)
	}
	return folded.String()
}

func consumeCommandJSONMember(budget *commandJSONBudget) error {
	budget.members++
	if budget.members > MaxCommandJSONMembers {
		return fmt.Errorf("command JSON members exceed %d", MaxCommandJSONMembers)
	}
	return nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var trailing any
	if err := decoder.Decode(&trailing); errors.Is(err, io.EOF) {
		return nil
	} else if err != nil {
		return fmt.Errorf("reading trailing JSON: %w", err)
	}
	return errors.New("command contains a trailing JSON value")
}
