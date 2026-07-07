package main

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// writeCutoverMarker arms cityPath by writing the marker directly (the file-level
// primitive the arm command wraps), creating the scope dir first.
func writeCutoverMarker(t *testing.T, cityPath string) {
	t.Helper()
	if err := os.MkdirAll(graphScopeRoot(cityPath), 0o755); err != nil {
		t.Fatalf("mkdir graph scope: %v", err)
	}
	if err := atomicWriteFile(graphCutoverMarkerPath(cityPath), []byte("armed\n")); err != nil {
		t.Fatalf("write cutover marker: %v", err)
	}
}

// setFrontierEnv sets or unsets GC_GRAPH_FRONTIER for one subtest. present=false
// genuinely unsets it (restored on cleanup); present=true sets the raw value.
func setFrontierEnv(t *testing.T, raw string, present bool) {
	t.Helper()
	orig, had := os.LookupEnv(graphFrontierModeEnvVar)
	t.Cleanup(func() {
		if had {
			_ = os.Setenv(graphFrontierModeEnvVar, orig)
		} else {
			_ = os.Unsetenv(graphFrontierModeEnvVar)
		}
	})
	if present {
		_ = os.Setenv(graphFrontierModeEnvVar, raw)
	} else {
		_ = os.Unsetenv(graphFrontierModeEnvVar)
	}
}

// TestGraphFrontierModeForCity_Matrix pins the marker×env → mode resolution:
// explicit env is the kill switch (legacy forces legacy even when armed), and an
// unset/blank env lets the cutover marker promote the default to serve.
func TestGraphFrontierModeForCity_Matrix(t *testing.T) {
	cases := []struct {
		name       string
		envPresent bool
		envValue   string
		armed      bool
		want       graphFrontierMode
	}{
		{name: "unset+unarmed=legacy", envPresent: false, armed: false, want: frontierModeLegacy},
		{name: "unset+armed=serve", envPresent: false, armed: true, want: frontierModeServe},
		{name: "blank+armed=serve", envPresent: true, envValue: "", armed: true, want: frontierModeServe},
		{name: "blank+unarmed=legacy", envPresent: true, envValue: "  ", armed: false, want: frontierModeLegacy},
		{name: "explicit-legacy-overrides-marker", envPresent: true, envValue: "legacy", armed: true, want: frontierModeLegacy},
		{name: "explicit-shadow", envPresent: true, envValue: "shadow", armed: true, want: frontierModeShadow},
		{name: "explicit-serve", envPresent: true, envValue: "serve", armed: false, want: frontierModeServe},
		{name: "garbage-is-legacy", envPresent: true, envValue: "banana", armed: true, want: frontierModeLegacy},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			city := t.TempDir()
			if tc.armed {
				writeCutoverMarker(t, city)
			}
			setFrontierEnv(t, tc.envValue, tc.envPresent)
			if got := graphFrontierModeForCity(city); got != tc.want {
				t.Fatalf("graphFrontierModeForCity(armed=%v, env=%q/%v) = %s, want %s",
					tc.armed, tc.envValue, tc.envPresent, got, tc.want)
			}
		})
	}
}

// TestCutoverArmRefusesWithoutParityVerified proves the parity gate: on an opted
// city, arming refuses (non-zero, no marker) without --parity-verified and
// succeeds with it.
func TestCutoverArmRefusesWithoutParityVerified(t *testing.T) {
	city := t.TempDir()
	if err := migrateGraphJournalInit(city); err != nil {
		t.Fatalf("init: %v", err)
	}

	var out, errb bytes.Buffer
	if code := runMigrateGraphJournalCutover(city, false /*disarm*/, false /*parityVerified*/, &out, &errb); code != 1 {
		t.Fatalf("arm without parity: exit=%d, want 1", code)
	}
	if cityGraphCutoverArmed(city) {
		t.Fatal("arm refused but marker was written")
	}
	if !strings.Contains(errb.String(), "parity") || !strings.Contains(errb.String(), "TestControlFrontierParityAgainstRealBd") {
		t.Fatalf("refusal message missing parity guidance: %q", errb.String())
	}

	out.Reset()
	errb.Reset()
	if code := runMigrateGraphJournalCutover(city, false, true /*parityVerified*/, &out, &errb); code != 0 {
		t.Fatalf("arm with parity: exit=%d stderr=%q, want 0", code, errb.String())
	}
	if !cityGraphCutoverArmed(city) {
		t.Fatal("arm with parity did not write the marker")
	}
}

// TestCutoverArmRefusesUnoptedCity proves arming a city that never ran init is
// refused (there is no journal leg to mint roots onto).
func TestCutoverArmRefusesUnoptedCity(t *testing.T) {
	city := t.TempDir()
	err := migrateGraphJournalArmCutover(city)
	if err == nil {
		t.Fatal("arm on un-opted city succeeded; want refusal")
	}
	if !strings.Contains(err.Error(), "init") {
		t.Fatalf("refusal should direct to init: %v", err)
	}
	if cityGraphCutoverArmed(city) {
		t.Fatal("marker written despite refusal")
	}
}

// TestCutoverDisarmRestoresLegacy proves --disarm removes the marker and the
// frontier default falls back to legacy (env unset).
func TestCutoverDisarmRestoresLegacy(t *testing.T) {
	city := t.TempDir()
	if err := migrateGraphJournalInit(city); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := migrateGraphJournalArmCutover(city); err != nil {
		t.Fatalf("arm: %v", err)
	}
	setFrontierEnv(t, "", false)
	if graphFrontierModeForCity(city) != frontierModeServe {
		t.Fatal("armed city should default to serve")
	}

	var out, errb bytes.Buffer
	if code := runMigrateGraphJournalCutover(city, true /*disarm*/, false, &out, &errb); code != 0 {
		t.Fatalf("disarm: exit=%d stderr=%q", code, errb.String())
	}
	if cityGraphCutoverArmed(city) {
		t.Fatal("disarm did not remove the marker")
	}
	if graphFrontierModeForCity(city) != frontierModeLegacy {
		t.Fatal("disarmed city should default to legacy")
	}
	// Disarm is idempotent.
	if code := runMigrateGraphJournalCutover(city, true, false, &out, &errb); code != 0 {
		t.Fatalf("disarm (idempotent): exit=%d", code)
	}
}

// TestCutoverDisarmAndParityMutuallyExclusive guards against an ambiguous invocation.
func TestCutoverDisarmAndParityMutuallyExclusive(t *testing.T) {
	city := t.TempDir()
	var out, errb bytes.Buffer
	if code := runMigrateGraphJournalCutover(city, true /*disarm*/, true /*parityVerified*/, &out, &errb); code != 1 {
		t.Fatalf("disarm+parity: exit=%d, want 1", code)
	}
	if !strings.Contains(errb.String(), "mutually exclusive") {
		t.Fatalf("want mutually-exclusive error, got %q", errb.String())
	}
}

// TestRouterMarkerPresentNewRootBornJournal proves the routing flip: with the
// cutover marker armed, a new root (no ParentID) mints on the journal leg.
func TestRouterMarkerPresentNewRootBornJournal(t *testing.T) {
	city := t.TempDir()
	writeCutoverMarker(t, city)

	journal := newResidenceLegStore("journal", 0)
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStoreForCity(journal, legacy, city)

	if _, err := router.Create(beads.Bead{Title: "born-journal root"}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	if !journal.has("Create") {
		t.Fatalf("armed new root did not mint on the journal leg: journal=%v legacy=%v", journal.calls, legacy.calls)
	}
	if legacy.has("Create") {
		t.Fatalf("armed new root also touched the legacy leg: %v", legacy.calls)
	}
}

// TestRouterMarkerAbsentNewRootLegacy proves inert routing: with no marker a new
// root mints legacy (byte-identical to pre-P3.3), and the journal leg is untouched.
func TestRouterMarkerAbsentNewRootLegacy(t *testing.T) {
	city := t.TempDir() // no marker

	journal := newResidenceLegStore("journal", 0)
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStoreForCity(journal, legacy, city)

	if _, err := router.Create(beads.Bead{Title: "legacy root"}); err != nil {
		t.Fatalf("create root: %v", err)
	}
	if !legacy.has("Create") {
		t.Fatalf("unarmed new root did not mint legacy: legacy=%v", legacy.calls)
	}
	if journal.has("Create") {
		t.Fatalf("unarmed new root touched the journal leg: %v", journal.calls)
	}

	// The no-city constructor is likewise inert regardless of any ambient marker.
	j2 := newResidenceLegStore("journal2", 0)
	l2 := newResidenceLegStore("legacy2", 2000)
	r2 := newResidenceRoutingGraphStore(j2, l2)
	if _, err := r2.Create(beads.Bead{Title: "root"}); err != nil {
		t.Fatalf("create root (no-city): %v", err)
	}
	if !l2.has("Create") || j2.has("Create") {
		t.Fatalf("no-city router should mint legacy: legacy=%v journal=%v", l2.calls, j2.calls)
	}
}

// TestApplyGraphPlanBornJournalRootUnderMarker pins HIGH-1: an un-anchored
// graph-apply plan — the molecule/formula-v2 root-mint shape, whose workflow root
// deliberately carries no ParentID anchor — routes to the JOURNAL leg once the
// cutover marker is armed (matching Create's born-journal policy), and to the
// legacy leg when the marker is absent (byte-identical to pre-P3.3). This is the
// dominant real-city root path (molecule.Instantiate → instantiateViaGraphApply),
// so without the fix an armed city would still mint roots legacy — a silent
// contract mismatch. The intra-plan ParentKey child rides on whichever leg the
// root lands on, proving co-residence.
func TestApplyGraphPlanBornJournalRootUnderMarker(t *testing.T) {
	molPlan := func() *beads.GraphApplyPlan {
		return &beads.GraphApplyPlan{
			Nodes: []beads.GraphApplyNode{
				{Key: "root", Title: "mol"},
				{Key: "step", Title: "step", ParentKey: "root"},
			},
		}
	}

	t.Run("armed=journal", func(t *testing.T) {
		city := t.TempDir()
		writeCutoverMarker(t, city)
		var applied []appliedPlan
		journal := newResidenceApplyLeg("journal", 0, &applied)
		legacy := newResidenceApplyLeg("legacy", 1000, &applied)
		router := newResidenceRoutingGraphStoreForCity(journal, legacy, city)
		handle, ok := router.GraphApplyHandle()
		if !ok {
			t.Fatal("router did not expose a graph-apply handle")
		}
		if _, err := handle.ApplyGraphPlan(context.Background(), molPlan()); err != nil {
			t.Fatalf("armed apply: %v", err)
		}
		if len(applied) != 1 || applied[0].leg != "journal" {
			t.Fatalf("armed un-anchored root routed to %+v, want journal leg", applied)
		}
	})

	t.Run("unarmed=legacy", func(t *testing.T) {
		city := t.TempDir() // no marker
		var applied []appliedPlan
		journal := newResidenceApplyLeg("journal", 0, &applied)
		legacy := newResidenceApplyLeg("legacy", 1000, &applied)
		router := newResidenceRoutingGraphStoreForCity(journal, legacy, city)
		handle, _ := router.GraphApplyHandle()
		if _, err := handle.ApplyGraphPlan(context.Background(), molPlan()); err != nil {
			t.Fatalf("unarmed apply: %v", err)
		}
		if len(applied) != 1 || applied[0].leg != "legacy" {
			t.Fatalf("unarmed un-anchored root routed to %+v, want legacy leg", applied)
		}
	})
}

// TestBornJournalRootDrainsThroughControlFrontier is the P3.3 end-to-end proof of
// what is provable now: a root minted journal-resident under the cutover marker
// gets a gcg-j* id, its control child is served by the P2.2 ControlFrontier under
// serve mode, and the control dispatcher's store selector (controlStoreForBead)
// resolves that child to the journal leg. Agent do/claim over the served bead is
// P4 (worker journal beads are worker-invisible until then).
func TestBornJournalRootDrainsThroughControlFrontier(t *testing.T) {
	city := t.TempDir()
	if err := migrateGraphJournalInit(city); err != nil {
		t.Fatalf("init: %v", err)
	}
	if err := migrateGraphJournalArmCutover(city); err != nil {
		t.Fatalf("arm: %v", err)
	}

	// Build the router over the SAME on-disk journal that controlStoreForBead's
	// one-shot cache opens, so a bead minted here is visible to that selector.
	journal := cachedCityGraphJournal(city)
	if journal == nil {
		t.Fatal("opted city returned a nil journal leg")
	}
	legacy := newResidenceLegStore("legacy", 1000)
	router := newResidenceRoutingGraphStoreForCity(journal, legacy, city)

	root, err := router.Create(beads.Bead{Title: "born-journal control root"})
	if err != nil {
		t.Fatalf("create born-journal root: %v", err)
	}
	if !strings.HasPrefix(root.ID, "gcg-j") {
		t.Fatalf("born-journal root id %q lacks the journal mint shape gcg-j*", root.ID)
	}
	// A control step under the root, addressed to the control dispatcher.
	child, err := router.Create(beads.Bead{
		Title:    "control step",
		ParentID: root.ID,
		Assignee: config.ControlDispatcherAgentName,
	})
	if err != nil {
		t.Fatalf("create control child: %v", err)
	}
	if !strings.HasPrefix(child.ID, "gcg-j") {
		t.Fatalf("control child id %q is not journal-resident (co-residence broke)", child.ID)
	}

	// ControlFrontier (the P2.2 serve source) returns the journal-resident child.
	frontier, ok := beads.ControlFrontierStoreFor(journal)
	if !ok {
		t.Fatal("journal leg does not expose the ControlFrontier capability")
	}
	params := controlFrontierInputs(
		config.Agent{Name: config.ControlDispatcherAgentName},
		config.BeadsConfig{}, "", func(string) string { return "" })
	rows, err := frontier.ControlFrontier(context.Background(), params)
	if err != nil {
		t.Fatalf("ControlFrontier: %v", err)
	}
	if !frontierContains(rows, child.ID) {
		t.Fatalf("ControlFrontier did not serve the born-journal control child %q: %v", child.ID, beadIDs(rows))
	}

	// Under serve mode (marker present, env unset), the dispatcher's store selector
	// routes the child to the journal leg — the leg that actually holds it.
	setFrontierEnv(t, "", false)
	if graphFrontierModeForCity(city) != frontierModeServe {
		t.Fatal("armed city should resolve to serve mode")
	}
	store, err := controlStoreForBead(city, city, nil, child.ID)
	if err != nil {
		t.Fatalf("controlStoreForBead: %v", err)
	}
	if _, err := store.Get(child.ID); err != nil {
		t.Fatalf("control store for a born-journal bead cannot read it back: %v", err)
	}
}

func frontierContains(rows []beads.Bead, id string) bool {
	for _, b := range rows {
		if b.ID == id {
			return true
		}
	}
	return false
}
