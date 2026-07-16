// Package nudgeparity compares normalized legacy and keyed nudge plans without
// owning a queue, durable command, worker, runtime provider, or tmux effect.
package nudgeparity

import (
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// MaxTextBytes bounds every retained identity and watermark string.
const MaxTextBytes = 256

var (
	// ErrInvalidObservation reports an observation that cannot be compared
	// safely. Invalid observations are never retained.
	ErrInvalidObservation = errors.New("invalid nudge parity observation")
	// ErrClockRegression reports that the injected comparator clock moved
	// backwards. State is left unchanged so expiry order stays deterministic.
	ErrClockRegression = errors.New("nudge parity clock regressed")
	// ErrInvalidClock reports a zero timestamp from the injected clock.
	ErrInvalidClock = errors.New("invalid nudge parity clock")
)

// Side identifies which independently produced plan an observation represents.
type Side uint8

const (
	// SideUnknown is the invalid zero value.
	SideUnknown Side = iota
	// SideExpected is the currently authoritative legacy plan.
	SideExpected
	// SideActual is the independently computed keyed plan.
	SideActual
)

// String returns the bounded telemetry spelling for a side.
func (s Side) String() string {
	switch s {
	case SideExpected:
		return "expected"
	case SideActual:
		return "actual"
	default:
		return "unknown"
	}
}

func (s Side) valid() bool { return s == SideExpected || s == SideActual }

// Decision is the normalized outcome of planning one nudge operation.
type Decision uint8

const (
	// DecisionUnknown is the invalid zero value.
	DecisionUnknown Decision = iota
	// DecisionExecute selects immediate execution.
	DecisionExecute
	// DecisionPark waits for an external condition without executing.
	DecisionPark
	// DecisionReject terminally refuses the operation.
	DecisionReject
	// DecisionRetry defers the operation for a safe later attempt.
	DecisionRetry
	// DecisionNoop determines that no provider action is required.
	DecisionNoop
)

func (d Decision) valid() bool { return d >= DecisionExecute && d <= DecisionNoop }

// Action is the normalized provider action selected by a plan.
type Action uint8

const (
	// ActionUnknown is the invalid zero value.
	ActionUnknown Action = iota
	// ActionNone performs no provider action.
	ActionNone
	// ActionNudge enters the provider's nudge operation.
	ActionNudge
)

func (a Action) valid() bool { return a == ActionNone || a == ActionNudge }

// Plan is the small, comparable output produced independently by each planner.
type Plan struct {
	Decision Decision
	Action   Action
}

// Input captures the immutable operation target presented to both planners.
// Incomplete inputs pair as incomparable rather than being silently normalized.
type Input struct {
	CommandDigest    string
	TargetSession    string
	TargetGeneration uint64
	TargetLaunch     string
}

func (i Input) complete() bool {
	return validText(i.CommandDigest, false) &&
		validText(i.TargetSession, false) &&
		i.TargetGeneration != 0 &&
		validText(i.TargetLaunch, false)
}

func (i Input) validPartial() bool {
	return validText(i.CommandDigest, true) &&
		validText(i.TargetSession, true) &&
		validText(i.TargetLaunch, true)
}

// Watermarks bind a plan to the same durable, configuration, runtime, and
// physical-owner view. Every field must be present before plans are comparable.
type Watermarks struct {
	StoreLineage    string
	DurableRevision uint64
	ConfigRevision  string
	RuntimeRevision uint64
	OwnerEpoch      uint64
}

func (w Watermarks) complete() bool {
	return validText(w.StoreLineage, false) &&
		w.DurableRevision != 0 &&
		validText(w.ConfigRevision, false) &&
		w.RuntimeRevision != 0 &&
		w.OwnerEpoch != 0
}

func (w Watermarks) validPartial() bool {
	return validText(w.StoreLineage, true) && validText(w.ConfigRevision, true)
}

// NativeStartProof identifies evidence captured at the provider's native T8
// entry boundary. No other proof kind is allowed to produce native latency.
type NativeStartProof uint8

const (
	// NativeStartProofNone explicitly records that no native-entry evidence is
	// present and therefore native-start latency must not be reported.
	NativeStartProofNone NativeStartProof = iota
	// NativeStartProofT8 records evidence captured at native provider entry.
	NativeStartProofT8
)

// TimingEvidence contains optional monotonic event timestamps. Wall-clock
// durations are reported only when their complete evidence chain is present.
type TimingEvidence struct {
	EnqueuedAt       time.Time
	PlannedAt        time.Time
	NativeStartedAt  time.Time
	NativeStartProof NativeStartProof
}

// Observation is one immutable, normalized planner observation.
type Observation struct {
	OperationID string
	Side        Side
	Input       Input
	Watermarks  Watermarks
	Plan        Plan
	CapturedAt  time.Time
	Timing      TimingEvidence
}

func (o Observation) validate(now time.Time) error {
	if !validText(o.OperationID, false) {
		return fmt.Errorf("%w: operation id is empty or non-canonical", ErrInvalidObservation)
	}
	if !o.Side.valid() {
		return fmt.Errorf("%w: side is unknown", ErrInvalidObservation)
	}
	if !o.Input.validPartial() {
		return fmt.Errorf("%w: input identity is non-canonical", ErrInvalidObservation)
	}
	if !o.Watermarks.validPartial() {
		return fmt.Errorf("%w: watermark identity is non-canonical", ErrInvalidObservation)
	}
	if !o.Plan.Decision.valid() || !o.Plan.Action.valid() {
		return fmt.Errorf("%w: plan is unknown", ErrInvalidObservation)
	}
	if o.CapturedAt.IsZero() || o.CapturedAt.After(now) {
		return fmt.Errorf("%w: capture timestamp is zero or in the future", ErrInvalidObservation)
	}
	if err := o.Timing.validate(o.CapturedAt, now); err != nil {
		return err
	}
	return nil
}

func (e TimingEvidence) validate(capturedAt, now time.Time) error {
	if timestampFollowsCapture(e.EnqueuedAt, capturedAt, now) {
		return fmt.Errorf("%w: enqueue timestamp follows capture", ErrInvalidObservation)
	}
	if timestampFollowsCapture(e.PlannedAt, capturedAt, now) {
		return fmt.Errorf("%w: plan timestamp follows capture", ErrInvalidObservation)
	}
	if timestampFollowsCapture(e.NativeStartedAt, capturedAt, now) {
		return fmt.Errorf("%w: native start timestamp follows capture", ErrInvalidObservation)
	}
	if !e.PlannedAt.IsZero() {
		if e.EnqueuedAt.IsZero() || e.PlannedAt.Before(e.EnqueuedAt) {
			return fmt.Errorf("%w: planning evidence has no ordered enqueue", ErrInvalidObservation)
		}
	}
	switch e.NativeStartProof {
	case NativeStartProofNone:
		if !e.NativeStartedAt.IsZero() {
			return fmt.Errorf("%w: native timestamp lacks T8 proof", ErrInvalidObservation)
		}
	case NativeStartProofT8:
		if e.NativeStartedAt.IsZero() || e.EnqueuedAt.IsZero() || e.NativeStartedAt.Before(e.EnqueuedAt) {
			return fmt.Errorf("%w: T8 evidence has no ordered enqueue", ErrInvalidObservation)
		}
	default:
		return fmt.Errorf("%w: native proof kind is unknown", ErrInvalidObservation)
	}
	return nil
}

func timestampFollowsCapture(value, capturedAt, now time.Time) bool {
	return !value.IsZero() && (value.After(capturedAt) || value.After(now))
}

func validText(value string, allowEmpty bool) bool {
	if value == "" {
		return allowEmpty
	}
	if len(value) > MaxTextBytes || !utf8.ValidString(value) || value != strings.TrimSpace(value) {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Classification is a bounded terminal comparison outcome.
type Classification uint8

const (
	// ClassificationUnknown is the invalid zero value.
	ClassificationUnknown Classification = iota
	// ClassificationSame identifies equivalent comparable plans.
	ClassificationSame
	// ClassificationDivergent identifies different comparable plans.
	ClassificationDivergent
	// ClassificationIncomparable identifies plans from unequal input views.
	ClassificationIncomparable
	// ClassificationMissingExpected identifies an actual-only observation.
	ClassificationMissingExpected
	// ClassificationMissingActual identifies an expected-only observation.
	ClassificationMissingActual
	// ClassificationDuplicate identifies a repeated same-side observation.
	ClassificationDuplicate
	// ClassificationLate identifies a counterpart arriving after terminal loss.
	ClassificationLate
	classificationLimit
)

// String returns the bounded telemetry spelling for a classification.
func (c Classification) String() string {
	switch c {
	case ClassificationSame:
		return "same"
	case ClassificationDivergent:
		return "divergent"
	case ClassificationIncomparable:
		return "incomparable"
	case ClassificationMissingExpected:
		return "missing_expected"
	case ClassificationMissingActual:
		return "missing_actual"
	case ClassificationDuplicate:
		return "duplicate"
	case ClassificationLate:
		return "late"
	default:
		return "unknown"
	}
}

// Reason is a bounded explanation for a comparison outcome.
type Reason uint8

const (
	// ReasonUnknown is the invalid zero value.
	ReasonUnknown Reason = iota
	// ReasonEquivalent identifies equal inputs, watermarks, and plans.
	ReasonEquivalent
	// ReasonPlanMismatch identifies unequal plans on equivalent views.
	ReasonPlanMismatch
	// ReasonInputIncomplete identifies a missing immutable input field.
	ReasonInputIncomplete
	// ReasonInputMismatch identifies different immutable inputs.
	ReasonInputMismatch
	// ReasonWatermarkIncomplete identifies a missing comparison watermark.
	ReasonWatermarkIncomplete
	// ReasonWatermarkMismatch identifies different comparison watermarks.
	ReasonWatermarkMismatch
	// ReasonExpired identifies retention expiry.
	ReasonExpired
	// ReasonCapacity identifies deterministic oldest-entry eviction.
	ReasonCapacity
	// ReasonFlush identifies graceful reset or shutdown.
	ReasonFlush
	// ReasonDuplicateIdentical identifies an exact same-side replay.
	ReasonDuplicateIdentical
	// ReasonDuplicateConflicting identifies a changed same-side replay.
	ReasonDuplicateConflicting
	// ReasonCounterpartAfterTerminal identifies a counterpart after loss.
	ReasonCounterpartAfterTerminal
	reasonLimit
)

// String returns the bounded telemetry spelling for a reason.
func (r Reason) String() string {
	switch r {
	case ReasonEquivalent:
		return "equivalent"
	case ReasonPlanMismatch:
		return "plan_mismatch"
	case ReasonInputIncomplete:
		return "input_incomplete"
	case ReasonInputMismatch:
		return "input_mismatch"
	case ReasonWatermarkIncomplete:
		return "watermark_incomplete"
	case ReasonWatermarkMismatch:
		return "watermark_mismatch"
	case ReasonExpired:
		return "expired"
	case ReasonCapacity:
		return "capacity"
	case ReasonFlush:
		return "flush"
	case ReasonDuplicateIdentical:
		return "duplicate_identical"
	case ReasonDuplicateConflicting:
		return "duplicate_conflicting"
	case ReasonCounterpartAfterTerminal:
		return "counterpart_after_terminal"
	default:
		return "unknown"
	}
}

// Latency contains only durations backed by a complete evidence chain.
type Latency struct {
	EnqueueToPlan           time.Duration
	HasEnqueueToPlan        bool
	EnqueueToNativeStart    time.Duration
	HasEnqueueToNativeStart bool
}

func latencyFor(observation Observation) Latency {
	var latency Latency
	if !observation.Timing.EnqueuedAt.IsZero() && !observation.Timing.PlannedAt.IsZero() {
		latency.EnqueueToPlan = observation.Timing.PlannedAt.Sub(observation.Timing.EnqueuedAt)
		latency.HasEnqueueToPlan = true
	}
	if observation.Timing.NativeStartProof == NativeStartProofT8 &&
		!observation.Timing.EnqueuedAt.IsZero() &&
		!observation.Timing.NativeStartedAt.IsZero() {
		latency.EnqueueToNativeStart = observation.Timing.NativeStartedAt.Sub(observation.Timing.EnqueuedAt)
		latency.HasEnqueueToNativeStart = true
	}
	return latency
}

// Result is one bounded structured comparison record. Incoming is populated
// for duplicate and late observations so callers can diagnose the discrepancy.
type Result struct {
	OperationID     string
	Classification  Classification
	Reason          Reason
	Expected        Observation
	HasExpected     bool
	Actual          Observation
	HasActual       bool
	Incoming        Observation
	HasIncoming     bool
	ExpectedLatency Latency
	ActualLatency   Latency
}

// Snapshot is a fixed-cardinality counter and retained-state snapshot.
type Snapshot struct {
	Pending             int
	Recent              int
	InvalidObservations uint64
	ClockRegressions    uint64
	classifications     [classificationLimit]uint64
	reasons             [reasonLimit]uint64
}

// Count returns the count for one bounded classification.
func (s Snapshot) Count(classification Classification) uint64 {
	if classification <= ClassificationUnknown || classification >= classificationLimit {
		return 0
	}
	return s.classifications[classification]
}

// ReasonCount returns the count for one bounded reason.
func (s Snapshot) ReasonCount(reason Reason) uint64 {
	if reason <= ReasonUnknown || reason >= reasonLimit {
		return 0
	}
	return s.reasons[reason]
}
