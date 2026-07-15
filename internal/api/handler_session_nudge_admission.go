package api

import (
	"context"
	"errors"
	"time"

	"github.com/gastownhall/gascity/internal/api/apierr"
	"github.com/gastownhall/gascity/internal/nudgequeue"
)

// SessionNudgeAdmission is the durable domain boundary used by the typed
// session-nudge endpoint. Implementations authenticate requester evidence from
// ctx and admit the canonical nudgequeue request; the API never invokes a
// runtime or worker effect directly.
type SessionNudgeAdmission interface {
	AdmitSessionNudge(context.Context, string, nudgequeue.NudgeIngressRequest) (nudgequeue.NudgeIngressResult, error)
}

// SessionNudgeAdmissionRequestBody is the caller-owned portion of one durable
// session nudge. Authenticated requester and credential evidence are
// deliberately absent: trusted middleware supplies them through the request
// context consumed by SessionNudgeAdmission.
type SessionNudgeAdmissionRequestBody struct {
	_                    struct{}                `json:"-" additionalProperties:"false"`
	RequestID            string                  `json:"request_id" minLength:"1" doc:"Caller-generated idempotency identity for this durable nudge."`
	Mode                 nudgequeue.DeliveryMode `json:"mode" enum:"queue,wait_idle,immediate" doc:"Durable delivery mode. Queue and wait_idle require continuation targeting; immediate requires exact-launch targeting."`
	IntentGeneration     uint64                  `json:"intent_generation" minimum:"1" doc:"Monotonic target-session intent generation."`
	ContinuationIdentity string                  `json:"continuation_identity,omitempty" doc:"Stable continuation identity, required for continuation targeting."`
	LaunchIdentity       string                  `json:"launch_identity,omitempty" doc:"Exact immutable launch identity, required for exact-launch targeting."`
	TargetPolicy         nudgequeue.TargetPolicy `json:"target_policy" enum:"continuation,exact_launch" doc:"Target binding policy. Use continuation with queue or wait_idle; use exact_launch with immediate."`
	Message              string                  `json:"message" minLength:"1" doc:"Message to deliver to the target session."`
	DeliverAfter         *time.Time              `json:"deliver_after,omitempty" doc:"Optional absolute earliest delivery time. Omit for authority-selected immediate eligibility."`
	ExpiresAt            time.Time               `json:"expires_at" doc:"Absolute expiry time for this nudge."`
}

// SessionNudgeAdmissionInput is the Huma input for POST
// /v0/city/{cityName}/session/{id}/nudges.
type SessionNudgeAdmissionInput struct {
	CityScope
	ID   string `path:"id" minLength:"1" pattern:"\\S" doc:"Canonical target session ID."`
	Body SessionNudgeAdmissionRequestBody
}

// SessionNudgeAdmissionResponseBody reports the durable command identity and
// lifecycle state returned by the canonical admission boundary.
type SessionNudgeAdmissionResponseBody struct {
	CommandID string                  `json:"command_id" doc:"Durable command identity."`
	Status    nudgequeue.CommandState `json:"status" enum:"pending,in_flight,delivered,injected_unconfirmed,delivery_unknown,expired,superseded,dead_lettered,upgrade_required" doc:"Current durable command state."`
	Created   bool                    `json:"created" doc:"True when this call created the command; false for an idempotent replay."`
}

// SessionNudgeAdmissionOutput is the Huma output for POST
// /v0/city/{cityName}/session/{id}/nudges.
type SessionNudgeAdmissionOutput struct {
	Body SessionNudgeAdmissionResponseBody
}

func (s *Server) humaHandleSessionNudgeAdmission(
	ctx context.Context,
	input *SessionNudgeAdmissionInput,
) (*SessionNudgeAdmissionOutput, error) {
	if s == nil || isNil(s.sessionNudgeAdmission) {
		return nil, apierr.ServiceUnavailable.Msg("durable session nudge admission is unavailable")
	}

	deliverAfter := time.Time{}
	if input.Body.DeliverAfter != nil {
		deliverAfter = *input.Body.DeliverAfter
	}
	result, err := s.sessionNudgeAdmission.AdmitSessionNudge(ctx, input.CityName, nudgequeue.NudgeIngressRequest{
		RequestID: input.Body.RequestID,
		Mode:      input.Body.Mode,
		Target: nudgequeue.CommandTarget{
			SessionID:            input.ID,
			IntentGeneration:     input.Body.IntentGeneration,
			ContinuationIdentity: input.Body.ContinuationIdentity,
			LaunchIdentity:       input.Body.LaunchIdentity,
			Policy:               input.Body.TargetPolicy,
		},
		Source:       nudgequeue.CommandSourceSession,
		Message:      input.Body.Message,
		DeliverAfter: deliverAfter,
		ExpiresAt:    input.Body.ExpiresAt,
	})
	if err != nil {
		return nil, sessionNudgeAdmissionProblem(err)
	}
	if result.Entry.Command == nil {
		return nil, apierr.Internal.Msg("durable session nudge admission returned no command")
	}
	return &SessionNudgeAdmissionOutput{Body: SessionNudgeAdmissionResponseBody{
		CommandID: result.Entry.Command.ID,
		Status:    result.Entry.Command.State,
		Created:   result.Created,
	}}, nil
}

func sessionNudgeAdmissionProblem(err error) error {
	switch {
	case errors.Is(err, nudgequeue.ErrNudgeAuthorizationDenied):
		return apierr.Forbidden.Msg("durable session nudge admission was denied")
	case errors.Is(err, nudgequeue.ErrNudgeAuthorizationInvalid),
		errors.Is(err, nudgequeue.ErrCommandRepositoryInvalidRequest):
		return apierr.InvalidRequest.Msg("durable session nudge request is invalid")
	case errors.Is(err, nudgequeue.ErrCommandRepositoryIdempotencyConflict):
		return apierr.ConflictWrongState.Msg("request ID is bound to different durable nudge content")
	case errors.Is(err, context.DeadlineExceeded):
		return apierr.GatewayTimeout.Msg("durable session nudge admission timed out")
	case errors.Is(err, context.Canceled),
		errors.Is(err, nudgequeue.ErrNudgeAuthorizationUnknown),
		errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityConflict),
		errors.Is(err, nudgequeue.ErrLocalNudgeAuthorityUnavailable),
		errors.Is(err, nudgequeue.ErrCommandRepositoryUnsupported),
		errors.Is(err, nudgequeue.ErrCommandRepositorySchemaSkew),
		errors.Is(err, nudgequeue.ErrCommandRepositoryLineage),
		errors.Is(err, nudgequeue.ErrCommandRepositoryPartition),
		errors.Is(err, nudgequeue.ErrCommandRepositorySnapshotLimit),
		errors.Is(err, nudgequeue.ErrCommandRepositoryCheckpointRequired),
		errors.Is(err, nudgequeue.ErrCommandRepositoryRecord),
		errors.Is(err, nudgequeue.ErrCommandRepositoryCheckpointConflict),
		errors.Is(err, nudgequeue.ErrRestoreAnchorAdmission),
		errors.Is(err, nudgequeue.ErrRestoreAnchorConflict),
		errors.Is(err, nudgequeue.ErrRestoreAnchorBusy),
		errors.Is(err, nudgequeue.ErrRestoreAnchorUnsafePath),
		errors.Is(err, nudgequeue.ErrRestoreAnchorDurabilityUncertain):
		return apierr.ServiceUnavailable.Msg("durable session nudge admission is unavailable")
	default:
		return apierr.Internal.Msg("durable session nudge admission failed")
	}
}
