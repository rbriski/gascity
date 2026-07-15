package main

import (
	"io"
	"sync"

	"github.com/gastownhall/gascity/internal/telemetry"
)

type nudgeKeyBacklogRegistrar func(telemetry.NudgeKeyBacklogObserver) (telemetry.NudgeKeyBacklogUnregister, error)

func newNudgeKeyBacklogRecord(snapshot nudgeKeyBacklogSnapshot) telemetry.NudgeKeyBacklogRecord {
	record := telemetry.NudgeKeyBacklogRecord{
		Depth:     snapshot.Depth,
		OldestAge: snapshot.OldestAge,
	}
	switch snapshot.AgeState {
	case nudgeKeyBacklogAgeEmpty:
		record.AgeState = telemetry.NudgeKeyBacklogAgeEmpty
	case nudgeKeyBacklogAgeObserved:
		record.AgeState = telemetry.NudgeKeyBacklogAgeObserved
	case nudgeKeyBacklogAgeUnavailable:
		record.AgeState = telemetry.NudgeKeyBacklogAgeUnavailable
	case nudgeKeyBacklogAgeClockRegressed:
		record.AgeState = telemetry.NudgeKeyBacklogAgeClockRegressed
	}
	return record
}

func startNudgeKeyBacklogObservation(controller *nudgeKeyController, warnings *nudgeKeyBacklogWarnings) func() {
	return startNudgeKeyBacklogObservationWithRegistrar(controller, warnings, telemetry.RegisterNudgeKeyBacklogObserver)
}

func startNudgeKeyBacklogObservationWithRegistrar(controller *nudgeKeyController, warnings *nudgeKeyBacklogWarnings, register nudgeKeyBacklogRegistrar) func() {
	if controller == nil {
		return func() {}
	}
	if register == nil {
		warnings.warn()
		return func() {}
	}
	observer := func() telemetry.NudgeKeyBacklogRecord {
		return newNudgeKeyBacklogRecord(controller.backlogSnapshot())
	}
	unregister, failed := invokeNudgeKeyBacklogRegistration(register, observer)
	if failed || unregister == nil {
		warnings.warn()
		return func() {}
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			if invokeNudgeKeyBacklogUnregister(unregister) {
				warnings.warn()
			}
		})
	}
}

func invokeNudgeKeyBacklogRegistration(register nudgeKeyBacklogRegistrar, observer telemetry.NudgeKeyBacklogObserver) (unregister telemetry.NudgeKeyBacklogUnregister, failed bool) {
	defer func() {
		if recover() != nil {
			unregister = nil
			failed = true
		}
	}()
	unregister, err := register(observer)
	return unregister, err != nil
}

func invokeNudgeKeyBacklogUnregister(unregister telemetry.NudgeKeyBacklogUnregister) (failed bool) {
	defer func() {
		if recover() != nil {
			failed = true
		}
	}()
	if unregister == nil {
		return true
	}
	return unregister() != nil
}

type nudgeKeyBacklogWarnings struct {
	once   sync.Once
	stderr io.Writer
}

func newNudgeKeyBacklogWarnings(stderr io.Writer) *nudgeKeyBacklogWarnings {
	return &nudgeKeyBacklogWarnings{stderr: stderr}
}

func (warnings *nudgeKeyBacklogWarnings) warn() {
	if warnings == nil {
		return
	}
	warnings.once.Do(func() {
		defer func() { _ = recover() }()
		if warnings.stderr != nil {
			_, _ = io.WriteString(warnings.stderr, "nudge keyed backlog observation unavailable\n")
		}
	})
}
