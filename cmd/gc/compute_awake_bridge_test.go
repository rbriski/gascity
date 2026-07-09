package main

import (
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
)

func TestBuildAwakeInputFromReconcilerUsesLifecycleProjectionForCompatibilityStates(t *testing.T) {
	now := time.Now().UTC()
	input := buildAwakeInputFromReconciler(
		&config.City{},
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(beads.Bead{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":        "stopped",
				"session_name": "s-worker",
				"template":     "worker",
			},
		})},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if got := input.SessionBeads[0].State; got != "asleep" {
		t.Fatalf("State = %q, want asleep-compatible projection for stopped", got)
	}
}

// TestBuildAwakeInputFromReconcilerReadsInfoSnapshot pins that the scan projects
// the typed session.Info it is handed rather than re-deriving any field: it sets a
// SleepReason on the Info that no raw bead projection would carry and asserts that
// value survives into the AwakeSessionBead.
func TestBuildAwakeInputFromReconcilerReadsInfoSnapshot(t *testing.T) {
	now := time.Now().UTC()
	b := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "s-worker",
			"template":     "worker",
			"sleep_reason": "from-bead",
		},
	}
	info := session.InfoFromPersistedBead(b)
	info.SleepReason = "from-snapshot"

	input := buildAwakeInputFromReconciler(
		&config.City{}, "", []session.Info{info},
		nil, nil, nil, nil, nil, nil, nil, nil, nil, now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if got := input.SessionBeads[0].SleepReason; got != "from-snapshot" {
		t.Fatalf("SleepReason = %q, want from-snapshot (scan must read the Info snapshot, not re-derive the raw bead)", got)
	}
}

// TestBuildAwakeInputFromReconcilerCanonicalizesLegacyBoundTemplate pins the
// bridge-side identity normalization for adopted legacy-bound session beads.
// A bead persisted under a removed binding ("gascity-packs/gc.implementation-worker")
// must enter the awake engine keyed by the current unbound agent's canonical
// template, so explicit wake, suspension gates, and scale/min-active
// accounting all see the adopted session. Without normalization the raw
// stored template misses every agentsByName lookup and the explicit wake
// request lingers unhonored.
func TestBuildAwakeInputFromReconcilerCanonicalizesLegacyBoundTemplate(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "implementation-worker", Dir: "gascity-packs"}},
	}
	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(beads.Bead{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":        "stopped",
				"session_name": "gc__implementation-worker-mc-1",
				"template":     "gascity-packs/gc.implementation-worker",
				"wake_request": "explicit",
			},
		})},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if got := input.SessionBeads[0].Template; got != "gascity-packs/implementation-worker" {
		t.Fatalf("Template = %q, want canonical current template", got)
	}
	if !input.SessionBeads[0].ExplicitWake {
		t.Fatal("ExplicitWake = false, want true for wake_request=explicit")
	}

	decisions := ComputeAwakeSet(input)
	got := decisions["gc__implementation-worker-mc-1"]
	if !got.ShouldWake || got.Reason != "explicit-wake" {
		t.Fatalf("decision = %+v, want explicit-wake for adopted legacy-bound bead", got)
	}
}

// TestBuildAwakeInputFromReconcilerKeepsUnresolvableTemplateRaw guards the
// conservative half of the bridge normalization: a stored template that does
// not resolve to any configured agent must pass through unchanged rather
// than being rewritten or dropped.
func TestBuildAwakeInputFromReconcilerKeepsUnresolvableTemplateRaw(t *testing.T) {
	now := time.Now().UTC()
	input := buildAwakeInputFromReconciler(
		&config.City{Agents: []config.Agent{{Name: "other", Dir: "rig"}}},
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(beads.Bead{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":        "stopped",
				"session_name": "s-orphan",
				"template":     "removed-rig/gone-worker",
			},
		})},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	if got := input.SessionBeads[0].Template; got != "removed-rig/gone-worker" {
		t.Fatalf("Template = %q, want raw stored template preserved", got)
	}
}

func TestBuildAwakeInputFromReconcilerCarriesResetPendingMetadata(t *testing.T) {
	now := time.Now().UTC()
	input := buildAwakeInputFromReconciler(
		&config.City{},
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(beads.Bead{
			ID:     "mc-session-1",
			Status: "open",
			Type:   "session",
			Metadata: map[string]string{
				"state":                      "stopped",
				"session_name":               "s-reset-target",
				"template":                   "build-agent",
				"restart_requested":          "true",
				"continuation_reset_pending": "true",
				session.ResetCommittedAtKey:  now.Format(time.RFC3339),
			},
		})},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	got := input.SessionBeads[0]
	if !got.RestartRequested {
		t.Fatalf("RestartRequested = false, want true")
	}
	if !got.ContinuationResetPending {
		t.Fatalf("ContinuationResetPending = false, want true")
	}
}

func TestBuildAwakeInputFromReconcilerPopulatesPendingInteractions(t *testing.T) {
	now := time.Now().UTC()
	sp := runtime.NewFake()
	sp.SetPendingInteraction("s-worker", &runtime.PendingInteraction{
		RequestID: "req-1",
		Kind:      "question",
		Prompt:    "approve?",
	})
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "s-worker",
			"template":     "worker",
		},
	}

	input := buildAwakeInputFromReconciler(
		&config.City{Agents: []config.Agent{{Name: "worker"}}},
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(sessionBead)},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		[]wakeTarget{{session: &sessionBead, alive: true}},
		sp,
		now,
	)

	if !input.PendingSessions["s-worker"] {
		t.Fatalf("PendingSessions[s-worker] = false, want true")
	}
	decisions := ComputeAwakeSet(input)
	got := decisions["s-worker"]
	if !got.ShouldWake || got.Reason != "pending" {
		t.Fatalf("decision = %+v, want pending wake", got)
	}
}

// TestBuildAwakeInputFromReconciler_BlockedAssignedOpenBeadDoesNotKeepSessionAwake
// pins the reconciler readiness fix: a blocked open assigned bead arrives via
// the open-routed orphan-release pass with readyAssignedFlags[i]=false. It must
// NOT keep its owning session awake — neither via assigned-work nor via
// named-demand — so the session can sleep and the existing resume-on-ShouldWake
// path can later re-wake it once its blocker clears (graph-store hang).
func TestBuildAwakeInputFromReconciler_BlockedAssignedOpenBeadDoesNotKeepSessionAwake(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{Agents: []config.Agent{{Name: "gc.run-operator"}}}
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "gc__run-operator-mc-1",
			"template":     "gc.run-operator",
		},
	}
	blockedWork := beads.Bead{
		ID:       "ga-blocked",
		Status:   "open",
		Assignee: "gc__run-operator-mc-1",
		Metadata: map[string]string{"gc.routed_to": "gc.run-operator"},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"",
		[]session.Info{session.InfoFromPersistedBead(sessionBead)},
		nil,
		nil,
		nil,
		nil,
		[]beads.Bead{blockedWork},
		[]bool{false}, // readyAssignedFlags: blocked bead is NOT ready
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	if len(input.WorkBeads) != 1 {
		t.Fatalf("WorkBeads length = %d, want 1", len(input.WorkBeads))
	}
	if input.WorkBeads[0].Ready {
		t.Fatalf("WorkBeads[0].Ready = true, want false for a blocked open bead")
	}

	decisions := ComputeAwakeSet(input)
	got := decisions["gc__run-operator-mc-1"]
	if got.ShouldWake {
		t.Fatalf("session should sleep for blocked open work; got decision = %+v", got)
	}
}

// TestBuildAwakeInputFromReconciler_ReadyAssignedOpenBeadWakesSession is the
// positive companion: the same open assigned bead admitted via the Ready()/deps
// pass (readyAssignedFlags[i]=true) still wakes/holds its session.
func TestBuildAwakeInputFromReconciler_ReadyAssignedOpenBeadWakesSession(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{Agents: []config.Agent{{Name: "gc.run-operator"}}}
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "gc__run-operator-mc-1",
			"template":     "gc.run-operator",
		},
	}
	readyWork := beads.Bead{
		ID:       "ga-ready",
		Status:   "open",
		Assignee: "gc__run-operator-mc-1",
		Metadata: map[string]string{"gc.routed_to": "gc.run-operator"},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"",
		[]session.Info{session.InfoFromPersistedBead(sessionBead)},
		nil,
		nil,
		nil,
		nil,
		[]beads.Bead{readyWork},
		[]bool{true}, // readyAssignedFlags: bead IS ready
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	if len(input.WorkBeads) != 1 || !input.WorkBeads[0].Ready {
		t.Fatalf("WorkBeads = %+v, want one bead with Ready=true", input.WorkBeads)
	}

	decisions := ComputeAwakeSet(input)
	got := decisions["gc__run-operator-mc-1"]
	if !got.ShouldWake || got.Reason != "assigned-work" {
		t.Fatalf("ready assigned open bead should wake session; got decision = %+v", got)
	}
}

// TestBuildAwakeInputFromReconciler_InProgressAssignedBeadStillWakes is the
// regression guard: in-progress assigned work keeps its session awake
// regardless of readyAssignedFlags (workBeadHasAwakeDemand returns true for
// in_progress unconditionally).
func TestBuildAwakeInputFromReconciler_InProgressAssignedBeadStillWakes(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{Agents: []config.Agent{{Name: "gc.run-operator"}}}
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "gc__run-operator-mc-1",
			"template":     "gc.run-operator",
		},
	}
	inProgressWork := beads.Bead{
		ID:       "ga-active",
		Status:   "in_progress",
		Assignee: "gc__run-operator-mc-1",
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"",
		[]session.Info{session.InfoFromPersistedBead(sessionBead)},
		nil,
		nil,
		nil,
		nil,
		[]beads.Bead{inProgressWork},
		nil, // readyAssignedFlags omitted entirely: in_progress must still wake
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	decisions := ComputeAwakeSet(input)
	got := decisions["gc__run-operator-mc-1"]
	if !got.ShouldWake || got.Reason != "assigned-work" {
		t.Fatalf("in-progress assigned bead should wake session; got decision = %+v", got)
	}
}

// TestBuildAwakeInputFromReconciler_CrossStoreSameIDReadinessIsStoreScoped pins
// the cross-store readiness fix: AssignedWorkBeads can carry the same bead ID
// from independent city and rig stores. A ready city bead must NOT mark a
// blocked open rig bead with the SAME ID as ready in the awake bridge — that
// store-blind leak (readiness keyed by bead ID alone) reintroduced the
// awake-demand hang. readyAssignedFlagsForBeads resolves readiness by
// (store ref, bead ID), so the blocked rig bead reaches the bridge Ready=false
// and its session sleeps while the ready city bead's session wakes.
func TestBuildAwakeInputFromReconciler_CrossStoreSameIDReadinessIsStoreScoped(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{Agents: []config.Agent{{Name: "gc.run-operator"}}}
	citySession := beads.Bead{
		ID:     "mc-session-city",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "gc__run-operator-city",
			"template":     "gc.run-operator",
		},
	}
	rigSession := beads.Bead{
		ID:     "mc-session-rig",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "gc__run-operator-rig",
			"template":     "gc.run-operator",
		},
	}
	// Same bead ID lives in two stores: the city copy is genuinely ready, the rig
	// copy is a blocked open bead admitted via the open-routed orphan-release pass.
	const sharedID = "ga-shared"
	cityWork := beads.Bead{
		ID:       sharedID,
		Status:   "open",
		Assignee: "gc__run-operator-city",
		Metadata: map[string]string{"gc.routed_to": "gc.run-operator"},
	}
	rigWork := beads.Bead{
		ID:       sharedID,
		Status:   "open",
		Assignee: "gc__run-operator-rig",
		Metadata: map[string]string{"gc.routed_to": "gc.run-operator"},
	}

	work := []beads.Bead{cityWork, rigWork}
	storeRefs := []string{"", "repo"}
	// Store-scoped readiness verdict: only the city copy (store ref "") is ready.
	readyAssigned := map[storeScopedBeadKey]bool{
		{StoreRef: "", ID: sharedID}: true,
	}
	flags := readyAssignedFlagsForBeads(readyAssigned, work, storeRefs)
	if len(flags) != 2 || !flags[0] || flags[1] {
		t.Fatalf("readyAssignedFlagsForBeads = %#v, want [true false] (city ready, rig blocked despite shared ID)", flags)
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"",
		[]session.Info{session.InfoFromPersistedBead(citySession), session.InfoFromPersistedBead(rigSession)},
		nil,
		nil,
		nil,
		nil,
		work,
		flags,
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	readyByAssignee := make(map[string]bool, len(input.WorkBeads))
	for _, wb := range input.WorkBeads {
		readyByAssignee[wb.Assignee] = wb.Ready
	}
	if !readyByAssignee["gc__run-operator-city"] {
		t.Fatal("city copy of the shared-ID bead must reach the bridge Ready=true")
	}
	if readyByAssignee["gc__run-operator-rig"] {
		t.Fatal("rig copy of the shared-ID bead must reach the bridge Ready=false (store-scoped readiness)")
	}

	decisions := ComputeAwakeSet(input)
	if got := decisions["gc__run-operator-rig"]; got.ShouldWake {
		t.Fatalf("rig session should sleep for its blocked same-ID bead; got decision = %+v", got)
	}
	if got := decisions["gc__run-operator-city"]; !got.ShouldWake || got.Reason != "assigned-work" {
		t.Fatalf("city session should wake for its ready bead; got decision = %+v", got)
	}
}

func TestAwakeSetToWakeEvalsPreservesDecisionReason(t *testing.T) {
	evals := awakeSetToWakeEvals(
		map[string]AwakeDecision{
			"s-worker": {ShouldWake: true, Reason: "assigned-work"},
		},
		[]AwakeSessionBead{{
			ID:          "mc-session-1",
			SessionName: "s-worker",
		}},
	)

	got := evals["mc-session-1"]
	if got.Reason != "assigned-work" {
		t.Fatalf("Reason = %q, want assigned-work", got.Reason)
	}
	if !containsWakeReason(got.Reasons, WakeWork) {
		t.Fatalf("Reasons = %v, want WakeWork", got.Reasons)
	}
}

func TestAwakeSetToWakeEvalsMapsMinActiveToWakeConfig(t *testing.T) {
	evals := awakeSetToWakeEvals(
		map[string]AwakeDecision{
			"s-worker": {ShouldWake: true, Reason: "min-active"},
		},
		[]AwakeSessionBead{{
			ID:          "mc-session-1",
			SessionName: "s-worker",
		}},
	)

	got := evals["mc-session-1"]
	if got.Reason != "min-active" {
		t.Fatalf("Reason = %q, want min-active", got.Reason)
	}
	if !containsWakeReason(got.Reasons, WakeConfig) {
		t.Fatalf("Reasons = %v, want WakeConfig", got.Reasons)
	}
}

func TestBuildAwakeInputFromReconcilerCarriesNamedSessionDemand(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{
			{Name: "primary", Template: "worker", Mode: "on_demand"},
		},
	}
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":                     "asleep",
			"session_name":              "primary",
			"template":                  "worker",
			"configured_named_identity": "primary",
			"configured_named_mode":     "on_demand",
		},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(sessionBead)},
		map[string]int{"worker": 1},
		map[string]bool{"primary": true},
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	if !input.NamedSessionDemand["primary"] {
		t.Fatalf("NamedSessionDemand[primary] = false, want true")
	}
	decisions := ComputeAwakeSet(input)
	got := decisions["primary"]
	if !got.ShouldWake || got.Reason != "named-demand" {
		t.Fatalf("decision = %+v, want named-demand wake", got)
	}
}

func TestBuildAwakeInputFromReconciler_RigNamedWorkQueryDemandWakesCanonicalSession(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		ResolvedWorkspaceName: "gc-test",
		Agents: []config.Agent{
			{Name: "worker", Scope: "rig", WorkQuery: "echo 1"},
		},
		NamedSessions: []config.NamedSession{
			{Name: "refinery", Template: "worker", Mode: "on_demand", Scope: "rig", Dir: "rig-a"},
		},
	}
	identity := "rig-a/refinery"
	runtimeName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, identity)
	sessionBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"configured_named_session":  "true",
			"state":                     "asleep",
			"session_name":              runtimeName,
			"template":                  "rig-a/worker",
			"configured_named_identity": identity,
			"configured_named_mode":     "on_demand",
		},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(sessionBead)},
		nil,
		nil,
		map[string]bool{"rig-a/worker": true},
		nil,
		nil,
		nil,
		nil,
		nil,
		runtime.NewFake(),
		now,
	)

	decisions := ComputeAwakeSet(input)
	got, ok := decisions[runtimeName]
	if !ok {
		t.Fatal("decision for rig named session missing from awake set")
	}
	if !got.ShouldWake {
		t.Fatalf("decision = %+v, want wake", got)
	}
	if got.Reason != "work-query" {
		t.Fatalf("Reason = %q, want work-query", got.Reason)
	}
}

// TestBuildAwakeInputFromReconcilerNamedAlwaysPostChurnRewakes pins the
// contract for a mode=always named session that was put to sleep after churn:
// if named-session metadata survives, the next awake-set pass must re-wake it.
func TestBuildAwakeInputFromReconcilerNamedAlwaysPostChurnRewakes(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{
		Agents: []config.Agent{{Name: "worker"}},
		NamedSessions: []config.NamedSession{
			{Name: "worker", Template: "worker", Mode: "always"},
		},
	}
	postChurnBead := beads.Bead{
		ID:     "mc-session-1",
		Status: "open",
		Type:   "session",
		Metadata: map[string]string{
			"state":                      "asleep",
			"sleep_reason":               "",
			"state_reason":               "creation_complete",
			"last_woke_at":               "",
			"wake_attempts":              "0",
			"churn_count":                "1",
			"session_key":                "",
			"continuation_reset_pending": "",
			"pending_create_claim":       "",
			"pin_awake":                  "",
			"session_name":               "worker",
			"template":                   "worker",
			"configured_named_identity":  "worker",
			"configured_named_mode":      "always",
		},
	}

	input := buildAwakeInputFromReconciler(
		cfg,
		"", // cityPath: empty exercises zero suspension state
		[]session.Info{session.InfoFromPersistedBead(postChurnBead)},
		nil, nil, nil, nil, nil, nil, nil, nil,
		runtime.NewFake(),
		now,
	)

	if len(input.SessionBeads) != 1 {
		t.Fatalf("SessionBeads length = %d, want 1", len(input.SessionBeads))
	}
	bead := input.SessionBeads[0]
	if bead.NamedIdentity != "worker" {
		t.Errorf("projected NamedIdentity = %q, want worker (configured_named_identity should survive churn)", bead.NamedIdentity)
	}
	if bead.State != "asleep" {
		t.Errorf("projected State = %q, want asleep", bead.State)
	}

	decisions := ComputeAwakeSet(input)
	got, ok := decisions["worker"]
	if !ok {
		t.Fatal("decision for 'worker' missing from awake set")
	}
	if !got.ShouldWake {
		t.Fatalf("post-churn named-always session should wake; got decision = %+v", got)
	}
	if got.Reason != "named-always" {
		t.Errorf("wake reason = %q, want named-always", got.Reason)
	}
}

// TestBuildAwakeInputFoldRowWakeGate (HIGH-1, HIGH-2, MEDIUM) pins the corrected
// fold-row wake-anchor gate. A Lumen fold-owned claim (tierBHookStoreName store ref,
// recorded claimant_id) holds its claimant's pool bead awake — the assigned-work wake
// anchor. The gate drops that anchor ONLY when the claimant is dead by the firewall's
// instance-death definition (ABSENT from the open set, open+stranded, or crashed-active:
// open with state=active and a dead runtime), so the killed bead reaches the poolFreeable
// close and the firewall strands it. An ASLEEP-recoverable claimant (gc stop/city-stop,
// max-session-age, idle-timeout — state=asleep, runtime down) is KEPT so its wake anchor
// survives and it re-wakes to resume/adopt: dropping it lost the anchor and caused the
// stop/start strand (HIGH-1) and the max-age permanent wedge (HIGH-2). A LIVE mid-do
// claimant still anchors (L1 preserve). The store-ref discriminant means an ordinary bd
// bead carrying a stray, user-set claimant_id is never gated (MEDIUM).
func TestBuildAwakeInputFoldRowWakeGate(t *testing.T) {
	now := time.Now().UTC()
	const foldID = "hello"
	const claimantID = "sess-A"

	// build renders a one-fold-row awake input. claimantState/sleepReason describe the
	// claimant session bead; alive is its runtime liveness; stranded stamps the reconciler
	// marker; claimantPresent controls open-set membership; storeRef is the row's store.
	build := func(claimantState, sleepReason string, alive, claimantPresent, stranded bool, storeRef string) AwakeInput {
		workBead := beads.Bead{
			ID: foldID, Status: "in_progress", Assignee: "worker-a",
			Metadata: map[string]string{engine.ClaimantIDMetaKey: claimantID},
		}
		var sessionInfos []session.Info
		if claimantPresent {
			meta := map[string]string{"state": claimantState, "session_name": "worker-a", "template": "worker"}
			if sleepReason != "" {
				meta["sleep_reason"] = sleepReason
			}
			if stranded {
				meta["stranded_event_emitted_at"] = now.Format(time.RFC3339)
			}
			sessionInfos = []session.Info{session.InfoFromPersistedBead(beads.Bead{
				ID: claimantID, Status: "open", Type: "session", Metadata: meta,
			})}
		}
		return buildAwakeInputFromReconciler(
			&config.City{}, "",
			sessionInfos,
			nil, nil, nil, nil,
			[]beads.Bead{workBead},
			[]bool{false},
			[]string{storeRef},
			[]wakeTarget{{session: &beads.Bead{ID: claimantID}, alive: alive}},
			nil, now,
		)
	}
	kept := func(in AwakeInput) bool {
		for _, wb := range in.WorkBeads {
			if wb.ID == foldID {
				return true
			}
		}
		return false
	}

	// HIGH-1/HIGH-2: an asleep-recoverable claimant is KEPT under every real sleep_reason.
	// This FAILS against the old runtime-liveness gate (alive=false → dropped). Raw "asleep"
	// (SleepPatch: max-session-age, idle) and raw "stopped" (city-stop) both normalize to
	// asleep-recoverable via CompatState, so both must be kept.
	t.Run("asleep_recoverable_kept", func(t *testing.T) {
		cases := []struct{ rawState, reason string }{
			{"asleep", "city-stop"},
			{"asleep", "max-session-age"},
			{"asleep", "idle"},
			{"stopped", "city-stop"},
		}
		for _, c := range cases {
			if !kept(build(c.rawState, c.reason, false, true, false, tierBHookStoreName)) {
				t.Fatalf("asleep-recoverable claimant (state=%q reason=%q) DROPPED — wake anchor lost, re-wake never fires", c.rawState, c.reason)
			}
		}
	})

	// Crashed-active: open, state=active, runtime dead → a real kill → DROPPED so the
	// pool bead closes and the firewall strands it (the SIGKILL e2e path).
	t.Run("crashed_active_dropped", func(t *testing.T) {
		if kept(build("active", "", false, true, false, tierBHookStoreName)) {
			t.Fatal("crashed-active claimant (state=active,!alive) KEPT — holds pool bead open → firewall wedge")
		}
	})

	// Crashed session slept with a DEATH reason (runtime-missing, provider-terminal-error,
	// failed-create) or an unknown reason is asleep but must still be DROPPED — keeping its
	// anchor holds shouldWake=true so the reconciler's poolFreeable close/strand path
	// (which requires !shouldWake) never fires (the observed wedge). Only the intentional-
	// stop reasons above resume.
	t.Run("crashed_asleep_death_reason_dropped", func(t *testing.T) {
		for _, reason := range []string{"runtime-missing", "provider-terminal-error", "failed-create", "" /*unknown*/} {
			if kept(build("asleep", reason, false, true, false, tierBHookStoreName)) {
				t.Fatalf("crashed asleep claimant (sleep_reason=%q) KEPT — poolFreeable never strands it → wedge", reason)
			}
		}
	})

	// Absent from the open set (recycled / fresh-id respawn) → DROPPED.
	t.Run("absent_dropped", func(t *testing.T) {
		if kept(build("active", "", false, false, false, tierBHookStoreName)) {
			t.Fatal("absent claimant KEPT — a recycled claimant must not anchor its pool bead")
		}
	})

	// Open + stranded marker → dead by the firewall's own verdict, even when asleep.
	t.Run("stranded_marked_dropped", func(t *testing.T) {
		if kept(build("asleep", "idle", false, true, true, tierBHookStoreName)) {
			t.Fatal("stranded-marked claimant KEPT — the firewall already ruled it dead")
		}
	})

	// Live mid-do claimant → KEPT (L1 preserve).
	t.Run("live_mid_do_kept", func(t *testing.T) {
		if !kept(build("active", "", true, true, false, tierBHookStoreName)) {
			t.Fatal("live claimant DROPPED — would drain a mid-do worker (L1 preserve broken)")
		}
	})

	// MEDIUM: the same dead signal on a NON-Tier-B bead — an ordinary bd bead carrying a
	// stray, user-set claimant_id — must NOT be gated (byte-identical to pre-gate behavior).
	t.Run("store_ref_discriminant_ordinary_bead_kept", func(t *testing.T) {
		if !kept(build("active", "", false, true, false, "some-rig")) {
			t.Fatal("ordinary bd bead (non-Tier-B store ref) with a stray claimant_id was gated")
		}
		if !kept(build("active", "", false, false, false, "")) {
			t.Fatal("ordinary bd bead (empty store ref) with a stray claimant_id was gated")
		}
	})

	// A legacy Tier-B fold row with NO recorded claimant_id keeps the name path — never
	// gated by the id rule (matches the firewall's legacy fallback).
	t.Run("tier_b_legacy_no_claimant_id_kept", func(t *testing.T) {
		workBead := beads.Bead{ID: foldID, Status: "in_progress", Assignee: "worker-a"}
		in := buildAwakeInputFromReconciler(
			&config.City{}, "", nil,
			nil, nil, nil, nil,
			[]beads.Bead{workBead}, []bool{false}, []string{tierBHookStoreName},
			[]wakeTarget{{session: &beads.Bead{ID: claimantID}, alive: false}},
			nil, now,
		)
		if !kept(in) {
			t.Fatal("legacy Tier-B fold row with empty claimant_id was gated by the id rule")
		}
	})
}

// TestFoldRowAsleepClaimantRewakesToResume is the HIGH-1/HIGH-2 wake-DECISION pin: an
// asleep claimant (here a max-session-age restart) whose Tier-B fold row is still
// in_progress must not merely keep its anchor — ComputeAwakeSet must WAKE it so it
// resumes/adopts. The old runtime-liveness gate dropped the anchor, so with the default
// min_active_sessions=0 the session had no wake reason and stayed asleep: the pool bead
// was freed → the run stranded (HIGH-1 stop/start), or under a max-age sleep the freeable
// allowlist excludes, the bead stayed open+asleep forever → permanent wedge (HIGH-2).
func TestFoldRowAsleepClaimantRewakesToResume(t *testing.T) {
	now := time.Now().UTC()
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}}
	claimant := beads.Bead{
		ID: "sess-A", Status: "open", Type: "session",
		Metadata: map[string]string{
			"state": "asleep", "sleep_reason": "max-session-age",
			"session_name": "gc__worker-mc-1", "template": "worker",
		},
	}
	foldRow := beads.Bead{
		ID: "n1", Status: "in_progress", Assignee: "gc__worker-mc-1",
		Metadata: map[string]string{engine.ClaimantIDMetaKey: "sess-A"},
	}
	input := buildAwakeInputFromReconciler(
		cfg, "",
		[]session.Info{session.InfoFromPersistedBead(claimant)},
		nil, nil, nil, nil,
		[]beads.Bead{foldRow},
		[]bool{false},
		[]string{tierBHookStoreName},
		[]wakeTarget{{session: &beads.Bead{ID: "sess-A"}, alive: false}},
		nil, now,
	)
	if len(input.WorkBeads) != 1 {
		t.Fatalf("asleep claimant's in_progress fold row missing from WorkBeads: %+v", input.WorkBeads)
	}
	decisions := ComputeAwakeSet(input)
	got := decisions["gc__worker-mc-1"]
	if !got.ShouldWake || got.Reason != "assigned-work" {
		t.Fatalf("asleep claimant did not re-wake for its in_progress fold row; decision = %+v", got)
	}
}
