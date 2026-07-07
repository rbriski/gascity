package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/graphstore"
)

// spyJournalFrontier records how many times the journal leg is consulted and
// returns a canned result, so the INERT invariant (the legacy path never touches
// the journal leg) is directly observable.
type spyJournalFrontier struct {
	calls int
	rows  []hookBead
	opted bool
	err   error
}

func (s *spyJournalFrontier) fn() journalFrontierFunc {
	return func() ([]hookBead, bool, error) {
		s.calls++
		return s.rows, s.opted, s.err
	}
}

func ids(bs []hookBead) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.ID
	}
	return out
}

// --- ControlFrontierParams mapping (§4.1 lockstep) --------------------------

func TestControlFrontierInputsMapsShellFlags(t *testing.T) {
	agentCfg := config.Agent{Dir: "core", Name: config.ControlDispatcherAgentName}
	env := map[string]string{
		"GC_SESSION_NAME": "sess-name",
		"GC_ALIAS":        "alias1",
		"GC_SESSION_ID":   "sess-id",
	}
	lookup := func(k string) string { return env[k] }

	got := controlFrontierInputs(agentCfg, config.BeadsConfig{}, "ctl-sess", lookup)

	// Assignee tier order mirrors the shell loop
	// (dispatch_runtime.go:813-819): [SESSION_NAME(control), SESSION_NAME(env),
	// ALIAS, TARGET, SESSION_ID], each followed by its
	// "${id%control-dispatcher}workflow-control" legacy alias. Only the target
	// ("core/control-dispatcher") ends in control-dispatcher, so only it yields a
	// legacy alias ("core/workflow-control"), interleaved immediately after.
	wantCandidates := []string{
		"ctl-sess",
		"sess-name",
		"alias1",
		"core/control-dispatcher",
		"core/workflow-control",
		"sess-id",
	}
	if !reflect.DeepEqual(got.AssigneeCandidates, wantCandidates) {
		t.Fatalf("AssigneeCandidates = %#v, want %#v", got.AssigneeCandidates, wantCandidates)
	}

	// Routes: [TARGET, LEGACY_TARGET, BARE_TARGET] in order. The bare route is
	// empty for an already-bare "core/control-dispatcher", so only two routes.
	wantRoutes := []string{"core/control-dispatcher", "core/workflow-control"}
	if !reflect.DeepEqual(got.Routes, wantRoutes) {
		t.Fatalf("Routes = %#v, want %#v", got.Routes, wantRoutes)
	}

	// run_target BEFORE routed_to (first-wins prefers run_target).
	wantKeys := []string{beadmeta.RunTargetMetadataKey, beadmeta.RoutedToMetadataKey}
	if !reflect.DeepEqual(got.RouteMetadataKeys, wantKeys) {
		t.Fatalf("RouteMetadataKeys = %#v, want %#v", got.RouteMetadataKeys, wantKeys)
	}
	if got.InstantiatingMetadataKey != beadmeta.InstantiatingMetadataKey {
		t.Fatalf("InstantiatingMetadataKey = %q, want %q", got.InstantiatingMetadataKey, beadmeta.InstantiatingMetadataKey)
	}
	if got.LimitPerTier != workflowServeScanLimit {
		t.Fatalf("LimitPerTier = %d, want %d", got.LimitPerTier, workflowServeScanLimit)
	}
	if got.IncludeEphemeral {
		t.Fatal("IncludeEphemeral = true for a default BeadsConfig, want false")
	}
}

// TestControlFrontierInputsBareRouteAndAssigneeAlias exercises the bare-route
// and assignee-legacy-alias branches for a dotted control-dispatcher identity.
func TestControlFrontierInputsBareRouteAndAssigneeAlias(t *testing.T) {
	agentCfg := config.Agent{Name: "core." + config.ControlDispatcherAgentName}
	got := controlFrontierInputs(agentCfg, config.BeadsConfig{}, "", func(string) string { return "" })

	// target = "core.control-dispatcher"; its assignee legacy alias strips the
	// literal "control-dispatcher" suffix -> "core.workflow-control".
	wantCandidates := []string{"core.control-dispatcher", "core.workflow-control"}
	if !reflect.DeepEqual(got.AssigneeCandidates, wantCandidates) {
		t.Fatalf("AssigneeCandidates = %#v, want %#v", got.AssigneeCandidates, wantCandidates)
	}
	// Routes: target then bare route "control-dispatcher"; no route-legacy for a
	// dotted (non-"/") suffix.
	wantRoutes := []string{"core.control-dispatcher", "control-dispatcher"}
	if !reflect.DeepEqual(got.Routes, wantRoutes) {
		t.Fatalf("Routes = %#v, want %#v", got.Routes, wantRoutes)
	}
}

// --- default-legacy byte-identity (INERT, §6) -------------------------------

func TestComposeQueueLegacyModeNeverInvokesJournal(t *testing.T) {
	for _, mode := range []string{"", "legacy", "garbage", "  "} {
		t.Run("mode="+mode, func(t *testing.T) {
			t.Setenv(graphFrontierModeEnvVar, mode)
			spy := &spyJournalFrontier{rows: []hookBead{{ID: "jrnl"}}, opted: true}
			legacy := []hookBead{{ID: "l1"}, {ID: "l2"}}

			got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
				t.TempDir(), legacy, spy.fn(), io.Discard)
			if err != nil {
				t.Fatalf("composeWorkflowServeQueue: %v", err)
			}
			if spy.calls != 0 {
				t.Fatalf("journal frontier invoked %d times in legacy mode, want 0 (INERT)", spy.calls)
			}
			if !reflect.DeepEqual(ids(got), []string{"l1", "l2"}) {
				t.Fatalf("queue = %v, want byte-identical legacy [l1 l2]", ids(got))
			}
		})
	}
}

func TestComposeQueueNilFrontierIsLegacy(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "serve")
	legacy := []hookBead{{ID: "l1"}}
	got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		t.TempDir(), legacy, nil, io.Discard)
	if err != nil {
		t.Fatalf("composeWorkflowServeQueue: %v", err)
	}
	if !reflect.DeepEqual(ids(got), []string{"l1"}) {
		t.Fatalf("queue = %v, want legacy [l1] when journalFrontier is nil", ids(got))
	}
}

func TestComposeQueueNonOptedCityIsLegacy(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "serve")
	spy := &spyJournalFrontier{rows: []hookBead{{ID: "jrnl"}}, opted: false} // not opted
	legacy := []hookBead{{ID: "l1"}, {ID: "l2"}}
	got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		t.TempDir(), legacy, spy.fn(), io.Discard)
	if err != nil {
		t.Fatalf("composeWorkflowServeQueue: %v", err)
	}
	if !reflect.DeepEqual(ids(got), []string{"l1", "l2"}) {
		t.Fatalf("queue = %v, want legacy [l1 l2] for a non-opted city", ids(got))
	}
}

// --- serve mode: residence-split merge (§2.3) -------------------------------

func TestComposeQueueServeModeMergesResidence(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "serve")
	// journal returns a shared id (l1) plus a distinct journal id — dedupe must
	// keep legacy l1 and append only the disjoint journal row.
	spy := &spyJournalFrontier{
		rows:  []hookBead{{ID: "l1"}, {ID: "gcg-j1"}},
		opted: true,
	}
	legacy := []hookBead{{ID: "l1"}, {ID: "l2"}}
	got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		t.TempDir(), legacy, spy.fn(), io.Discard)
	if err != nil {
		t.Fatalf("composeWorkflowServeQueue: %v", err)
	}
	if want := []string{"l1", "l2", "gcg-j1"}; !reflect.DeepEqual(ids(got), want) {
		t.Fatalf("queue = %v, want legacy-first merged/deduped %v", ids(got), want)
	}
	if spy.calls != 1 {
		t.Fatalf("journal frontier invoked %d times, want 1", spy.calls)
	}
}

// --- serve/shadow: journal read error discipline (§2.2.4, HIGH-1) -----------

func TestComposeQueueJournalErrorFailsTickLoudly(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "serve")
	spy := &spyJournalFrontier{err: context.DeadlineExceeded, opted: true}
	legacy := []hookBead{{ID: "l1"}}
	_, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		t.TempDir(), legacy, spy.fn(), io.Discard)
	if err == nil {
		t.Fatal("composeWorkflowServeQueue returned nil error on a journal read failure, want a loud hard-fail")
	}
}

// TestComposeQueueShadowModeJournalErrorDegradesToLegacy pins HIGH-1: shadow is
// zero-blast-radius observation and ALWAYS serves the legacy queue. A
// non-transient journal fault (schema drift, corrupt journal.db, malformed
// defer_until) must degrade to legacy — never fail the tick and kill the
// control-dispatcher follow loop.
func TestComposeQueueShadowModeJournalErrorDegradesToLegacy(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "shadow")
	spy := &spyJournalFrontier{
		err:   errors.New("journal schema drift: no such column: defer_until"),
		opted: true,
	}
	legacy := []hookBead{{ID: "l1"}, {ID: "l2"}}
	got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		t.TempDir(), legacy, spy.fn(), io.Discard)
	if err != nil {
		t.Fatalf("shadow-mode journal error must degrade to legacy, got err=%v", err)
	}
	if !reflect.DeepEqual(ids(got), []string{"l1", "l2"}) {
		t.Fatalf("queue = %v, want legacy [l1 l2] served unchanged on a shadow-mode journal fault", ids(got))
	}
}

// --- MED-1: serve mode must hard-fail on an opted-but-unopenable journal leg -

// optedButUnopenableCity builds a city whose graph scope is OPTED (a marker path
// exists) but whose journal leg cannot be opened/probed: a regular file sits
// where the scope's .beads directory is expected, so the scope probe fails with
// ENOTDIR — an "opted, cannot determine" state, NOT authoritative absence.
func optedButUnopenableCity(t *testing.T) string {
	t.Helper()
	city := t.TempDir()
	scopeBeads := filepath.Join(city, ".gc", "graph", ".beads")
	if err := os.MkdirAll(filepath.Dir(scopeBeads), 0o755); err != nil {
		t.Fatalf("mkdir graph scope root: %v", err)
	}
	if err := os.WriteFile(scopeBeads, []byte("not a dir"), 0o644); err != nil {
		t.Fatalf("write blocking file: %v", err)
	}
	return city
}

func TestJournalControlFrontierListOptedButOpenFailsSurfacesError(t *testing.T) {
	city := optedButUnopenableCity(t)
	rows, opted, err := journalControlFrontierList(city, beads.ControlFrontierParams{})
	if err == nil {
		t.Fatal("journalControlFrontierList swallowed an opted-but-open-fails error as silent legacy (MED-1)")
	}
	if !opted {
		t.Fatal("opted = false on an open failure; serve mode would silently serve legacy and strand journal roots")
	}
	if rows != nil {
		t.Fatalf("rows = %v, want nil when the journal leg cannot be opened", rows)
	}
}

func TestComposeQueueServeOptedButOpenFailsHardFails(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "serve")
	city := optedButUnopenableCity(t)
	frontier := func() ([]hookBead, bool, error) {
		return journalControlFrontierList(city, beads.ControlFrontierParams{})
	}
	legacy := []hookBead{{ID: "l1"}}
	_, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		city, legacy, frontier, io.Discard)
	if err == nil {
		t.Fatal("serve mode must hard-fail when an opted city's journal leg cannot be opened, got silent legacy (MED-1)")
	}
}

// --- shadow mode: divergence tee (§4.3) -------------------------------------

func readDivergenceEvents(t *testing.T, cityPath string) []events.Event {
	t.Helper()
	evts, err := events.ReadFiltered(filepath.Join(cityPath, ".gc", "events.jsonl"),
		events.Filter{Type: events.FrontierShadowDivergence})
	if err != nil {
		t.Fatalf("reading divergence events: %v", err)
	}
	return evts
}

func TestComposeQueueShadowEmitsDivergenceAndServesLegacy(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "shadow")
	city := t.TempDir()
	spy := &spyJournalFrontier{
		rows:  []hookBead{{ID: "l1"}, {ID: "gcg-j1"}}, // differs from legacy
		opted: true,
	}
	legacy := []hookBead{{ID: "l1"}, {ID: "l2"}}

	got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		city, legacy, spy.fn(), io.Discard)
	if err != nil {
		t.Fatalf("composeWorkflowServeQueue: %v", err)
	}
	// Shadow serves the LEGACY result unchanged.
	if !reflect.DeepEqual(ids(got), []string{"l1", "l2"}) {
		t.Fatalf("shadow queue = %v, want legacy [l1 l2] served unchanged", ids(got))
	}

	evts := readDivergenceEvents(t, city)
	if len(evts) != 1 {
		t.Fatalf("divergence events = %d, want exactly 1", len(evts))
	}
	var payload events.FrontierShadowDivergencePayload
	if err := json.Unmarshal(evts[0].Payload, &payload); err != nil {
		t.Fatalf("decode divergence payload: %v", err)
	}
	if !reflect.DeepEqual(payload.OnlyInLegacy, []string{"l2"}) {
		t.Fatalf("OnlyInLegacy = %v, want [l2]", payload.OnlyInLegacy)
	}
	if !reflect.DeepEqual(payload.OnlyInJournal, []string{"gcg-j1"}) {
		t.Fatalf("OnlyInJournal = %v, want [gcg-j1]", payload.OnlyInJournal)
	}
	if payload.LegacyCount != 2 || payload.JournalCount != 2 {
		t.Fatalf("counts = (%d,%d), want (2,2)", payload.LegacyCount, payload.JournalCount)
	}
}

// TestComposeQueueShadowResidenceDisjointEmitsNoDivergence pins HIGH-2: at P2 the
// legacy leg (bd ready over the city/rig store) and the journal leg
// (ControlFrontier over .gc/graph/journal.db) hold RESIDENCE-DISJOINT populations
// — no dual-write mirror exists until P3 — so a naive full-set diff would flag the
// entire legacy set as only_in_legacy on every drain. The dormant-correct tee
// emits nothing because the two legs share no bead. (The genuine-overlap emit path
// is covered by TestComposeQueueShadowEmitsDivergenceAndServesLegacy, whose legs
// share id "l1".)
func TestComposeQueueShadowResidenceDisjointEmitsNoDivergence(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "shadow")
	city := t.TempDir()
	spy := &spyJournalFrontier{
		rows:  []hookBead{{ID: "gcg-j1"}, {ID: "gcg-j2"}}, // disjoint from legacy
		opted: true,
	}
	legacy := []hookBead{{ID: "l1"}, {ID: "l2"}}

	got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		city, legacy, spy.fn(), io.Discard)
	if err != nil {
		t.Fatalf("composeWorkflowServeQueue: %v", err)
	}
	if !reflect.DeepEqual(ids(got), []string{"l1", "l2"}) {
		t.Fatalf("shadow queue = %v, want legacy [l1 l2] served unchanged", ids(got))
	}
	if evts := readDivergenceEvents(t, city); len(evts) != 0 {
		t.Fatalf("divergence events = %d, want 0 for residence-disjoint legs (the P2 reality)", len(evts))
	}
}

func TestComposeQueueShadowNoEventWhenIdentical(t *testing.T) {
	t.Setenv(graphFrontierModeEnvVar, "shadow")
	city := t.TempDir()
	// Same id set (order need not match — divergence is a set-diff).
	spy := &spyJournalFrontier{
		rows:  []hookBead{{ID: "l2"}, {ID: "l1"}},
		opted: true,
	}
	legacy := []hookBead{{ID: "l1"}, {ID: "l2"}}

	got, err := composeWorkflowServeQueue(config.Agent{Name: config.ControlDispatcherAgentName},
		city, legacy, spy.fn(), io.Discard)
	if err != nil {
		t.Fatalf("composeWorkflowServeQueue: %v", err)
	}
	if !reflect.DeepEqual(ids(got), []string{"l1", "l2"}) {
		t.Fatalf("shadow queue = %v, want legacy [l1 l2]", ids(got))
	}
	if evts := readDivergenceEvents(t, city); len(evts) != 0 {
		t.Fatalf("divergence events = %d, want 0 when frontiers are identical", len(evts))
	}
}

// --- cap forwarding through the residence router (§ item 1) -----------------

func newTestJournalLeg(t *testing.T) beads.Store {
	t.Helper()
	gs, err := graphstore.Open(context.Background(), filepath.Join(t.TempDir(), "journal.db"),
		graphstore.Options{CityID: "city-under-test"})
	if err != nil {
		t.Fatalf("open graphstore: %v", err)
	}
	t.Cleanup(func() { _ = gs.Close() })
	return beads.NewJournalStore(gs)
}

func TestControlFrontierStoreForReachesJournalLegThroughRouter(t *testing.T) {
	journal := wrapStoreWithBeadPolicies(newTestJournalLeg(t), nil)
	legacy := beads.NewMemStore()
	router := newResidenceRoutingGraphStore(journal, legacy)

	frontier, ok := beads.ControlFrontierStoreFor(router)
	if !ok {
		t.Fatal("ControlFrontierStoreFor(router) = false, want the journal leg's capability forwarded")
	}
	// The forwarded handle must actually execute a SELECT against the journal leg.
	if _, err := frontier.ControlFrontier(context.Background(), beads.ControlFrontierParams{}); err != nil {
		t.Fatalf("ControlFrontier via router handle: %v", err)
	}
	// The legacy leg (a MemStore) has no frontier capability; the router must not
	// synthesize one from it.
	if _, ok := beads.ControlFrontierStoreFor(legacy); ok {
		t.Fatal("ControlFrontierStoreFor(legacy MemStore) = true, want false")
	}
}
