package nudgequeue

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func TestCommandV1RoundTripsEveryLifecycleState(t *testing.T) {
	states := []CommandState{
		CommandStatePending,
		CommandStateInFlight,
		CommandStateDelivered,
		CommandStateInjectedUnconfirmed,
		CommandStateDeliveryUnknown,
		CommandStateExpired,
		CommandStateSuperseded,
		CommandStateDeadLettered,
		CommandStateUpgradeRequired,
	}
	for _, state := range states {
		t.Run(string(state), func(t *testing.T) {
			want := validCommandV1(state)
			wire, err := EncodeCommandV1(want)
			if err != nil {
				t.Fatalf("EncodeCommandV1(%s): %v", state, err)
			}
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDecoded {
				t.Fatalf("DecodeCommand disposition = %q, want %q; reason=%q detail=%q", got.Disposition, CommandDecodeDecoded, got.DeadLetterReason, got.Detail)
			}
			if got.Version != CommandVersion1 {
				t.Fatalf("decoded version = %d, want %d", got.Version, CommandVersion1)
			}
			if got.Routing != commandRoutingHeader(want) {
				t.Fatalf("decoded routing = %#v, want %#v", got.Routing, commandRoutingHeader(want))
			}
			if !reflect.DeepEqual(got.Command, want) {
				t.Fatalf("round trip mismatch:\n got=%#v\nwant=%#v", got.Command, want)
			}
			reencoded, err := EncodeCommandV1(got.Command)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			if !bytes.Equal(reencoded, wire) {
				t.Fatalf("canonical re-encode differs:\n got=%s\nwant=%s", reencoded, wire)
			}
		})
	}
}

func TestCommandV1CanonicalWireGolden(t *testing.T) {
	command := validCommandV1(CommandStatePending)
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	const want = `{"version":1,"id":"command-1","state":"pending","mode":"queue","target":{"session_id":"session-1","intent_generation":5,"continuation_identity":"continuation-1","policy":"continuation"},"store":{"store_uuid":"store-1","restore_epoch":7},"order":{"sequence":11,"revision":13},"trusted_ingress":{"issuer":"local-ingress","reference_id":"ingress-1","principal_id":"principal-1","tenant_scope":"tenant-1","city_scope":"city-1","credential_class":"controller-ingress","policy_version":"policy-v1","policy_decision_id":"decision-1","action":"nudge","target_session_id":"session-1","payload_digest":"71bb3cafdac278f6bb66d7f37bb70535b5359d3201785757a37f86af7856443a","issued_at":"2026-07-15T09:59:00Z","expires_at":"2026-07-15T10:10:00Z"},"source":"wait","message":"wake up","reference":{"kind":"wait","id":"wait-1"},"created_at":"2026-07-15T10:00:00Z","deliver_after":"2026-07-15T10:00:01Z","expires_at":"2026-07-15T11:00:00Z"}`
	if string(wire) != want {
		t.Fatalf("wire = %s\nwant = %s", wire, want)
	}
}

func TestCommandV1RoundTripsBothExactTargetPolicies(t *testing.T) {
	queued := validCommandV1(CommandStatePending)
	waitIdle := validCommandV1(CommandStatePending)
	waitIdle.Mode = DeliveryModeWaitIdle
	waitIdle.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(waitIdle)
	immediate := validCommandV1(CommandStatePending)
	immediate.Mode = DeliveryModeImmediate
	immediate.DeliverAfter = immediate.CreatedAt
	immediate.Target.Policy = TargetPolicyExactLaunch
	immediate.Target.ContinuationIdentity = ""
	immediate.Target.LaunchIdentity = "launch-1"
	immediate.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: immediate.CreatedAt}
	immediate.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(immediate)

	for _, command := range []Command{queued, waitIdle, immediate} {
		wire, err := EncodeCommandV1(command)
		if err != nil {
			t.Fatalf("EncodeCommandV1(%s/%s): %v", command.Mode, command.Target.Policy, err)
		}
		decoded := DecodeCommand(wire)
		if decoded.Disposition != CommandDecodeDecoded || !reflect.DeepEqual(decoded.Command, command) {
			t.Fatalf("DecodeCommand(%s/%s) = %#v", command.Mode, command.Target.Policy, decoded)
		}
	}
}

func TestCommandV1UpgradeRequiredPreservesImmediateCommitBinding(t *testing.T) {
	command := validImmediateCommandV1()
	command.State = CommandStateUpgradeRequired
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	decoded := DecodeCommand(wire)
	if decoded.Disposition != CommandDecodeDecoded || decoded.Command.Binding == nil || *decoded.Command.Binding != *command.Binding {
		t.Fatalf("upgrade_required immediate binding did not round trip: %#v", decoded)
	}
}

func TestCommandV1RoundTripsRetryPendingEnvelope(t *testing.T) {
	command := validCommandV1(CommandStateInFlight)
	command.State = CommandStatePending
	command.Claim = nil
	next := command.CreatedAt.Add(2 * time.Minute)
	command.Retry.NextEligibleAt = &next
	command.Retry.ErrorClass = CommandErrorClassProviderBusy
	command.Retry.ErrorDetail = "provider admission was unavailable"
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	decoded := DecodeCommand(wire)
	if decoded.Disposition != CommandDecodeDecoded || !reflect.DeepEqual(decoded.Command, command) {
		t.Fatalf("retry-pending round trip = %#v, want %#v", decoded, command)
	}
}

func TestCommandV1RoundTripsWithoutOptionalReference(t *testing.T) {
	command := validCommandV1(CommandStatePending)
	command.Reference = nil
	command.Source = CommandSourceSession
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	decoded := DecodeCommand(wire)
	if decoded.Disposition != CommandDecodeDecoded || !reflect.DeepEqual(decoded.Command, command) {
		t.Fatalf("reference-free round trip = %#v, want %#v", decoded, command)
	}
}

func TestCommandPayloadDigestExcludesOnlyControllerEnvelopeFields(t *testing.T) {
	command := validCommandV1(CommandStatePending)
	want := ComputeCommandPayloadDigest(command)
	command.Store.RestoreEpoch++
	command.Order.Revision++
	next := command.CreatedAt.Add(2 * time.Minute)
	command.Retry = &CommandRetry{AttemptCount: 1, LastAttemptAt: command.CreatedAt.Add(time.Minute), NextEligibleAt: &next, ErrorClass: "provider_busy", ErrorDetail: "busy"}
	if got := ComputeCommandPayloadDigest(command); got != want {
		t.Fatalf("controller envelope changed requester payload digest: got %q want %q", got, want)
	}
	command.Message = "changed requester payload"
	if got := ComputeCommandPayloadDigest(command); got == want {
		t.Fatal("requester message mutation did not change payload digest")
	}
}

func TestCommandPayloadDigestV1CoversEveryRequesterFieldAndDomain(t *testing.T) {
	command := validCommandV1(CommandStatePending)
	want := ComputeCommandPayloadDigest(command)
	if CommandPayloadDigestDomainV1 != "gascity.nudge-command.payload" {
		t.Fatalf("digest domain = %q, want pinned protocol domain", CommandPayloadDigestDomainV1)
	}
	if CommandPayloadDigestSchemaV1 != "v1" {
		t.Fatalf("digest schema = %q, want v1", CommandPayloadDigestSchemaV1)
	}
	if want != "71bb3cafdac278f6bb66d7f37bb70535b5359d3201785757a37f86af7856443a" {
		t.Fatalf("digest = %q, update only for an intentional v1 requester-fingerprint change", want)
	}

	tests := []struct {
		name   string
		mutate func(*Command)
	}{
		{name: "version", mutate: func(c *Command) { c.Version++ }},
		{name: "id", mutate: func(c *Command) { c.ID += "-changed" }},
		{name: "mode", mutate: func(c *Command) { c.Mode = DeliveryModeWaitIdle }},
		{name: "target session", mutate: func(c *Command) { c.Target.SessionID += "-changed" }},
		{name: "target generation", mutate: func(c *Command) { c.Target.IntentGeneration++ }},
		{name: "target continuation", mutate: func(c *Command) { c.Target.ContinuationIdentity += "-changed" }},
		{name: "target launch", mutate: func(c *Command) { c.Target.LaunchIdentity = "launch-changed" }},
		{name: "target policy", mutate: func(c *Command) { c.Target.Policy = TargetPolicyExactLaunch }},
		{name: "source", mutate: func(c *Command) { c.Source = CommandSourceMail }},
		{name: "message", mutate: func(c *Command) { c.Message += " changed" }},
		{name: "reference kind", mutate: func(c *Command) { c.Reference.Kind = CommandReferenceBead }},
		{name: "reference id", mutate: func(c *Command) { c.Reference.ID += "-changed" }},
		{name: "reference presence", mutate: func(c *Command) { c.Reference = nil }},
		{name: "created at", mutate: func(c *Command) { c.CreatedAt = c.CreatedAt.Add(time.Nanosecond) }},
		{name: "deliver after", mutate: func(c *Command) { c.DeliverAfter = c.DeliverAfter.Add(time.Nanosecond) }},
		{name: "expires at", mutate: func(c *Command) { c.ExpiresAt = c.ExpiresAt.Add(time.Nanosecond) }},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changed := command
			if command.Reference != nil {
				reference := *command.Reference
				changed.Reference = &reference
			}
			tc.mutate(&changed)
			if got := ComputeCommandPayloadDigest(changed); got == want {
				t.Fatalf("mutating requester field %q did not change digest", tc.name)
			}
		})
	}
}

func TestCommandPayloadDigestV1ExcludesEveryControllerOwnedField(t *testing.T) {
	command := validCommandV1(CommandStatePending)
	want := ComputeCommandPayloadDigest(command)
	boundAt := command.CreatedAt.Add(time.Second)
	next := command.CreatedAt.Add(2 * time.Minute)
	mutations := []func(*Command){
		func(c *Command) { c.State = CommandStateInFlight },
		func(c *Command) { c.Store.StoreUUID += "-changed" },
		func(c *Command) { c.Store.RestoreEpoch++ },
		func(c *Command) { c.Order.Sequence++ },
		func(c *Command) { c.Order.Revision++ },
		func(c *Command) { c.TrustedIngress.PolicyDecisionID += "-changed" },
		func(c *Command) { c.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: boundAt} },
		func(c *Command) {
			c.Retry = &CommandRetry{AttemptCount: 1, LastAttemptAt: command.CreatedAt.Add(time.Minute), NextEligibleAt: &next, ErrorClass: CommandErrorClassProviderBusy, ErrorDetail: "busy"}
		},
		func(c *Command) { c.Claim = &CommandClaim{ID: "claim-1"} },
		func(c *Command) { c.Terminal = &CommandTerminal{ActionResult: CommandActionResultExpired} },
	}
	for i, mutate := range mutations {
		changed := command
		mutate(&changed)
		if got := ComputeCommandPayloadDigest(changed); got != want {
			t.Fatalf("controller mutation %d changed requester digest: got %q want %q", i, got, want)
		}
	}
}

func TestDecodeCommandStrictlyRejectsInvalidKnownV1(t *testing.T) {
	claim := validCommandV1(CommandStateInFlight).Claim
	retry := validCommandV1(CommandStateInFlight).Retry
	terminal := validCommandV1(CommandStateDelivered).Terminal
	nonUTC := time.Date(2026, 7, 15, 10, 0, 0, 0, time.FixedZone("offset", 3600))
	tests := []struct {
		name   string
		mutate func(*Command)
	}{
		{name: "missing command id", mutate: func(c *Command) { c.ID = "" }},
		{name: "noncanonical command id", mutate: func(c *Command) { c.ID = " command-1" }},
		{name: "unknown state", mutate: func(c *Command) { c.State = "mystery" }},
		{name: "unknown mode", mutate: func(c *Command) { c.Mode = "later" }},
		{name: "missing session id", mutate: func(c *Command) { c.Target.SessionID = "" }},
		{name: "zero target intent generation", mutate: func(c *Command) { c.Target.IntentGeneration = 0 }},
		{name: "unknown target policy", mutate: func(c *Command) { c.Target.Policy = "any" }},
		{name: "queue exact launch policy", mutate: func(c *Command) {
			c.Target.Policy = TargetPolicyExactLaunch
			c.Target.ContinuationIdentity = ""
			c.Target.LaunchIdentity = "launch-1"
		}},
		{name: "immediate continuation policy", mutate: func(c *Command) { c.Mode = DeliveryModeImmediate }},
		{name: "continuation missing identity", mutate: func(c *Command) { c.Target.ContinuationIdentity = "" }},
		{name: "continuation carries launch", mutate: func(c *Command) { c.Target.LaunchIdentity = "launch-1" }},
		{name: "missing store uuid", mutate: func(c *Command) { c.Store.StoreUUID = "" }},
		{name: "zero restore epoch", mutate: func(c *Command) { c.Store.RestoreEpoch = 0 }},
		{name: "zero order sequence", mutate: func(c *Command) { c.Order.Sequence = 0 }},
		{name: "zero order revision", mutate: func(c *Command) { c.Order.Revision = 0 }},
		{name: "missing ingress issuer", mutate: func(c *Command) { c.TrustedIngress.Issuer = "" }},
		{name: "missing ingress reference", mutate: func(c *Command) { c.TrustedIngress.ReferenceID = "" }},
		{name: "missing principal", mutate: func(c *Command) { c.TrustedIngress.PrincipalID = "" }},
		{name: "missing tenant scope", mutate: func(c *Command) { c.TrustedIngress.TenantScope = "" }},
		{name: "missing city scope", mutate: func(c *Command) { c.TrustedIngress.CityScope = "" }},
		{name: "missing credential class", mutate: func(c *Command) { c.TrustedIngress.CredentialClass = "" }},
		{name: "missing policy version", mutate: func(c *Command) { c.TrustedIngress.PolicyVersion = "" }},
		{name: "missing policy decision", mutate: func(c *Command) { c.TrustedIngress.PolicyDecisionID = "" }},
		{name: "wrong action", mutate: func(c *Command) { c.TrustedIngress.Action = "stop" }},
		{name: "wrong target coverage", mutate: func(c *Command) { c.TrustedIngress.TargetSessionID = "session-2" }},
		{name: "wrong payload digest", mutate: func(c *Command) { c.TrustedIngress.PayloadDigest = strings.Repeat("0", 64) }},
		{name: "zero ingress issued at", mutate: func(c *Command) { c.TrustedIngress.IssuedAt = time.Time{} }},
		{name: "ingress expiry before issue", mutate: func(c *Command) { c.TrustedIngress.ExpiresAt = c.TrustedIngress.IssuedAt }},
		{name: "command predates ingress", mutate: func(c *Command) { c.TrustedIngress.IssuedAt = c.CreatedAt.Add(time.Second) }},
		{name: "command after ingress expiry", mutate: func(c *Command) { c.TrustedIngress.ExpiresAt = c.CreatedAt }},
		{name: "missing source", mutate: func(c *Command) { c.Source = "" }},
		{name: "missing message", mutate: func(c *Command) { c.Message = "" }},
		{name: "reference missing kind", mutate: func(c *Command) { c.Reference.Kind = "" }},
		{name: "reference missing id", mutate: func(c *Command) { c.Reference.ID = "" }},
		{name: "copied ingress stamp cannot change schedule", mutate: func(c *Command) { c.DeliverAfter = c.DeliverAfter.Add(time.Second) }},
		{name: "copied ingress stamp cannot change expiry", mutate: func(c *Command) { c.ExpiresAt = c.ExpiresAt.Add(time.Hour) }},
		{name: "zero created at", mutate: func(c *Command) { c.CreatedAt = time.Time{} }},
		{name: "non utc created at", mutate: func(c *Command) { c.CreatedAt = nonUTC }},
		{name: "deliver before create", mutate: func(c *Command) { c.DeliverAfter = c.CreatedAt.Add(-time.Second) }},
		{name: "expiry before delivery", mutate: func(c *Command) { c.ExpiresAt = c.DeliverAfter }},
		{name: "retry zero attempt count", mutate: func(c *Command) { copied := *retry; copied.AttemptCount = 0; c.Retry = &copied }},
		{name: "retry zero last attempt", mutate: func(c *Command) { copied := *retry; copied.LastAttemptAt = time.Time{}; c.Retry = &copied }},
		{name: "retry error class without detail", mutate: func(c *Command) {
			copied := *retry
			copied.ErrorClass = "provider_busy"
			copied.ErrorDetail = ""
			c.Retry = &copied
		}},
		{name: "retry detail without class", mutate: func(c *Command) {
			copied := *retry
			copied.ErrorClass = ""
			copied.ErrorDetail = "provider unavailable"
			c.Retry = &copied
		}},
		{name: "retry oversized detail", mutate: func(c *Command) {
			copied := *retry
			copied.ErrorClass = "provider_busy"
			copied.ErrorDetail = strings.Repeat("x", MaxCommandRetryErrorDetailBytes+1)
			c.Retry = &copied
		}},
		{name: "retry pending missing next eligibility", mutate: func(c *Command) { copied := *retry; c.Retry = &copied }},
		{name: "retry next not after last attempt", mutate: func(c *Command) {
			copied := *retry
			next := copied.LastAttemptAt
			copied.NextEligibleAt = &next
			c.Retry = &copied
		}},
		{name: "retry next at expiry", mutate: func(c *Command) {
			copied := *retry
			next := c.ExpiresAt
			copied.NextEligibleAt = &next
			c.Retry = &copied
		}},
		{name: "in flight carries next eligibility", mutate: func(c *Command) {
			inflight := validCommandV1(CommandStateInFlight)
			*c = inflight
			next := c.CreatedAt.Add(2 * time.Minute)
			c.Retry.NextEligibleAt = &next
		}},
		{name: "terminal carries next eligibility", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			next := c.CreatedAt.Add(3 * time.Minute)
			c.Retry.NextEligibleAt = &next
		}},
		{name: "upgrade required carries retry", mutate: func(c *Command) { copied := *retry; c.State = CommandStateUpgradeRequired; c.Retry = &copied }},
		{name: "pending carries claim", mutate: func(c *Command) { c.Claim = claim }},
		{name: "pending carries terminal", mutate: func(c *Command) { c.Terminal = terminal }},
		{name: "in flight missing claim", mutate: func(c *Command) { c.State = CommandStateInFlight }},
		{name: "in flight missing attempt evidence", mutate: func(c *Command) { inflight := validCommandV1(CommandStateInFlight); *c = inflight; c.Retry = nil }},
		{name: "in flight attempt time differs from claim", mutate: func(c *Command) {
			inflight := validCommandV1(CommandStateInFlight)
			*c = inflight
			c.Retry.LastAttemptAt = c.Retry.LastAttemptAt.Add(-time.Second)
		}},
		{name: "in flight carries terminal", mutate: func(c *Command) { c.State = CommandStateInFlight; c.Claim = claim; c.Terminal = terminal }},
		{name: "claim missing id", mutate: func(c *Command) { c.State = CommandStateInFlight; copied := *claim; copied.ID = ""; c.Claim = &copied }},
		{name: "claim missing owner", mutate: func(c *Command) {
			c.State = CommandStateInFlight
			copied := *claim
			copied.OwnerID = ""
			c.Claim = &copied
		}},
		{name: "claim operation mismatch", mutate: func(c *Command) {
			c.State = CommandStateInFlight
			copied := *claim
			copied.OperationID = "other-command"
			c.Claim = &copied
		}},
		{name: "claim missing attempt", mutate: func(c *Command) {
			c.State = CommandStateInFlight
			copied := *claim
			copied.AttemptID = ""
			c.Claim = &copied
		}},
		{name: "claim missing bound launch", mutate: func(c *Command) {
			c.State = CommandStateInFlight
			copied := *claim
			copied.BoundLaunchIdentity = ""
			c.Claim = &copied
		}},
		{name: "claim exact launch mismatch", mutate: func(c *Command) {
			c.Mode = DeliveryModeImmediate
			c.Target.Policy = TargetPolicyExactLaunch
			c.Target.ContinuationIdentity = ""
			c.Target.LaunchIdentity = "launch-exact"
			c.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(*c)
			c.State = CommandStateInFlight
			copied := *claim
			c.Claim = &copied
		}},
		{name: "claim missing authorization decision", mutate: func(c *Command) {
			c.State = CommandStateInFlight
			copied := *claim
			copied.AuthorizationDecisionID = ""
			c.Claim = &copied
		}},
		{name: "claim missing authorization policy", mutate: func(c *Command) {
			c.State = CommandStateInFlight
			copied := *claim
			copied.AuthorizationPolicyVersion = ""
			c.Claim = &copied
		}},
		{name: "claim lease not after claim", mutate: func(c *Command) {
			c.State = CommandStateInFlight
			copied := *claim
			copied.LeaseUntil = copied.ClaimedAt
			c.Claim = &copied
		}},
		{name: "terminal state missing terminal", mutate: func(c *Command) { c.State = CommandStateDelivered }},
		{name: "delivered missing attempt evidence", mutate: func(c *Command) { delivered := validCommandV1(CommandStateDelivered); *c = delivered; c.Retry = nil }},
		{name: "terminal missing action result", mutate: func(c *Command) {
			c.State = CommandStateDelivered
			copied := *terminal
			copied.ActionResult = ""
			c.Terminal = &copied
		}},
		{name: "terminal before create", mutate: func(c *Command) {
			c.State = CommandStateDelivered
			copied := *terminal
			copied.At = c.CreatedAt.Add(-time.Second)
			c.Terminal = &copied
		}},
		{name: "terminal unknown provider stage", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			c.Terminal.ProviderStage = "magic"
		}},
		{name: "terminal unknown completion", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			c.Terminal.Completion = "magic"
		}},
		{name: "delivered operation mismatch", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			c.Terminal.OperationID = "other-command"
		}},
		{name: "delivered missing attempt correlation", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			c.Terminal.AttemptID = ""
		}},
		{name: "delivered exact launch mismatch", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			c.Mode = DeliveryModeImmediate
			c.Target.Policy = TargetPolicyExactLaunch
			c.Target.ContinuationIdentity = ""
			c.Target.LaunchIdentity = "launch-exact"
			c.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(*c)
			c.Terminal.BoundLaunchIdentity = "other-launch"
		}},
		{name: "delivered wrong provider evidence", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			c.Terminal.ProviderStage = ProviderStageMayHaveEntered
		}},
		{name: "unknown delivery wrong completion", mutate: func(c *Command) {
			unknown := validCommandV1(CommandStateDeliveryUnknown)
			*c = unknown
			c.Terminal.Completion = CompletionStateCompleted
		}},
		{name: "terminal state carries active claim", mutate: func(c *Command) {
			delivered := validCommandV1(CommandStateDelivered)
			*c = delivered
			c.Claim = claim
		}},
		{name: "unclaimed terminal carries correlation", mutate: func(c *Command) {
			expired := validCommandV1(CommandStateExpired)
			*c = expired
			c.Terminal.AttemptID = "attempt-1"
		}},
		{name: "upgrade required carries claim", mutate: func(c *Command) { c.State = CommandStateUpgradeRequired; c.Claim = claim }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command := validCommandV1(CommandStatePending)
			tc.mutate(&command)
			if wire, err := EncodeCommandV1(command); err == nil {
				t.Fatalf("EncodeCommandV1(invalid) = %s, nil error", wire)
			}
			wire, err := json.Marshal(command)
			if err != nil {
				t.Fatalf("json.Marshal fixture: %v", err)
			}
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterInvalidKnownVersion {
				t.Fatalf("DecodeCommand disposition/reason = %q/%q, want %q/%q; detail=%q", got.Disposition, got.DeadLetterReason, CommandDecodeDeadLetter, CommandDeadLetterInvalidKnownVersion, got.Detail)
			}
			if got.Command != (Command{}) {
				t.Fatalf("invalid known command escaped decoder: %#v", got.Command)
			}
		})
	}
}

func TestDecodeCommandRejectsUnknownAndDuplicateKnownV1Fields(t *testing.T) {
	validWire, err := EncodeCommandV1(validCommandV1(CommandStatePending))
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	unknown := append([]byte(nil), validWire[:len(validWire)-1]...)
	unknown = append(unknown, []byte(`,"surprise":true}`)...)
	duplicate := bytes.Replace(validWire, []byte(`"id":"command-1"`), []byte(`"id":"command-1","id":"command-2"`), 1)
	nestedDuplicate := bytes.Replace(validWire, []byte(`"session_id":"session-1"`), []byte(`"session_id":"session-1","session_id":"session-2"`), 1)

	for name, tc := range map[string]struct {
		wire   []byte
		reason CommandDeadLetterReason
	}{
		"unknown":          {wire: unknown, reason: CommandDeadLetterInvalidKnownVersion},
		"duplicate":        {wire: duplicate, reason: CommandDeadLetterMalformed},
		"nested duplicate": {wire: nestedDuplicate, reason: CommandDeadLetterMalformed},
	} {
		t.Run(name, func(t *testing.T) {
			got := DecodeCommand(tc.wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != tc.reason {
				t.Fatalf("DecodeCommand = %#v, want dead letter %q", got, tc.reason)
			}
		})
	}
}

func TestDecodeCommandClassifiesMalformedAndOversizedInput(t *testing.T) {
	tests := []struct {
		name   string
		wire   []byte
		reason CommandDeadLetterReason
	}{
		{name: "empty", wire: nil, reason: CommandDeadLetterMalformed},
		{name: "invalid json", wire: []byte(`{"version":`), reason: CommandDeadLetterMalformed},
		{name: "trailing json", wire: []byte(`{"version":1}{}`), reason: CommandDeadLetterMalformed},
		{name: "top level array", wire: []byte(`[1]`), reason: CommandDeadLetterMalformed},
		{name: "missing version", wire: []byte(`{"id":"x"}`), reason: CommandDeadLetterMalformed},
		{name: "null version", wire: []byte(`{"version":null}`), reason: CommandDeadLetterMalformed},
		{name: "string version", wire: []byte(`{"version":"2"}`), reason: CommandDeadLetterMalformed},
		{name: "fractional version", wire: []byte(`{"version":1.5}`), reason: CommandDeadLetterMalformed},
		{name: "invalid utf8", wire: []byte{'{', '"', 'v', 'e', 'r', 's', 'i', 'o', 'n', '"', ':', '2', ',', '"', 'x', '"', ':', 0xff, '}'}, reason: CommandDeadLetterMalformed},
		{name: "oversized", wire: bytes.Repeat([]byte("x"), MaxCommandBytes+1), reason: CommandDeadLetterPayloadTooLarge},
		{name: "old unsupported version", wire: []byte(`{"version":0}`), reason: CommandDeadLetterUnsupportedVersion},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DecodeCommand(tc.wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != tc.reason {
				t.Fatalf("DecodeCommand = %#v, want dead letter reason %q", got, tc.reason)
			}
			if len(got.Raw) != 0 {
				t.Fatalf("dead letter retained %d raw bytes; only supported-size newer versions may retain raw", len(got.Raw))
			}
		})
	}
}

func TestDecodeCommandPreservesWellFormedNewerVersionByteForByte(t *testing.T) {
	wire := append([]byte("  \n"), validFutureCommandWire(`,"future":{"opaque":[1,true,null]}`)...)
	wire = append(wire, '\t')
	got := DecodeCommand(wire)
	if got.Disposition != CommandDecodeUpgradeRequired {
		t.Fatalf("disposition = %q, want %q; reason=%q detail=%q", got.Disposition, CommandDecodeUpgradeRequired, got.DeadLetterReason, got.Detail)
	}
	if got.Version != 2 {
		t.Fatalf("version = %d, want 2", got.Version)
	}
	if !bytes.Equal(got.Raw, wire) {
		t.Fatalf("raw newer bytes changed:\n got=%q\nwant=%q", got.Raw, wire)
	}
	if got.Command != (Command{}) {
		t.Fatalf("newer version decoded into known command: %#v", got.Command)
	}

	// The result owns its preservation buffer; callers may reuse the input.
	wire[0] = 'x'
	if got.Raw[0] != ' ' {
		t.Fatal("newer raw preservation aliases caller input")
	}
}

func TestDecodeCommandRejectsMalformedNewerEnvelopeInsteadOfParkingIt(t *testing.T) {
	valid := validFutureCommandWire("")
	duplicateVersion := append([]byte(nil), valid[:len(valid)-1]...)
	duplicateVersion = append(duplicateVersion, []byte(`,"version":3}`)...)
	for name, wire := range map[string][]byte{
		"duplicate version": duplicateVersion,
		"nested duplicate":  validFutureCommandWire(`,"future":{"x":1,"x":2}`),
		"trailing value":    append(valid, []byte(` true`)...),
	} {
		t.Run(name, func(t *testing.T) {
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterMalformed {
				t.Fatalf("DecodeCommand = %#v, want malformed dead letter", got)
			}
		})
	}
}

func TestDecodeCommandRequiresVersionInvariantRoutingForNewerCommands(t *testing.T) {
	wire := validFutureCommandWire(`,"future":{"opaque":[1,true,null]}`)
	got := DecodeCommand(wire)
	if got.Disposition != CommandDecodeUpgradeRequired {
		t.Fatalf("DecodeCommand = %#v, want upgrade-required", got)
	}
	want := CommandRoutingHeader{
		CommandID:        "command-future",
		Store:            CommandStoreBinding{StoreUUID: "store-1", RestoreEpoch: 7},
		TargetSessionID:  "session-1",
		IntentGeneration: 5,
		Sequence:         11,
		Revision:         13,
	}
	if got.Routing != want {
		t.Fatalf("routing = %#v, want %#v", got.Routing, want)
	}
	if !bytes.Equal(got.Raw, wire) {
		t.Fatalf("opaque bytes changed: got %q want %q", got.Raw, wire)
	}
}

func TestDecodeCommandRejectsUnroutableNewerCommands(t *testing.T) {
	valid := validFutureCommandWire("")
	tests := map[string][]byte{
		"version only":              []byte(`{"version":2}`),
		"missing command id":        bytes.Replace(valid, []byte(`,"id":"command-future"`), nil, 1),
		"missing store":             bytes.Replace(valid, []byte(`,"store":{"store_uuid":"store-1","restore_epoch":7}`), nil, 1),
		"missing target":            bytes.Replace(valid, []byte(`,"target":{"session_id":"session-1","intent_generation":5}`), nil, 1),
		"missing order":             bytes.Replace(valid, []byte(`,"order":{"sequence":11,"revision":13}`), nil, 1),
		"empty command id":          bytes.Replace(valid, []byte(`"command-future"`), []byte(`""`), 1),
		"zero restore epoch":        bytes.Replace(valid, []byte(`"restore_epoch":7`), []byte(`"restore_epoch":0`), 1),
		"zero intent generation":    bytes.Replace(valid, []byte(`"intent_generation":5`), []byte(`"intent_generation":0`), 1),
		"zero sequence":             bytes.Replace(valid, []byte(`"sequence":11`), []byte(`"sequence":0`), 1),
		"zero revision":             bytes.Replace(valid, []byte(`"revision":13`), []byte(`"revision":0`), 1),
		"wrong target field casing": bytes.Replace(valid, []byte(`"session_id"`), []byte(`"Session_ID"`), 1),
	}
	for name, wire := range tests {
		t.Run(name, func(t *testing.T) {
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterMalformed {
				t.Fatalf("DecodeCommand = %#v, want malformed dead letter", got)
			}
			if len(got.Raw) != 0 || got.Routing != (CommandRoutingHeader{}) {
				t.Fatalf("unroutable command retained authority-free routing/raw: %#v", got)
			}
		})
	}
}

func TestDecodeCommandRejectsCaseFoldedJSONKeyCollisionsRecursively(t *testing.T) {
	known, err := EncodeCommandV1(validCommandV1(CommandStatePending))
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	knownTop := append([]byte(nil), known[:len(known)-1]...)
	knownTop = append(knownTop, []byte(`,"ID":"other"}`)...)
	knownNested := bytes.Replace(known, []byte(`"session_id":"session-1"`), []byte(`"session_id":"session-1","Session_ID":"other"`), 1)
	futureNested := validFutureCommandWire(`,"future":{"opaque":1,"OPAQUE":2}`)
	futureUnicode := validFutureCommandWire(`,"future":{"K":1,"\u212a":2}`)
	futureSigma := validFutureCommandWire(`,"future":{"\u03c3":1,"\u03c2":2}`)

	for name, wire := range map[string][]byte{
		"known top level":    knownTop,
		"known nested":       knownNested,
		"future nested":      futureNested,
		"future kelvin fold": futureUnicode,
		"future sigma fold":  futureSigma,
	} {
		t.Run(name, func(t *testing.T) {
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterMalformed {
				t.Fatalf("DecodeCommand = %#v, want malformed collision dead letter", got)
			}
		})
	}
}

func TestDecodeCommandRejectsLoneWrongCaseAliasesAtEveryKnownV1Object(t *testing.T) {
	pending, err := EncodeCommandV1(validCommandV1(CommandStatePending))
	if err != nil {
		t.Fatalf("EncodeCommandV1(pending): %v", err)
	}
	inFlight, err := EncodeCommandV1(validCommandV1(CommandStateInFlight))
	if err != nil {
		t.Fatalf("EncodeCommandV1(in_flight): %v", err)
	}
	delivered, err := EncodeCommandV1(validCommandV1(CommandStateDelivered))
	if err != nil {
		t.Fatalf("EncodeCommandV1(delivered): %v", err)
	}
	tests := map[string][]byte{
		"command":         bytes.Replace(pending, []byte(`"message"`), []byte(`"MESSAGE"`), 1),
		"target":          bytes.Replace(pending, []byte(`"session_id"`), []byte(`"Session_ID"`), 1),
		"store":           bytes.Replace(pending, []byte(`"restore_epoch"`), []byte(`"RESTORE_EPOCH"`), 1),
		"order":           bytes.Replace(pending, []byte(`"revision"`), []byte(`"Revision"`), 1),
		"trusted ingress": bytes.Replace(pending, []byte(`"principal_id"`), []byte(`"Principal_ID"`), 1),
		"reference":       bytes.Replace(pending, []byte(`"kind"`), []byte(`"KIND"`), 1),
		"binding":         bytes.Replace(inFlight, []byte(`"bound_at"`), []byte(`"BOUND_AT"`), 1),
		"retry":           bytes.Replace(inFlight, []byte(`"attempt_count"`), []byte(`"Attempt_Count"`), 1),
		"claim":           bytes.Replace(inFlight, []byte(`"owner_id"`), []byte(`"Owner_ID"`), 1),
		"terminal":        bytes.Replace(delivered, []byte(`"action_result"`), []byte(`"Action_Result"`), 1),
	}
	for name, wire := range tests {
		t.Run(name, func(t *testing.T) {
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterInvalidKnownVersion {
				t.Fatalf("DecodeCommand = %#v, want invalid-known dead letter", got)
			}
		})
	}
}

func TestDecodeCommandRejectsExplicitNullForOmittedKnownV1Pointers(t *testing.T) {
	wire, err := EncodeCommandV1(validCommandV1(CommandStatePending))
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	wire = append([]byte(nil), wire[:len(wire)-1]...)
	wire = append(wire, []byte(`,"binding":null}`)...)
	got := DecodeCommand(wire)
	if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterInvalidKnownVersion {
		t.Fatalf("DecodeCommand = %#v, want noncanonical-null invalid-known dead letter", got)
	}
}

func TestDecodeCommandRejectsJSONResourceBudgetExhaustion(t *testing.T) {
	tooDeep := "0"
	for range MaxCommandJSONDepth {
		tooDeep = "[" + tooDeep + "]"
	}
	tooMany := strings.Repeat("0,", MaxCommandJSONMembers) + "0"
	tests := map[string][]byte{
		"depth":  validFutureCommandWire(`,"future":` + tooDeep),
		"member": validFutureCommandWire(`,"future":[` + tooMany + `]`),
	}
	for name, wire := range tests {
		t.Run(name, func(t *testing.T) {
			if len(wire) > MaxCommandBytes {
				t.Fatalf("fixture size %d exceeds byte budget and does not isolate %s budget", len(wire), name)
			}
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterMalformed {
				t.Fatalf("DecodeCommand = %#v, want malformed budget dead letter", got)
			}
		})
	}
}

func TestDecodeCommandRejectsLossyUnicodeSurrogatesInRoutingIdentity(t *testing.T) {
	valid := validFutureCommandWire("")
	for name, replacement := range map[string][]byte{
		"unpaired high surrogate": []byte(`"\ud800"`),
		"unpaired low surrogate":  []byte(`"\udfff"`),
	} {
		t.Run(name, func(t *testing.T) {
			wire := bytes.Replace(valid, []byte(`"session-1"`), replacement, 1)
			got := DecodeCommand(wire)
			if got.Disposition != CommandDecodeDeadLetter || got.DeadLetterReason != CommandDeadLetterMalformed {
				t.Fatalf("DecodeCommand = %#v, want malformed dead letter", got)
			}
		})
	}

	wire := bytes.Replace(valid, []byte(`"command-future"`), []byte(`"\ud83d\ude80"`), 1)
	got := DecodeCommand(wire)
	if got.Disposition != CommandDecodeUpgradeRequired || got.Routing.CommandID != "🚀" || !bytes.Equal(got.Raw, wire) {
		t.Fatalf("valid surrogate pair was not preserved and routed exactly: %#v", got)
	}
}

func TestCommandV1AcceptsOnlyClosedSourceReferenceCombinations(t *testing.T) {
	valid := []struct {
		source    CommandSource
		reference *Reference
	}{
		{source: CommandSourceSession},
		{source: CommandSourceQueue},
		{source: CommandSourceController},
		{source: CommandSourceMail, reference: &Reference{Kind: CommandReferenceBead, ID: "mail-1"}},
		{source: CommandSourceSling, reference: &Reference{Kind: CommandReferenceBead, ID: "work-1"}},
		{source: CommandSourceWait, reference: &Reference{Kind: CommandReferenceWait, ID: "wait-1"}},
	}
	for _, tc := range valid {
		t.Run(string(tc.source), func(t *testing.T) {
			command := validCommandV1(CommandStatePending)
			command.Source = tc.source
			command.Reference = tc.reference
			command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
			if _, err := EncodeCommandV1(command); err != nil {
				t.Fatalf("EncodeCommandV1(%s/%#v): %v", tc.source, tc.reference, err)
			}
		})
	}

	invalid := []struct {
		name      string
		source    CommandSource
		reference *Reference
	}{
		{name: "unknown source", source: "future"},
		{name: "session reference", source: CommandSourceSession, reference: &Reference{Kind: CommandReferenceWait, ID: "wait-1"}},
		{name: "wait missing reference", source: CommandSourceWait},
		{name: "wait bead reference", source: CommandSourceWait, reference: &Reference{Kind: CommandReferenceBead, ID: "wait-1"}},
		{name: "mail wait reference", source: CommandSourceMail, reference: &Reference{Kind: CommandReferenceWait, ID: "mail-1"}},
		{name: "sling missing reference", source: CommandSourceSling},
		{name: "unknown reference", source: CommandSourceWait, reference: &Reference{Kind: "future", ID: "wait-1"}},
	}
	for _, tc := range invalid {
		t.Run(tc.name, func(t *testing.T) {
			command := validCommandV1(CommandStatePending)
			command.Source = tc.source
			command.Reference = tc.reference
			command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
			if wire, err := EncodeCommandV1(command); err == nil {
				t.Fatalf("EncodeCommandV1(invalid) = %s, nil error", wire)
			}
		})
	}
}

func TestCommandV1PendingRetryRetainsImmutableBindingAndAttemptEvidence(t *testing.T) {
	command := validCommandV1(CommandStateInFlight)
	claim := *command.Claim
	command.State = CommandStatePending
	command.Claim = nil
	next := claim.ClaimedAt.Add(time.Minute)
	command.Retry.NextEligibleAt = &next
	command.Retry.ErrorClass = CommandErrorClassProviderBusy
	command.Retry.ErrorDetail = "provider admission unavailable"

	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	got := DecodeCommand(wire)
	if got.Disposition != CommandDecodeDecoded {
		t.Fatalf("DecodeCommand = %#v, want decoded", got)
	}
	if got.Command.Binding == nil || *got.Command.Binding != *command.Binding {
		t.Fatalf("binding after claim release = %#v, want %#v", got.Command.Binding, command.Binding)
	}
	if got.Command.Retry == nil || got.Command.Retry.ClaimID != claim.ID || got.Command.Retry.AttemptID != claim.AttemptID ||
		got.Command.Retry.BoundLaunchIdentity != claim.BoundLaunchIdentity ||
		got.Command.Retry.AuthorizationDecisionID != claim.AuthorizationDecisionID ||
		got.Command.Retry.AuthorizationPolicyVersion != claim.AuthorizationPolicyVersion {
		t.Fatalf("attempt evidence after claim release = %#v, want exact prior claim %#v", got.Command.Retry, claim)
	}
}

func TestCommandV1TerminalRetainsExactClaimAuthorizationAndBindingEvidence(t *testing.T) {
	command := validCommandV1(CommandStateDelivered)
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	got := DecodeCommand(wire)
	if got.Disposition != CommandDecodeDecoded {
		t.Fatalf("DecodeCommand = %#v, want decoded", got)
	}
	terminal := got.Command.Terminal
	retry := got.Command.Retry
	if terminal == nil || retry == nil || got.Command.Binding == nil {
		t.Fatalf("terminal command lost evidence: %#v", got.Command)
	}
	if terminal.ClaimID != retry.ClaimID || terminal.OperationID != retry.OperationID || terminal.AttemptID != retry.AttemptID ||
		terminal.BoundLaunchIdentity != retry.BoundLaunchIdentity || terminal.BoundLaunchIdentity != got.Command.Binding.LaunchIdentity ||
		terminal.AuthorizationDecisionID != retry.AuthorizationDecisionID || terminal.AuthorizationPolicyVersion != retry.AuthorizationPolicyVersion {
		t.Fatalf("terminal/retry/binding evidence diverged: terminal=%#v retry=%#v binding=%#v", terminal, retry, got.Command.Binding)
	}
}

func TestCommandV1RetriedCommandTerminalizesWithoutDiscardingAttemptEvidence(t *testing.T) {
	results := []CommandActionResult{
		CommandActionResultExpired,
		CommandActionResultSuperseded,
		CommandActionResultRetryExhausted,
	}
	for _, result := range results {
		t.Run(string(result), func(t *testing.T) {
			command := validRetriedTerminalCommandV1(result)
			wire, err := EncodeCommandV1(command)
			if err != nil {
				t.Fatalf("EncodeCommandV1: %v", err)
			}
			decoded := DecodeCommand(wire)
			if decoded.Disposition != CommandDecodeDecoded {
				t.Fatalf("DecodeCommand = %#v, want decoded", decoded)
			}
			got := decoded.Command
			if got.Binding == nil || got.Retry == nil || got.Terminal == nil {
				t.Fatalf("terminalized retry lost evidence: %#v", got)
			}
			if got.Terminal.ClaimID != got.Retry.ClaimID || got.Terminal.AttemptID != got.Retry.AttemptID ||
				got.Terminal.BoundLaunchIdentity != got.Binding.LaunchIdentity ||
				got.Terminal.AuthorizationDecisionID != got.Retry.AuthorizationDecisionID ||
				got.Terminal.AuthorizationPolicyVersion != got.Retry.AuthorizationPolicyVersion {
				t.Fatalf("terminalized retry evidence diverged: terminal=%#v retry=%#v binding=%#v", got.Terminal, got.Retry, got.Binding)
			}

			for name, mutate := range map[string]func(*Command){
				"clear terminal evidence": func(c *Command) {
					c.Terminal.ClaimID = ""
					c.Terminal.OperationID = ""
					c.Terminal.AttemptID = ""
					c.Terminal.BoundLaunchIdentity = ""
					c.Terminal.AuthorizationDecisionID = ""
					c.Terminal.AuthorizationPolicyVersion = ""
				},
				"retarget terminal evidence": func(c *Command) {
					c.Terminal.BoundLaunchIdentity = "replacement-launch"
				},
			} {
				t.Run(name, func(t *testing.T) {
					invalid := command
					terminal := *command.Terminal
					invalid.Terminal = &terminal
					mutate(&invalid)
					if wire, err := EncodeCommandV1(invalid); err == nil {
						t.Fatalf("EncodeCommandV1(invalid) = %s, nil error", wire)
					}
				})
			}
		})
	}
}

func TestCommandV1ImmediateAuthorizationDenialPreservesCommitBinding(t *testing.T) {
	command := validImmediateCommandV1()
	command.State = CommandStateDeadLettered
	command.Terminal = authorizationDeniedTerminalV1(command.CreatedAt.Add(time.Second))
	wire, err := EncodeCommandV1(command)
	if err != nil {
		t.Fatalf("EncodeCommandV1: %v", err)
	}
	decoded := DecodeCommand(wire)
	if decoded.Disposition != CommandDecodeDecoded || decoded.Command.Binding == nil || *decoded.Command.Binding != *command.Binding {
		t.Fatalf("immediate denial did not preserve commit binding: %#v", decoded)
	}
}

func TestCommandV1RejectsIncompleteOrDivergentBindingAndAttemptEvidence(t *testing.T) {
	tests := []struct {
		name    string
		command func() Command
		mutate  func(*Command)
	}{
		{
			name:    "exact target missing commit binding",
			command: validImmediateCommandV1,
			mutate:  func(c *Command) { c.Binding = nil },
		},
		{
			name:    "exact target binding mismatch",
			command: validImmediateCommandV1,
			mutate:  func(c *Command) { c.Binding.LaunchIdentity = "replacement-launch" },
		},
		{
			name:    "active claim missing binding",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Binding = nil },
		},
		{
			name:    "claim retargets binding",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Claim.BoundLaunchIdentity = "replacement-launch" },
		},
		{
			name:    "attempt missing claim id",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Retry.ClaimID = "" },
		},
		{
			name:    "attempt operation mismatch",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Retry.OperationID = "other-command" },
		},
		{
			name:    "attempt retargets binding",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Retry.BoundLaunchIdentity = "replacement-launch" },
		},
		{
			name:    "attempt missing authorization decision",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Retry.AuthorizationDecisionID = "" },
		},
		{
			name:    "attempt missing authorization policy",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Retry.AuthorizationPolicyVersion = "" },
		},
		{
			name:    "claim and attempt ids diverge",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Claim.ID = "different-claim" },
		},
		{
			name:    "claim and attempt authorization diverges",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate:  func(c *Command) { c.Claim.AuthorizationDecisionID = "different-decision" },
		},
		{
			name:    "in flight carries concluded retry error",
			command: func() Command { return validCommandV1(CommandStateInFlight) },
			mutate: func(c *Command) {
				c.Retry.ErrorClass = CommandErrorClassProviderBusy
				c.Retry.ErrorDetail = "provider busy"
			},
		},
		{
			name: "released claim forgets binding",
			command: func() Command {
				c := validCommandV1(CommandStateInFlight)
				c.State = CommandStatePending
				c.Claim = nil
				next := c.CreatedAt.Add(time.Minute)
				c.Retry.NextEligibleAt = &next
				c.Retry.ErrorClass = CommandErrorClassProviderBusy
				c.Retry.ErrorDetail = "provider busy"
				return c
			},
			mutate: func(c *Command) { c.Binding = nil },
		},
		{
			name:    "terminal missing claim id",
			command: func() Command { return validCommandV1(CommandStateDelivered) },
			mutate:  func(c *Command) { c.Terminal.ClaimID = "" },
		},
		{
			name:    "terminal retargets binding",
			command: func() Command { return validCommandV1(CommandStateDelivered) },
			mutate:  func(c *Command) { c.Terminal.BoundLaunchIdentity = "replacement-launch" },
		},
		{
			name:    "terminal authorization diverges",
			command: func() Command { return validCommandV1(CommandStateDelivered) },
			mutate:  func(c *Command) { c.Terminal.AuthorizationPolicyVersion = "other-policy" },
		},
		{
			name:    "retry exhaustion missing last retry error",
			command: func() Command { return validRetriedTerminalCommandV1(CommandActionResultRetryExhausted) },
			mutate: func(c *Command) {
				c.Retry.ErrorClass = CommandErrorClassNone
				c.Retry.ErrorDetail = ""
			},
		},
		{
			name:    "authorization denial missing decision",
			command: func() Command { return validCommandForActionResultV1(CommandActionResultAuthorizationDenied) },
			mutate:  func(c *Command) { c.Terminal.AuthorizationDecisionID = "" },
		},
		{
			name:    "authorization denial carries binding",
			command: func() Command { return validCommandForActionResultV1(CommandActionResultAuthorizationDenied) },
			mutate: func(c *Command) {
				c.Binding = &CommandBinding{LaunchIdentity: "launch-1", BoundAt: c.CreatedAt}
			},
		},
		{
			name:    "non-attempt terminal carries authorization",
			command: func() Command { return validCommandV1(CommandStateExpired) },
			mutate:  func(c *Command) { c.Terminal.AuthorizationDecisionID = "decision-1" },
		},
		{
			name:    "terminal detail contains control",
			command: func() Command { return validCommandV1(CommandStateExpired) },
			mutate:  func(c *Command) { c.Terminal.Detail = "unsafe\ntext" },
		},
		{
			name:    "terminal detail exceeds bound",
			command: func() Command { return validCommandV1(CommandStateExpired) },
			mutate:  func(c *Command) { c.Terminal.Detail = strings.Repeat("x", MaxCommandTerminalDetailBytes+1) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			command := tc.command()
			tc.mutate(&command)
			if wire, err := EncodeCommandV1(command); err == nil {
				t.Fatalf("EncodeCommandV1(invalid) = %s, nil error", wire)
			}
			wire, err := json.Marshal(command)
			if err != nil {
				t.Fatalf("json.Marshal: %v", err)
			}
			decoded := DecodeCommand(wire)
			if decoded.Disposition != CommandDecodeDeadLetter || decoded.DeadLetterReason != CommandDeadLetterInvalidKnownVersion {
				t.Fatalf("DecodeCommand = %#v, want invalid-known dead letter", decoded)
			}
		})
	}
}

func TestCommandV1ActionResultMatrixIsClosedAndExhaustive(t *testing.T) {
	results := []CommandActionResult{
		CommandActionResultDelivered,
		CommandActionResultDuplicate,
		CommandActionResultInjectedUnconfirmed,
		CommandActionResultDeliveryUnknown,
		CommandActionResultExpired,
		CommandActionResultSuperseded,
		CommandActionResultTargetMissing,
		CommandActionResultRejected,
		CommandActionResultAuthorizationDenied,
		CommandActionResultRetryExhausted,
		CommandActionResultDeadLettered,
	}
	for _, result := range results {
		t.Run(string(result), func(t *testing.T) {
			command := validCommandForActionResultV1(result)
			if _, err := EncodeCommandV1(command); err != nil {
				t.Fatalf("valid result %q rejected: %v\ncommand=%#v", result, err, command)
			}
			for name, mutate := range map[string]func(*Command){
				"state": func(c *Command) { c.State = CommandStateUpgradeRequired },
				"stage": func(c *Command) {
					if c.Terminal.ProviderStage == ProviderStageNotEntered {
						c.Terminal.ProviderStage = ProviderStageAccepted
					} else {
						c.Terminal.ProviderStage = ProviderStageNotEntered
					}
				},
				"completion": func(c *Command) {
					if c.Terminal.Completion == CompletionStateUnknown {
						c.Terminal.Completion = CompletionStateCompleted
					} else {
						c.Terminal.Completion = CompletionStateUnknown
					}
				},
				"error": func(c *Command) { c.Terminal.ErrorClass = CommandErrorClassProviderUnavailable },
			} {
				t.Run(name, func(t *testing.T) {
					invalid := command
					terminal := *command.Terminal
					invalid.Terminal = &terminal
					mutate(&invalid)
					if wire, err := EncodeCommandV1(invalid); err == nil {
						t.Fatalf("EncodeCommandV1(invalid %s) = %s, nil error", name, wire)
					}
				})
			}
		})
	}

	unknown := validCommandForActionResultV1(CommandActionResultDeadLettered)
	unknown.Terminal.ActionResult = "future"
	if wire, err := EncodeCommandV1(unknown); err == nil {
		t.Fatalf("EncodeCommandV1(unknown action result) = %s, nil error", wire)
	}
}

func TestTrustedIngressReferenceIsDataNotAuthorization(t *testing.T) {
	typ := reflect.TypeOf(TrustedIngressReference{})
	for _, forbidden := range []string{"Authorized", "IsAuthorized", "Allow", "Allowed"} {
		if _, ok := typ.FieldByName(forbidden); ok {
			t.Fatalf("TrustedIngressReference exposes authority field %q; the codec may carry only a reference", forbidden)
		}
		if _, ok := typ.MethodByName(forbidden); ok {
			t.Fatalf("TrustedIngressReference exposes authority method %q; claim-time authorization is a later gate", forbidden)
		}
	}
}

func TestCommandClaimCannotReplaceTrustedIngressProvenance(t *testing.T) {
	typ := reflect.TypeOf(CommandClaim{})
	for _, forbidden := range []string{"TrustedIngress", "Issuer", "PrincipalID", "TenantScope", "CityScope", "CredentialClass"} {
		if _, ok := typ.FieldByName(forbidden); ok {
			t.Fatalf("CommandClaim can mint or replace provenance through field %q", forbidden)
		}
	}
}

func TestDecodeCommandDeadLetterDetailIsBoundedAndContentFree(t *testing.T) {
	secret := "SECRET-COMMAND-CONTENT-DO-NOT-ECHO"
	command := validCommandV1(CommandStatePending)
	command.State = CommandState(strings.Repeat(secret, 1500))
	wire, err := json.Marshal(command)
	if err != nil {
		t.Fatalf("json.Marshal: %v", err)
	}
	if len(wire) > MaxCommandBytes {
		t.Fatalf("fixture exceeds MaxCommandBytes: %d", len(wire))
	}
	result := DecodeCommand(wire)
	if result.Disposition != CommandDecodeDeadLetter {
		t.Fatalf("disposition = %q, want dead letter", result.Disposition)
	}
	if len(result.Detail) > MaxCommandDeadLetterDetailBytes {
		t.Fatalf("detail bytes = %d, want <= %d", len(result.Detail), MaxCommandDeadLetterDetailBytes)
	}
	if !utf8.ValidString(result.Detail) {
		t.Fatal("detail is not valid UTF-8")
	}
	if strings.Contains(result.Detail, secret) {
		t.Fatalf("detail echoed attacker command content: %q", result.Detail)
	}
}

func FuzzDecodeCommandIsTotal(f *testing.F) {
	valid, err := EncodeCommandV1(validCommandV1(CommandStatePending))
	if err != nil {
		f.Fatalf("EncodeCommandV1 seed: %v", err)
	}
	f.Add(valid)
	f.Add(validFutureCommandWire(`,"future":true`))
	f.Add([]byte(`{"version":1}`))
	f.Add([]byte{0xff})
	f.Fuzz(func(t *testing.T, wire []byte) {
		result := DecodeCommand(wire)
		switch result.Disposition {
		case CommandDecodeDecoded:
			if result.Version != CommandVersion1 {
				t.Fatalf("decoded disposition has version %d", result.Version)
			}
			if result.Routing != commandRoutingHeader(result.Command) || len(result.Raw) != 0 || result.DeadLetterReason != "" {
				t.Fatalf("decoded tagged result is internally inconsistent: %#v", result)
			}
			if _, err := EncodeCommandV1(result.Command); err != nil {
				t.Fatalf("decoder returned non-encodable command: %v", err)
			}
		case CommandDecodeUpgradeRequired:
			if result.Version <= CommandVersion1 || !bytes.Equal(result.Raw, wire) || result.Routing == (CommandRoutingHeader{}) ||
				result.Command != (Command{}) || result.DeadLetterReason != "" {
				t.Fatalf("upgrade result version/raw = %d/%q, input %q", result.Version, result.Raw, wire)
			}
		case CommandDecodeDeadLetter:
			if result.DeadLetterReason == "" || result.Routing != (CommandRoutingHeader{}) || result.Command != (Command{}) || len(result.Raw) != 0 {
				t.Fatal("dead-letter result has no typed reason")
			}
		default:
			t.Fatalf("decoder returned unknown disposition %q", result.Disposition)
		}
	})
}

func validCommandV1(state CommandState) Command {
	created := time.Date(2026, 7, 15, 10, 0, 0, 0, time.UTC)
	command := Command{
		Version: CommandVersion1,
		ID:      "command-1",
		State:   state,
		Mode:    DeliveryModeQueue,
		Target: CommandTarget{
			SessionID:            "session-1",
			IntentGeneration:     5,
			ContinuationIdentity: "continuation-1",
			Policy:               TargetPolicyContinuation,
		},
		Store: CommandStoreBinding{
			StoreUUID:    "store-1",
			RestoreEpoch: 7,
		},
		Order: CommandOrder{
			Sequence: 11,
			Revision: 13,
		},
		TrustedIngress: TrustedIngressReference{
			Issuer:           "local-ingress",
			ReferenceID:      "ingress-1",
			PrincipalID:      "principal-1",
			TenantScope:      "tenant-1",
			CityScope:        "city-1",
			CredentialClass:  "controller-ingress",
			PolicyVersion:    "policy-v1",
			PolicyDecisionID: "decision-1",
			Action:           NudgeCommandAction,
			TargetSessionID:  "session-1",
			IssuedAt:         created.Add(-time.Minute),
			ExpiresAt:        created.Add(10 * time.Minute),
		},
		Source:  CommandSourceWait,
		Message: "wake up",
		Reference: &Reference{
			Kind: CommandReferenceWait,
			ID:   "wait-1",
		},
		CreatedAt:    created,
		DeliverAfter: created.Add(time.Second),
		ExpiresAt:    created.Add(time.Hour),
	}
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	claim := &CommandClaim{
		ID:                         "claim-1",
		OwnerID:                    "controller-1",
		OperationID:                command.ID,
		AttemptID:                  "attempt-1",
		BoundLaunchIdentity:        "launch-bound-1",
		AuthorizationDecisionID:    "claim-decision-1",
		AuthorizationPolicyVersion: "policy-v2",
		ClaimedAt:                  created.Add(2 * time.Second),
		LeaseUntil:                 created.Add(time.Minute),
	}
	binding := &CommandBinding{
		LaunchIdentity: claim.BoundLaunchIdentity,
		BoundAt:        claim.ClaimedAt,
	}
	attempt := &CommandRetry{
		AttemptCount:               1,
		LastAttemptAt:              claim.ClaimedAt,
		ClaimID:                    claim.ID,
		OperationID:                claim.OperationID,
		AttemptID:                  claim.AttemptID,
		BoundLaunchIdentity:        claim.BoundLaunchIdentity,
		AuthorizationDecisionID:    claim.AuthorizationDecisionID,
		AuthorizationPolicyVersion: claim.AuthorizationPolicyVersion,
	}
	switch state {
	case CommandStateInFlight:
		command.Binding = binding
		command.Retry = attempt
		command.Claim = claim
	case CommandStateDelivered, CommandStateInjectedUnconfirmed, CommandStateDeliveryUnknown:
		command.Binding = binding
		attempt.AttemptCount = 2
		attempt.LastAttemptAt = created.Add(2 * time.Minute)
		command.Retry = attempt
		command.Terminal = &CommandTerminal{
			At:                         created.Add(2 * time.Minute),
			ActionResult:               CommandActionResult(state),
			ClaimID:                    claim.ID,
			OperationID:                command.ID,
			AttemptID:                  claim.AttemptID,
			BoundLaunchIdentity:        claim.BoundLaunchIdentity,
			AuthorizationDecisionID:    claim.AuthorizationDecisionID,
			AuthorizationPolicyVersion: claim.AuthorizationPolicyVersion,
			ProviderStage:              ProviderStageAccepted,
			Completion:                 CompletionStateCompleted,
		}
		if state == CommandStateDeliveryUnknown {
			command.Terminal.ErrorClass = CommandErrorClassProviderAmbiguous
			command.Terminal.Detail = "provider result is ambiguous"
			command.Terminal.ProviderStage = ProviderStageMayHaveEntered
			command.Terminal.Completion = CompletionStateUnknown
		}
	case CommandStateExpired, CommandStateSuperseded, CommandStateDeadLettered:
		errorClass := CommandErrorClassExpired
		if state == CommandStateSuperseded {
			errorClass = CommandErrorClassSuperseded
		}
		if state == CommandStateDeadLettered {
			errorClass = CommandErrorClassInvalidCommand
		}
		terminalAt := created.Add(2 * time.Minute)
		if state == CommandStateExpired {
			terminalAt = command.ExpiresAt
		}
		command.Terminal = &CommandTerminal{
			At:            terminalAt,
			ActionResult:  CommandActionResult(state),
			ErrorClass:    errorClass,
			Detail:        strings.ReplaceAll(string(state), "_", " "),
			ProviderStage: ProviderStageNotEntered,
			Completion:    CompletionStateNotCompleted,
		}
	}
	return command
}

func validCommandForActionResultV1(result CommandActionResult) Command {
	switch result {
	case CommandActionResultDelivered:
		return validCommandV1(CommandStateDelivered)
	case CommandActionResultDuplicate:
		command := validCommandV1(CommandStateDelivered)
		command.Terminal.ActionResult = CommandActionResultDuplicate
		return command
	case CommandActionResultInjectedUnconfirmed:
		return validCommandV1(CommandStateInjectedUnconfirmed)
	case CommandActionResultDeliveryUnknown:
		return validCommandV1(CommandStateDeliveryUnknown)
	case CommandActionResultExpired:
		return validCommandV1(CommandStateExpired)
	case CommandActionResultSuperseded:
		return validCommandV1(CommandStateSuperseded)
	case CommandActionResultTargetMissing:
		command := validCommandV1(CommandStateDelivered)
		command.State = CommandStateSuperseded
		command.Terminal.ActionResult = CommandActionResultTargetMissing
		command.Terminal.ErrorClass = CommandErrorClassTargetMissing
		command.Terminal.Detail = "exact target is absent"
		command.Terminal.ProviderStage = ProviderStageRejected
		command.Terminal.Completion = CompletionStateNotCompleted
		return command
	case CommandActionResultRejected:
		command := validCommandV1(CommandStateDelivered)
		command.State = CommandStateDeadLettered
		command.Terminal.ActionResult = CommandActionResultRejected
		command.Terminal.ErrorClass = CommandErrorClassProviderRejected
		command.Terminal.Detail = "provider rejected the command"
		command.Terminal.ProviderStage = ProviderStageRejected
		command.Terminal.Completion = CompletionStateNotCompleted
		return command
	case CommandActionResultAuthorizationDenied:
		command := validCommandV1(CommandStatePending)
		command.State = CommandStateDeadLettered
		command.Terminal = authorizationDeniedTerminalV1(command.CreatedAt.Add(2 * time.Second))
		return command
	case CommandActionResultRetryExhausted:
		command := validRetriedTerminalCommandV1(CommandActionResultRetryExhausted)
		return command
	case CommandActionResultDeadLettered:
		return validCommandV1(CommandStateDeadLettered)
	default:
		panic("test fixture requested unknown action result " + string(result))
	}
}

func validImmediateCommandV1() Command {
	command := validCommandV1(CommandStatePending)
	command.Mode = DeliveryModeImmediate
	command.DeliverAfter = command.CreatedAt
	command.Target.Policy = TargetPolicyExactLaunch
	command.Target.ContinuationIdentity = ""
	command.Target.LaunchIdentity = "launch-immediate"
	command.Binding = &CommandBinding{
		LaunchIdentity: command.Target.LaunchIdentity,
		BoundAt:        command.CreatedAt,
	}
	command.TrustedIngress.PayloadDigest = ComputeCommandPayloadDigest(command)
	return command
}

func validRetriedTerminalCommandV1(result CommandActionResult) Command {
	command := validCommandV1(CommandStateInFlight)
	command.Claim = nil
	command.Retry.AttemptCount = 3
	command.Retry.ErrorClass = CommandErrorClassProviderBusy
	command.Retry.ErrorDetail = "provider remained busy"
	command.State = CommandStateSuperseded
	terminalAt := command.Retry.LastAttemptAt.Add(time.Minute)
	terminal := &CommandTerminal{
		At:                         terminalAt,
		ActionResult:               CommandActionResultSuperseded,
		ErrorClass:                 CommandErrorClassSuperseded,
		Detail:                     "target intent was superseded",
		ClaimID:                    command.Retry.ClaimID,
		OperationID:                command.Retry.OperationID,
		AttemptID:                  command.Retry.AttemptID,
		BoundLaunchIdentity:        command.Retry.BoundLaunchIdentity,
		AuthorizationDecisionID:    command.Retry.AuthorizationDecisionID,
		AuthorizationPolicyVersion: command.Retry.AuthorizationPolicyVersion,
		ProviderStage:              ProviderStageNotEntered,
		Completion:                 CompletionStateNotCompleted,
	}
	switch result {
	case CommandActionResultExpired:
		command.State = CommandStateExpired
		terminal.At = command.ExpiresAt
		terminal.ActionResult = CommandActionResultExpired
		terminal.ErrorClass = CommandErrorClassExpired
		terminal.Detail = "delivery window expired"
	case CommandActionResultSuperseded:
	case CommandActionResultRetryExhausted:
		command.State = CommandStateDeadLettered
		terminal.ActionResult = CommandActionResultRetryExhausted
		terminal.ErrorClass = CommandErrorClassRetryExhausted
		terminal.Detail = "retry budget exhausted"
		terminal.ProviderStage = ProviderStageRejected
	default:
		panic("test fixture requested unsupported retried terminal result " + string(result))
	}
	command.Terminal = terminal
	return command
}

func authorizationDeniedTerminalV1(at time.Time) *CommandTerminal {
	return &CommandTerminal{
		At:                         at,
		ActionResult:               CommandActionResultAuthorizationDenied,
		ErrorClass:                 CommandErrorClassAuthorizationDenied,
		Detail:                     "claim-time authorization denied",
		AuthorizationDecisionID:    "denial-1",
		AuthorizationPolicyVersion: "policy-v2",
		ProviderStage:              ProviderStageNotEntered,
		Completion:                 CompletionStateNotCompleted,
	}
}

func validFutureCommandWire(suffix string) []byte {
	return []byte(`{"version":2` +
		`,"id":"command-future"` +
		`,"target":{"session_id":"session-1","intent_generation":5}` +
		`,"store":{"store_uuid":"store-1","restore_epoch":7}` +
		`,"order":{"sequence":11,"revision":13}` + suffix + `}`)
}
