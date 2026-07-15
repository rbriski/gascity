package main

import (
	"context"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/reconcilekey"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestNudgeEffectOwnershipLegacyIsTheZeroDefault(t *testing.T) {
	var zero nudgeEffectOwnership
	if zero != nudgeEffectOwnershipLegacy {
		t.Fatalf("zero nudge effect ownership = %d, want legacy %d", zero, nudgeEffectOwnershipLegacy)
	}
	if nudgeEffectOwnershipKeyed == nudgeEffectOwnershipLegacy {
		t.Fatal("keyed ownership must be an explicit value distinct from legacy")
	}
}

func TestLegacyNudgeOwnershipKeepsKeyedCallbackReadOnlyAndDispatcherEnabled(t *testing.T) {
	command := immediateNudgeEffectCommand(time.Now().UTC())
	source := newMutexNudgeEffectSource(command)
	store := newNudgeOwnershipReadCountingStore(nil)
	provider := runtime.NewFake()
	cr := newNudgeOwnershipBridgeRuntime(
		t,
		nudgeEffectOwnershipLegacy,
		source,
		store,
		provider,
		allowingNudgeEffectAuthorizer{},
	)

	if err := cr.installNudgeKeyShadow(t.Context()); err != nil {
		t.Fatalf("installNudgeKeyShadow: %v", err)
	}
	controller, reader, scope, _ := cr.nudgeKeyShadowState()
	if controller == nil || reader == nil || scope == "" {
		t.Fatalf("legacy read shadow publication = controller:%v reader:%v scope:%q, want complete read-only shadow", controller != nil, reader != nil, scope)
	}
	key := mustNudgeOwnershipBridgeKey(t, scope, command.Target.SessionID)
	outcome := controller.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if err := outcome.validate(); err != nil {
		t.Fatalf("read-only callback outcome: %v", err)
	}
	if got := source.claimCallCount(); got != 0 {
		t.Fatalf("legacy-owned keyed callback claims = %d, want 0", got)
	}
	if got := provider.CountCalls("NudgeEffect", "city--worker-1"); got != 0 {
		t.Fatalf("legacy-owned keyed callback provider entries = %d, want 0", got)
	}

	readsBefore := store.reads
	cr.nudgeDispatchTick(t.Context())
	if store.reads <= readsBefore {
		t.Fatalf("legacy dispatcher store reads = %d, want path enabled after %d", store.reads, readsBefore)
	}
	if got := source.claimCallCount(); got != 0 {
		t.Fatalf("legacy dispatcher crossed into keyed claims = %d, want 0", got)
	}
}

func TestKeyedNudgeOwnershipDisablesLegacyDispatchBeforeInstallation(t *testing.T) {
	store := newNudgeOwnershipReadCountingStore(nil)
	provider := runtime.NewFake()
	cr := newNudgeOwnershipBridgeRuntime(
		t,
		nudgeEffectOwnershipKeyed,
		nil,
		store,
		provider,
		allowingNudgeEffectAuthorizer{},
	)

	cr.nudgeDispatchTick(t.Context())
	if store.reads != 0 {
		t.Fatalf("keyed ownership touched legacy nudge/session stores %d time(s) before installation", store.reads)
	}
	if got := provider.CountCalls("Nudge", "city--worker-1"); got != 0 {
		t.Fatalf("keyed ownership entered legacy provider %d time(s) before installation", got)
	}
}

func TestKeyedNudgeOwnershipPublishesCompleteEffectOwnerBeforeFirstClaim(t *testing.T) {
	now := time.Now().UTC()
	command := immediateNudgeEffectCommand(now)
	baseSource := newMutexNudgeEffectSource(command)
	store := newNudgeOwnershipReadCountingStore(nudgeOwnershipSessionStore(command))
	provider := runtime.NewFake()
	if err := provider.Start(t.Context(), "city--worker-1", runtime.Config{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if err := provider.SetMeta("city--worker-1", "GC_INSTANCE_TOKEN", command.Target.LaunchIdentity); err != nil {
		t.Fatalf("SetMeta launch identity: %v", err)
	}
	provider.NudgeEffectResults["city--worker-1"] = runtime.NudgeEffectResult{
		Stage:                runtime.NudgeEffectStageAccepted,
		Completion:           runtime.NudgeEffectCompletionCompleted,
		ConsumptionConfirmed: true,
	}
	source := &publicationCheckingNudgeEffectSource{mutexNudgeEffectSource: baseSource}
	cr := newNudgeOwnershipBridgeRuntime(
		t,
		nudgeEffectOwnershipKeyed,
		source,
		store,
		provider,
		allowingNudgeEffectAuthorizer{},
	)
	source.beforeFirstSnapshot = func() {
		controller, reader, scope, _ := cr.nudgeKeyShadowState()
		if controller != nil || reader != nil || scope != "" {
			t.Fatalf("partial publication during startup snapshot: controller=%v reader=%v scope=%q", controller != nil, reader != nil, scope)
		}
		if got := baseSource.claimCallCount(); got != 0 {
			t.Fatalf("claims during startup publication = %d, want 0", got)
		}
	}
	source.beforeClaim = func() {
		controller, reader, scope, _ := cr.nudgeKeyShadowState()
		if controller == nil || reader == nil || scope == "" {
			t.Fatalf("claim observed partial publication: controller=%v reader=%v scope=%q", controller != nil, reader != nil, scope)
		}
	}

	if err := cr.installNudgeKeyShadow(t.Context()); err != nil {
		t.Fatalf("installNudgeKeyShadow: %v", err)
	}
	controller, reader, scope, _ := cr.nudgeKeyShadowState()
	if controller == nil || reader == nil || scope == "" {
		t.Fatalf("keyed publication = controller:%v reader:%v scope:%q, want all fields", controller != nil, reader != nil, scope)
	}
	key := mustNudgeOwnershipBridgeKey(t, scope, command.Target.SessionID)
	outcome := controller.reconcile(t.Context(), key, nudgeReconcileBatch{Causes: nudgeCauseCommandCommit})
	if err := outcome.validate(); err != nil {
		t.Fatalf("effect-owner callback outcome: %v", err)
	}
	if outcome.disposition == nudgeReconcileOutcomeInvariant {
		t.Fatalf("effect-owner callback invariant failure: %v", outcome.err)
	}
	if got := baseSource.claimCallCount(); got != 1 {
		t.Fatalf("claims after complete publication = %d, want 1", got)
	}
	if got := provider.CountCalls("NudgeEffect", "city--worker-1"); got != 1 {
		t.Fatalf("classified provider entries = %d, want 1", got)
	}
	if got := provider.CountCalls("Nudge", "city--worker-1"); got != 0 {
		t.Fatalf("legacy provider entries = %d, want 0", got)
	}
}

func TestFailedKeyedNudgeInstallationPublishesNothingAndDoesNotFallBack(t *testing.T) {
	command := immediateNudgeEffectCommand(time.Now().UTC())
	readOnlySource := &fakeNudgeCommandSource{snapshot: nudgequeue.CommandIndexSnapshot{
		Store:             command.Store,
		Entries:           []nudgequeue.CommandIndexEntry{knownPageEntry(command)},
		Revision:          command.Order.Revision,
		SequenceHighWater: command.Order.Sequence,
	}}
	store := newNudgeOwnershipReadCountingStore(nil)
	provider := runtime.NewFake()
	cr := newNudgeOwnershipBridgeRuntime(
		t,
		nudgeEffectOwnershipKeyed,
		readOnlySource,
		store,
		provider,
		allowingNudgeEffectAuthorizer{},
	)

	if err := cr.installNudgeKeyShadow(t.Context()); err == nil {
		t.Fatal("effect-incapable keyed source installation error = nil, want fail-closed refusal")
	}
	controller, reader, scope, _ := cr.nudgeKeyShadowState()
	if controller != nil || reader != nil || scope != "" {
		t.Fatalf("failed keyed install published controller=%v reader=%v scope=%q", controller != nil, reader != nil, scope)
	}
	if got := readOnlySource.snapshotCallCount(); got != 0 {
		t.Fatalf("effect-incapable source snapshot calls = %d, want rejection before reader construction", got)
	}
	cr.nudgeDispatchTick(t.Context())
	if store.reads != 0 {
		t.Fatalf("failed keyed install fell back to legacy store path with %d read(s)", store.reads)
	}
	if got := provider.CountCalls("Nudge", "city--worker-1"); got != 0 {
		t.Fatalf("failed keyed install entered legacy provider %d time(s)", got)
	}
	if got := provider.CountCalls("NudgeEffect", "city--worker-1"); got != 0 {
		t.Fatalf("failed keyed install entered classified provider %d time(s)", got)
	}
}

func TestKeyedNudgeInstallationWithoutAuthorizerPublishesNothing(t *testing.T) {
	command := immediateNudgeEffectCommand(time.Now().UTC())
	source := newMutexNudgeEffectSource(command)
	store := newNudgeOwnershipReadCountingStore(nil)
	provider := runtime.NewFake()
	cr := newNudgeOwnershipBridgeRuntime(
		t,
		nudgeEffectOwnershipKeyed,
		source,
		store,
		provider,
		nil,
	)

	if err := cr.installNudgeKeyShadow(t.Context()); err == nil {
		t.Fatal("keyed installation without claim authorizer error = nil, want fail-closed refusal")
	}
	controller, reader, scope, _ := cr.nudgeKeyShadowState()
	if controller != nil || reader != nil || scope != "" {
		t.Fatalf("authorizer failure published controller=%v reader=%v scope=%q", controller != nil, reader != nil, scope)
	}
	if got := source.claimCallCount(); got != 0 {
		t.Fatalf("authorizer failure claims = %d, want 0", got)
	}
	cr.nudgeDispatchTick(t.Context())
	if store.reads != 0 {
		t.Fatalf("authorizer failure fell back to legacy store path with %d read(s)", store.reads)
	}
	if got := provider.CountCalls("NudgeEffect", "city--worker-1"); got != 0 {
		t.Fatalf("authorizer failure provider entries = %d, want 0", got)
	}
}

func TestNewCityRuntimeRestoresLegacyOwnershipByDefaultAfterKeyedRuntime(t *testing.T) {
	stubManagedDoltStoreOpeners(t)
	authorizer := &nudgeOwnershipBridgeAuthorizer{}
	first := newConstructedNudgeOwnershipRuntime(t, CityRuntimeParams{
		NudgeEffectOwnership: nudgeEffectOwnershipKeyed,
		NudgeClaimAuthorizer: authorizer,
	})
	if first.nudgeEffectOwnership != nudgeEffectOwnershipKeyed || first.nudgeClaimAuthorizer != authorizer {
		t.Fatalf("first runtime ownership/authorizer = %d/%T, want explicit keyed/%T", first.nudgeEffectOwnership, first.nudgeClaimAuthorizer, authorizer)
	}

	second := newConstructedNudgeOwnershipRuntime(t, CityRuntimeParams{})
	if second.nudgeEffectOwnership != nudgeEffectOwnershipLegacy {
		t.Fatalf("restarted default ownership = %d, want legacy", second.nudgeEffectOwnership)
	}
	if second.nudgeClaimAuthorizer != nil {
		t.Fatalf("restarted default authorizer = %T, want nil", second.nudgeClaimAuthorizer)
	}
	if second.nudgeKeyController != nil || second.nudgeKeyReader != nil || second.nudgeKeyShadowScope != "" {
		t.Fatalf("restarted runtime inherited keyed publication: controller=%v reader=%v scope=%q", second.nudgeKeyController != nil, second.nudgeKeyReader != nil, second.nudgeKeyShadowScope)
	}
	store := newNudgeOwnershipReadCountingStore(nil)
	second.standaloneCityStore = store
	second.nudgeDispatchTick(t.Context())
	if store.reads == 0 {
		t.Fatal("restarted default runtime did not restore the legacy dispatch path")
	}
}

func newNudgeOwnershipBridgeRuntime(
	t *testing.T,
	ownership nudgeEffectOwnership,
	source nudgeCommandSource,
	store beads.Store,
	provider runtime.Provider,
	authorizer nudgequeue.NudgeClaimAuthorizer,
) *CityRuntime {
	t.Helper()
	return &CityRuntime{
		cityPath:             t.TempDir(),
		cityName:             "bridge-city",
		cfg:                  supervisorCfg(),
		sp:                   provider,
		rec:                  events.Discard,
		standaloneCityStore:  store,
		stderr:               io.Discard,
		nudgeEffectOwnership: ownership,
		nudgeClaimAuthorizer: authorizer,
		nudgeCommandSourceOpener: func(context.Context, string, beads.Store, nudgequeue.TrustedCityPartition, nudgequeue.TrustedCityPartitionResolver) (nudgeCommandSource, error) {
			return source, nil
		},
	}
}

func newConstructedNudgeOwnershipRuntime(t *testing.T, ownership CityRuntimeParams) *CityRuntime {
	t.Helper()
	cityPath := t.TempDir()
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	cfg.Daemon.NudgeDispatcher = "supervisor"
	provider := runtime.NewFake()
	return newCityRuntime(CityRuntimeParams{
		CityPath: cityPath,
		CityName: "bridge-city",
		TomlPath: tomlPath,
		Cfg:      cfg,
		SP:       provider,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:                 newDrainOps(provider),
		Rec:                  events.Discard,
		NudgeEffectOwnership: ownership.NudgeEffectOwnership,
		NudgeClaimAuthorizer: ownership.NudgeClaimAuthorizer,
		Stdout:               io.Discard,
		Stderr:               io.Discard,
	})
}

func nudgeOwnershipSessionStore(command nudgequeue.Command) beads.Store {
	return beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     command.Target.SessionID,
		Title:  "Session: worker-1",
		Type:   session.BeadType,
		Status: "open",
		Labels: []string{session.LabelSession},
		Metadata: map[string]string{
			"generation":     "7",
			"session_key":    "continuation-1",
			"instance_token": command.Target.LaunchIdentity,
			"session_name":   "city--worker-1",
			"provider":       "fake-provider",
			"transport":      "tmux-cli",
		},
	}}, nil)
}

func mustNudgeOwnershipBridgeKey(t *testing.T, scope, sessionID string) reconcilekey.Session {
	t.Helper()
	key, err := reconcilekey.NewSession(scope, sessionID)
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}
	return key
}

type nudgeOwnershipReadCountingStore struct {
	beads.Store
	reads int
}

func newNudgeOwnershipReadCountingStore(delegate beads.Store) *nudgeOwnershipReadCountingStore {
	if delegate == nil {
		delegate = beads.NewMemStore()
	}
	return &nudgeOwnershipReadCountingStore{Store: delegate}
}

func (s *nudgeOwnershipReadCountingStore) Get(id string) (beads.Bead, error) {
	s.reads++
	return s.Store.Get(id)
}

func (s *nudgeOwnershipReadCountingStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.reads++
	return s.Store.List(query)
}

type publicationCheckingNudgeEffectSource struct {
	*mutexNudgeEffectSource
	beforeFirstSnapshot func()
	beforeClaim         func()
	snapshots           int
}

func (s *publicationCheckingNudgeEffectSource) Snapshot(ctx context.Context, limit int) (nudgequeue.CommandIndexSnapshot, error) {
	s.snapshots++
	if s.snapshots == 1 && s.beforeFirstSnapshot != nil {
		s.beforeFirstSnapshot()
	}
	return s.mutexNudgeEffectSource.Snapshot(ctx, limit)
}

func (s *publicationCheckingNudgeEffectSource) ClaimAuthorized(ctx context.Context, request nudgeEffectClaimRequest, authorizer nudgequeue.NudgeClaimAuthorizer) (nudgequeue.CommandClaimResult, error) {
	if s.beforeClaim != nil {
		s.beforeClaim()
	}
	return s.mutexNudgeEffectSource.ClaimAuthorized(ctx, request, authorizer)
}

type nudgeOwnershipBridgeAuthorizer struct{}

func (*nudgeOwnershipBridgeAuthorizer) AuthorizeNudgeClaim(_ context.Context, request nudgequeue.NudgeClaimAuthorizationRequest) (nudgequeue.NudgeClaimAuthorization, error) {
	return nudgequeue.NudgeClaimAuthorization{
		Disposition:            nudgequeue.NudgeAuthorizationAllowed,
		PrincipalSchemaVersion: nudgequeue.NudgePrincipalSchemaVersion,
		DecisionID:             "bridge-decision",
		PolicyVersion:          "bridge-policy",
		Reference:              request.Command.TrustedIngress,
	}, nil
}

var (
	_ nudgeCommandEffectSource        = (*publicationCheckingNudgeEffectSource)(nil)
	_ nudgequeue.NudgeClaimAuthorizer = (*nudgeOwnershipBridgeAuthorizer)(nil)
)
