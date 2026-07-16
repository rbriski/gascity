package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestCityRuntimeKeyedReadinessRefusalPreservesPreexistingSession(t *testing.T) {
	fixture := newKeyedReadinessRunFixture(t, func(context.Context) (nudgeCommandSource, error) {
		return nil, errors.Join(errNudgeCommandSourceUnverified, errors.New("authority schema invariant failed"))
	})

	fixture.cr.run(t.Context())

	if *fixture.openCalls != 1 {
		t.Fatalf("keyed command source open calls = %d, want 1; stderr:\n%s", *fixture.openCalls, fixture.stderr.String())
	}
	if *fixture.started {
		t.Fatal("OnStarted called after keyed authority refused readiness")
	}
	if !fixture.cr.preserveSessionsShutdown.Load() {
		t.Fatal("keyed authority refusal did not select preserve-sessions shutdown")
	}
	if !fixture.provider.IsRunning(fixture.sessionName) {
		t.Fatalf("preexisting session stopped after keyed authority refused readiness; calls=%+v", fixture.provider.SnapshotCalls())
	}
	if got := fixture.provider.CountCalls("Interrupt", fixture.sessionName); got != 0 {
		t.Fatalf("preexisting session interrupt calls = %d, want 0", got)
	}
	if got := fixture.provider.CountCalls("Stop", fixture.sessionName); got != 0 {
		t.Fatalf("preexisting session stop calls = %d, want 0", got)
	}
	got, err := fixture.store.Get(fixture.sessionBeadID)
	if err != nil {
		t.Fatalf("get preexisting session bead: %v", err)
	}
	if reason := got.Metadata["sleep_reason"]; reason == string(session.SleepReasonCityStop) {
		t.Fatalf("preexisting session sleep_reason = %q after readiness refusal, want no city-stop mutation", reason)
	}
}

func TestCityRuntimeKeyedReadinessCancellationRetainsNormalShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	fixture := newKeyedReadinessRunFixture(t, func(context.Context) (nudgeCommandSource, error) {
		cancel()
		return nil, retryableNudgeCommandSourceFailure(context.Canceled)
	})

	fixture.cr.run(ctx)

	if *fixture.openCalls != 1 {
		t.Fatalf("keyed command source open calls = %d, want 1; stderr:\n%s", *fixture.openCalls, fixture.stderr.String())
	}
	if *fixture.started {
		t.Fatal("OnStarted called after keyed startup cancellation")
	}
	if fixture.cr.preserveSessionsShutdown.Load() {
		t.Fatal("keyed startup cancellation selected preserve-sessions shutdown")
	}
	if fixture.provider.IsRunning(fixture.sessionName) {
		t.Fatal("preexisting session remained live after explicit startup cancellation")
	}
	if got := fixture.provider.CountCalls("Stop", fixture.sessionName); got == 0 {
		t.Fatal("preexisting session received no stop call after cancellation")
	}
	got, err := fixture.store.Get(fixture.sessionBeadID)
	if err != nil {
		t.Fatalf("get canceled session bead: %v", err)
	}
	if reason := got.Metadata["sleep_reason"]; reason != string(session.SleepReasonCityStop) {
		t.Fatalf("canceled session sleep_reason = %q, want %q", reason, session.SleepReasonCityStop)
	}
}

type keyedReadinessRunFixture struct {
	cr            *CityRuntime
	provider      *runtime.Fake
	store         beads.Store
	sessionBeadID string
	sessionName   string
	started       *bool
	openCalls     *int
	stderr        *bytes.Buffer
}

func newKeyedReadinessRunFixture(
	t *testing.T,
	open func(context.Context) (nudgeCommandSource, error),
) keyedReadinessRunFixture {
	t.Helper()
	const sessionName = "startup-fence-city--worker-1"

	store := beads.NewMemStore()
	sessionBead, err := store.Create(beads.Bead{
		Title:  "Session: worker-1",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"session_name":  sessionName,
			"template":      "worker-1",
			"instance_name": "worker-1",
			"state":         "active",
		},
	})
	if err != nil {
		t.Fatalf("create preexisting session bead: %v", err)
	}

	provider := runtime.NewFake()
	if err := provider.Start(t.Context(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("start preexisting session: %v", err)
	}
	if err := provider.SetMeta(sessionName, "GC_SESSION_ID", sessionBead.ID); err != nil {
		t.Fatalf("bind preexisting session to bead: %v", err)
	}

	started := new(bool)
	openCalls := new(int)
	stderr := new(bytes.Buffer)
	cityPath := t.TempDir()
	cr := &CityRuntime{
		cityPath: cityPath,
		cityName: "startup-fence-city",
		cfg:      supervisorCfg(),
		sp:       provider,
		buildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{
				sessionName: {
					Command:      "test-agent",
					SessionName:  sessionName,
					TemplateName: "worker-1",
					InstanceName: "worker-1",
				},
			}}
		},
		rec:                  events.Discard,
		standaloneCityStore:  store,
		onStarted:            func() { *started = true },
		stdout:               io.Discard,
		stderr:               stderr,
		nudgeEffectOwnership: nudgeEffectOwnershipKeyed,
		nudgeClaimAuthorizer: allowingNudgeEffectAuthorizer{},
		nudgeCommandSourceOpener: func(ctx context.Context, _ string, _ beads.Store, _ nudgequeue.TrustedCityPartition, _ nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			*openCalls++
			return open(ctx)
		},
	}
	cr.setControllerState(&controllerState{
		cfg:           cr.cfg,
		sp:            provider,
		beadStores:    map[string]beads.Store{},
		cityBeadStore: store,
		cityName:      cr.cityName,
		cityPath:      cityPath,
	})
	return keyedReadinessRunFixture{
		cr:            cr,
		provider:      provider,
		store:         store,
		sessionBeadID: sessionBead.ID,
		sessionName:   sessionName,
		started:       started,
		openCalls:     openCalls,
		stderr:        stderr,
	}
}
