package main

import (
	"context"
	"io"
	"sync"
	"time"

	"github.com/gastownhall/gascity/internal/telemetry"
)

type nudgeKeyQueueDelayState = telemetry.NudgeKeyQueueDelayState

const (
	nudgeKeyQueueDelayObserved       = telemetry.NudgeKeyQueueDelayObserved
	nudgeKeyQueueDelayUnavailable    = telemetry.NudgeKeyQueueDelayUnavailable
	nudgeKeyQueueDelayClockRegressed = telemetry.NudgeKeyQueueDelayClockRegressed
)

type nudgeKeyScopeCertification uint8

const nudgeKeyScopeStoreLineageVerified nudgeKeyScopeCertification = 1

type nudgeKeyAuthorizationState uint8

const nudgeKeyAuthorizationNotEvaluated nudgeKeyAuthorizationState = 1

// nudgeKeySchedulingObservation contains only bounded scheduler facts. Stable
// keys, command IDs, aliases, message content, and any other user-controlled
// identity are deliberately unrepresentable here.
type nudgeKeySchedulingObservation struct {
	CauseBits          uint8
	WorkqueueReplay    bool
	QueueDelay         time.Duration
	QueueDelayState    nudgeKeyQueueDelayState
	ScopeCertification nudgeKeyScopeCertification
	Authorization      nudgeKeyAuthorizationState
	EffectsAdmissible  bool
}

func newNudgeKeySchedulingObservation(batch nudgeReconcileBatch, now time.Time) nudgeKeySchedulingObservation {
	delayState := nudgeKeyQueueDelayUnavailable
	var delay time.Duration
	if !batch.FirstEnqueuedAt.IsZero() {
		delay = now.Sub(batch.FirstEnqueuedAt)
		if delay < 0 {
			delay = 0
			delayState = nudgeKeyQueueDelayClockRegressed
		} else {
			delayState = nudgeKeyQueueDelayObserved
		}
	}
	return nudgeKeySchedulingObservation{
		CauseBits:          uint8(batch.Causes),
		WorkqueueReplay:    batch.WorkqueueReplay,
		QueueDelay:         delay,
		QueueDelayState:    delayState,
		ScopeCertification: nudgeKeyScopeStoreLineageVerified,
		Authorization:      nudgeKeyAuthorizationNotEvaluated,
		EffectsAdmissible:  false,
	}
}

func observeNudgeKeyScheduling(ctx context.Context, batch nudgeReconcileBatch, now time.Time, warnings *nudgeKeyObservationWarnings) {
	observation := newNudgeKeySchedulingObservation(batch, now)
	emitNudgeKeySchedulingObservation(ctx, observation, warnings, recordNudgeKeySchedulingObservation)
}

type nudgeKeySchedulingEmitter func(context.Context, nudgeKeySchedulingObservation) error

func emitNudgeKeySchedulingObservation(ctx context.Context, observation nudgeKeySchedulingObservation, warnings *nudgeKeyObservationWarnings, emit nudgeKeySchedulingEmitter) {
	if invokeNudgeKeySchedulingEmitter(ctx, observation, emit) {
		warnings.warn()
	}
}

func invokeNudgeKeySchedulingEmitter(ctx context.Context, observation nudgeKeySchedulingObservation, emit nudgeKeySchedulingEmitter) (failed bool) {
	defer func() {
		if recover() != nil {
			failed = true
		}
	}()
	if emit == nil {
		return true
	}
	return emit(ctx, observation) != nil
}

type nudgeKeyObservationWarnings struct {
	once   sync.Once
	stderr io.Writer
}

func newNudgeKeyObservationWarnings(stderr io.Writer) *nudgeKeyObservationWarnings {
	return &nudgeKeyObservationWarnings{stderr: stderr}
}

func (warnings *nudgeKeyObservationWarnings) warn() {
	if warnings == nil {
		return
	}
	warnings.once.Do(func() {
		writeNudgeKeyObservationWarning(warnings.stderr)
	})
}

func writeNudgeKeyObservationWarning(stderr io.Writer) {
	defer func() {
		_ = recover()
	}()
	if stderr != nil {
		_, _ = io.WriteString(stderr, "nudge keyed shadow scheduling observation failed\n")
	}
}

func recordNudgeKeySchedulingObservation(ctx context.Context, observation nudgeKeySchedulingObservation) error {
	return telemetry.RecordNudgeKeyScheduling(ctx, telemetry.NudgeKeySchedulingRecord{
		CauseBits:       observation.CauseBits,
		WorkqueueReplay: observation.WorkqueueReplay,
		QueueDelay:      observation.QueueDelay,
		QueueDelayState: observation.QueueDelayState,
	})
}
