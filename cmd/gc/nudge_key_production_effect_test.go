package main

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/worker"
)

func TestProductionNudgeEffectTargetsMapsExactPersistedIdentity(t *testing.T) {
	store := &nudgeSessionTargetStoreFake{info: session.Info{
		ID:            "session-7",
		Generation:    "42",
		SessionKey:    "continuation-7",
		InstanceToken: "launch-7",
		SessionName:   "city--worker-7",
		Provider:      "fake-provider",
		Transport:     "tmux-cli",
		Closed:        true,
	}}
	targets := &productionNudgeEffectTargets{store: store}

	got, err := targets.Read(t.Context(), "session-7")
	if err != nil {
		t.Fatalf("Read: %v", err)
	}
	want := nudgeEffectTarget{
		sessionID:            "session-7",
		sessionName:          "city--worker-7",
		intentGeneration:     42,
		continuationIdentity: "continuation-7",
		launchIdentity:       "launch-7",
		provider:             "fake-provider",
		transport:            "tmux-cli",
		closed:               true,
	}
	if got != want {
		t.Fatalf("Read target = %#v, want exact persisted projection %#v", got, want)
	}
	if len(store.calls) != 1 || store.calls[0] != "session-7" {
		t.Fatalf("store Get calls = %v, want [session-7]", store.calls)
	}
}

func TestProductionNudgeEffectTargetsRejectsNonCanonicalGeneration(t *testing.T) {
	tests := []struct {
		name       string
		generation string
	}{
		{name: "missing", generation: ""},
		{name: "zero", generation: "0"},
		{name: "whitespace", generation: " 7 "},
		{name: "positive sign", generation: "+7"},
		{name: "negative sign", generation: "-7"},
		{name: "leading zero", generation: "07"},
		{name: "overflow", generation: "18446744073709551616"},
		{name: "nonnumeric", generation: "seven"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := validProductionNudgeTargetInfo()
			info.Generation = test.generation
			targets := &productionNudgeEffectTargets{store: &nudgeSessionTargetStoreFake{info: info}}

			got, err := targets.Read(t.Context(), info.ID)
			assertProductionNudgeTargetReadFailedClosed(t, got, err)
		})
	}
}

func TestProductionNudgeEffectTargetsRejectsIncompleteOrMismatchedIdentity(t *testing.T) {
	tests := []struct {
		name      string
		requested string
		mutate    func(*session.Info)
	}{
		{
			name:      "persisted session id missing",
			requested: "session-7",
			mutate:    func(info *session.Info) { info.ID = "" },
		},
		{
			name:      "persisted session id mismatches request",
			requested: "session-other",
			mutate:    func(*session.Info) {},
		},
		{
			name:      "continuation identity missing",
			requested: "session-7",
			mutate:    func(info *session.Info) { info.SessionKey = "" },
		},
		{
			name:      "launch identity missing",
			requested: "session-7",
			mutate:    func(info *session.Info) { info.InstanceToken = "" },
		},
		{
			name:      "session name missing",
			requested: "session-7",
			mutate:    func(info *session.Info) { info.SessionName = "" },
		},
		{
			name:      "provider missing",
			requested: "session-7",
			mutate:    func(info *session.Info) { info.Provider = "" },
		},
		{
			name:      "transport missing",
			requested: "session-7",
			mutate:    func(info *session.Info) { info.Transport = "" },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			info := validProductionNudgeTargetInfo()
			test.mutate(&info)
			targets := &productionNudgeEffectTargets{store: &nudgeSessionTargetStoreFake{info: info}}

			got, err := targets.Read(t.Context(), test.requested)
			assertProductionNudgeTargetReadFailedClosed(t, got, err)
		})
	}
}

func TestProductionNudgeEffectTargetsHonorsCancellationAroundStoreRead(t *testing.T) {
	t.Run("before read", func(t *testing.T) {
		store := &nudgeSessionTargetStoreFake{info: validProductionNudgeTargetInfo()}
		targets := &productionNudgeEffectTargets{store: store}
		ctx, cancel := context.WithCancel(t.Context())
		cancel()

		got, err := targets.Read(ctx, "session-7")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read error = %v, want context.Canceled", err)
		}
		if got != (nudgeEffectTarget{}) {
			t.Fatalf("canceled Read target = %#v, want zero", got)
		}
		if len(store.calls) != 0 {
			t.Fatalf("canceled Read store calls = %v, want none", store.calls)
		}
	})

	t.Run("during read", func(t *testing.T) {
		ctx, cancel := context.WithCancel(t.Context())
		store := &nudgeSessionTargetStoreFake{
			info:  validProductionNudgeTargetInfo(),
			onGet: cancel,
		}
		targets := &productionNudgeEffectTargets{store: store}

		got, err := targets.Read(ctx, "session-7")
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("Read error = %v, want context.Canceled after store return", err)
		}
		if got != (nudgeEffectTarget{}) {
			t.Fatalf("canceled Read target = %#v, want zero", got)
		}
		if len(store.calls) != 1 {
			t.Fatalf("store calls = %v, want the one in-flight Get", store.calls)
		}
	})
}

func TestProductionNudgeEffectTargetsSurfacesStoreErrorWithoutIdentity(t *testing.T) {
	storeErr := errors.New("session store unavailable")
	store := &nudgeSessionTargetStoreFake{err: storeErr}
	targets := &productionNudgeEffectTargets{store: store}

	got, err := targets.Read(t.Context(), "session-7")
	if !errors.Is(err, storeErr) {
		t.Fatalf("Read error = %v, want wrapped store error", err)
	}
	if got != (nudgeEffectTarget{}) {
		t.Fatalf("failed Read target = %#v, want zero", got)
	}
}

func TestProductionNudgeEffectHandlesUsesRuntimeHandleClassifiedEntry(t *testing.T) {
	const (
		sessionName = "city--worker-7"
		launchID    = "launch-7"
		operationID = "command-7"
	)
	provider := runtime.NewFake()
	if err := provider.Start(t.Context(), sessionName, runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := provider.SetMeta(sessionName, "GC_INSTANCE_TOKEN", launchID); err != nil {
		t.Fatalf("SetMeta launch identity: %v", err)
	}
	provider.NudgeEffectResults[sessionName] = runtime.NudgeEffectResult{
		Stage:                runtime.NudgeEffectStageAccepted,
		Completion:           runtime.NudgeEffectCompletionCompleted,
		ConsumptionConfirmed: true,
	}
	spy := &recordingProductionNudgeEffectProvider{
		Provider: provider,
		effects:  provider,
	}
	recorder := &productionNudgeEffectRecorder{}
	factory := &productionNudgeEffectHandles{provider: spy, recorder: recorder}
	target := validProductionNudgeEffectTarget()

	handle, err := factory.Handle(target)
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if _, ok := handle.(*worker.RuntimeHandle); !ok {
		t.Fatalf("Handle type = %T, want *worker.RuntimeHandle from worker.NewRuntimeHandle", handle)
	}
	result, err := handle.Nudge(t.Context(), worker.NudgeRequest{
		Text:     "inspect the failed build",
		Delivery: worker.NudgeDeliveryImmediate,
		Wake:     worker.NudgeWakeLiveOnly,
		Effect: &runtime.NudgeEffectContract{
			OperationID:            operationID,
			ExpectedLaunchIdentity: launchID,
			InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
		},
	})
	if err != nil {
		t.Fatalf("Nudge: %v", err)
	}
	if !result.Delivered || result.Effect == nil || *result.Effect != provider.NudgeEffectResults[sessionName] {
		t.Fatalf("Nudge result = %#v, want exact accepted provider evidence", result)
	}
	if got := provider.CountCalls("NudgeEffect", sessionName); got != 1 {
		t.Fatalf("NudgeEffect entries = %d, want 1", got)
	}
	if got := provider.CountCalls("Nudge", sessionName); got != 0 {
		t.Fatalf("legacy Nudge entries = %d, want 0", got)
	}
	if len(spy.calls) != 1 {
		t.Fatalf("classified provider requests = %d, want 1", len(spy.calls))
	}
	call := spy.calls[0]
	if call.name != sessionName || call.request.Contract.OperationID != operationID ||
		call.request.Contract.ExpectedLaunchIdentity != launchID ||
		call.request.Contract.InteractionPolicy != runtime.NudgeInteractionRequireUnattachedNormal ||
		runtime.FlattenText(call.request.Content) != "inspect the failed build" {
		t.Fatalf("classified provider request = %#v, want exact operation/launch/interaction/content", call)
	}

	payload := recorder.singleOperation(t)
	if payload.SessionName != sessionName || payload.Provider != target.provider || payload.Transport != target.transport {
		t.Fatalf("worker operation identity = %#v, want target session/provider/transport", payload)
	}
}

func TestProductionNudgeEffectHandlesResolvesCurrentProviderForEachEffect(t *testing.T) {
	const sessionName = "city--worker-7"
	first := runtime.NewFake()
	second := runtime.NewFake()
	for _, provider := range []*runtime.Fake{first, second} {
		if err := provider.Start(t.Context(), sessionName, runtime.Config{}); err != nil {
			t.Fatalf("Start: %v", err)
		}
		if err := provider.SetMeta(sessionName, "GC_INSTANCE_TOKEN", "launch-7"); err != nil {
			t.Fatalf("SetMeta launch identity: %v", err)
		}
		provider.NudgeEffectResults[sessionName] = runtime.NudgeEffectResult{
			Stage:      runtime.NudgeEffectStageAccepted,
			Completion: runtime.NudgeEffectCompletionCompleted,
		}
	}
	selected := runtime.Provider(first)
	factory := &productionNudgeEffectHandles{
		currentProvider: func() runtime.Provider { return selected },
		recorder:        events.Discard,
	}
	target := validProductionNudgeEffectTarget()
	nudge := func(operationID string) {
		handle, err := factory.Handle(target)
		if err != nil {
			t.Fatalf("Handle: %v", err)
		}
		if _, err := handle.Nudge(t.Context(), worker.NudgeRequest{
			Text:     "continue",
			Delivery: worker.NudgeDeliveryImmediate,
			Wake:     worker.NudgeWakeLiveOnly,
			Effect: &runtime.NudgeEffectContract{
				OperationID:            operationID,
				ExpectedLaunchIdentity: target.launchIdentity,
				InteractionPolicy:      runtime.NudgeInteractionRequireUnattachedNormal,
			},
		}); err != nil {
			t.Fatalf("Nudge: %v", err)
		}
	}

	nudge("command-before-swap")
	selected = second
	nudge("command-after-swap")

	if got := first.CountCalls("NudgeEffect", sessionName); got != 1 {
		t.Fatalf("retired provider entries = %d, want only the pre-swap effect", got)
	}
	if got := second.CountCalls("NudgeEffect", sessionName); got != 1 {
		t.Fatalf("current provider entries = %d, want the post-swap effect", got)
	}
}

func TestProductionNudgeEffectHandlesRejectsIncompleteDependenciesAndTarget(t *testing.T) {
	tests := []struct {
		name            string
		withoutProvider bool
		mutate          func(*nudgeEffectTarget)
	}{
		{name: "provider dependency missing", withoutProvider: true, mutate: func(*nudgeEffectTarget) {}},
		{name: "target missing", mutate: func(target *nudgeEffectTarget) { *target = nudgeEffectTarget{} }},
		{name: "session id missing", mutate: func(target *nudgeEffectTarget) { target.sessionID = "" }},
		{name: "session name missing", mutate: func(target *nudgeEffectTarget) { target.sessionName = "" }},
		{name: "generation missing", mutate: func(target *nudgeEffectTarget) { target.intentGeneration = 0 }},
		{name: "continuation identity missing", mutate: func(target *nudgeEffectTarget) { target.continuationIdentity = "" }},
		{name: "launch identity missing", mutate: func(target *nudgeEffectTarget) { target.launchIdentity = "" }},
		{name: "target provider missing", mutate: func(target *nudgeEffectTarget) { target.provider = "" }},
		{name: "target transport missing", mutate: func(target *nudgeEffectTarget) { target.transport = "" }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			provider := runtime.NewFake()
			if err := provider.Start(t.Context(), "city--worker-7", runtime.Config{}); err != nil {
				t.Fatalf("Start: %v", err)
			}
			baseline := len(provider.SnapshotCalls())
			var dependency runtime.Provider = provider
			if test.withoutProvider {
				dependency = nil
			}
			factory := &productionNudgeEffectHandles{provider: dependency, recorder: events.Discard}
			target := validProductionNudgeEffectTarget()
			test.mutate(&target)

			handle, err := factory.Handle(target)
			if err == nil || handle != nil {
				t.Fatalf("Handle = %T, err=%v; want fail-closed construction", handle, err)
			}
			if calls := provider.SnapshotCalls(); len(calls) != baseline {
				t.Fatalf("provider calls after rejected target = %#v, want no calls beyond setup", calls[baseline:])
			}
		})
	}
}

func validProductionNudgeTargetInfo() session.Info {
	return session.Info{
		ID:            "session-7",
		Generation:    "7",
		SessionKey:    "continuation-7",
		InstanceToken: "launch-7",
		SessionName:   "city--worker-7",
		Provider:      "fake-provider",
		Transport:     "tmux-cli",
	}
}

func validProductionNudgeEffectTarget() nudgeEffectTarget {
	return nudgeEffectTarget{
		sessionID:            "session-7",
		sessionName:          "city--worker-7",
		intentGeneration:     7,
		continuationIdentity: "continuation-7",
		launchIdentity:       "launch-7",
		provider:             "fake-provider",
		transport:            "tmux-cli",
	}
}

func assertProductionNudgeTargetReadFailedClosed(t *testing.T, got nudgeEffectTarget, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("Read error = nil, want fail-closed target rejection")
	}
	if got != (nudgeEffectTarget{}) {
		t.Fatalf("rejected Read target = %#v, want zero", got)
	}
}

type nudgeSessionTargetStoreFake struct {
	info  session.Info
	err   error
	calls []string
	onGet func()
}

func (s *nudgeSessionTargetStoreFake) Get(id string) (session.Info, error) {
	s.calls = append(s.calls, id)
	if s.onGet != nil {
		s.onGet()
	}
	return s.info, s.err
}

type productionNudgeEffectProviderCall struct {
	name    string
	request runtime.NudgeEffectRequest
}

type recordingProductionNudgeEffectProvider struct {
	runtime.Provider
	effects runtime.NudgeEffectProvider
	calls   []productionNudgeEffectProviderCall
}

func (p *recordingProductionNudgeEffectProvider) NudgeEffect(ctx context.Context, name string, request runtime.NudgeEffectRequest) (runtime.NudgeEffectResult, error) {
	p.calls = append(p.calls, productionNudgeEffectProviderCall{name: name, request: request})
	return p.effects.NudgeEffect(ctx, name, request)
}

type productionNudgeEffectRecorder struct {
	events []events.Event
}

func (r *productionNudgeEffectRecorder) Record(event events.Event) {
	r.events = append(r.events, event)
}

func (r *productionNudgeEffectRecorder) singleOperation(t *testing.T) productionNudgeEffectOperationPayload {
	t.Helper()
	if len(r.events) != 1 || r.events[0].Type != events.WorkerOperation {
		t.Fatalf("recorded events = %#v, want one worker operation", r.events)
	}
	var payload productionNudgeEffectOperationPayload
	if err := json.Unmarshal(r.events[0].Payload, &payload); err != nil {
		t.Fatalf("decode worker operation: %v", err)
	}
	return payload
}

type productionNudgeEffectOperationPayload struct {
	SessionName string `json:"session_name"`
	Provider    string `json:"provider"`
	Transport   string `json:"transport"`
}

var (
	_ nudgeSessionTargetStore     = (*nudgeSessionTargetStoreFake)(nil)
	_ runtime.Provider            = (*recordingProductionNudgeEffectProvider)(nil)
	_ runtime.NudgeEffectProvider = (*recordingProductionNudgeEffectProvider)(nil)
	_ events.Recorder             = (*productionNudgeEffectRecorder)(nil)
)
