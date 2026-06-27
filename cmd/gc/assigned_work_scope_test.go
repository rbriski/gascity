package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/agentutil"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

func TestFilterAssignedWorkBeadsForSessionWakeKeepsOnlyReachableAssigneeSources(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "worker",
			Dir:      "riga",
			Mode:     "on_demand",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":                  "riga/worker",
			"session_name":              "worker-session",
			"configured_named_identity": "riga/worker",
		},
	}}
	work := []beads.Bead{
		{ID: "city-named", Status: "open", Assignee: "riga/worker"},
		{ID: "rig-named", Status: "open", Assignee: "riga/worker"},
		{ID: "city-session", Status: "in_progress", Assignee: "session-1"},
		{ID: "rig-session", Status: "in_progress", Assignee: "session-1"},
	}
	storeRefs := []string{"", "riga", "", "riga"}

	got := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, sessions, work, storeRefs)

	if len(got) != 2 {
		t.Fatalf("filtered work length = %d, want 2: %#v", len(got), got)
	}
	if got[0].ID != "rig-named" || got[1].ID != "rig-session" {
		t.Fatalf("filtered work IDs = [%s %s], want [rig-named rig-session]", got[0].ID, got[1].ID)
	}
}

func TestFilterAssignedWorkBeadsForSessionWakeCityScopedAgentIsCrossStoreEligible(t *testing.T) {
	// vp-kvp: a city-scoped singleton legitimately serves per-rig routed work.
	// Its assigned work may live in ANY store, so reachability must federate
	// across stores — gating it to its own configured rig is the cross-store
	// dead-drop this fixes. Rig-scoped agents stay single-store (unchanged).
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:  "auditor",
			Scope: "city",
		}},
		NamedSessions: []config.NamedSession{{
			Template: "auditor",
			Scope:    "city",
			Mode:     "on_demand",
		}},
	}
	identity := cfg.NamedSessions[0].QualifiedName()
	work := []beads.Bead{
		{ID: "city-work", Status: "open", Assignee: identity},
		{ID: "rig-work", Status: "open", Assignee: identity},
	}
	storeRefs := []string{"", "riga"} // city store + rig store

	got := filterAssignedWorkBeadsForSessionWake(cfg, cityPath, nil, work, storeRefs)

	if len(got) != 2 {
		t.Fatalf("city-scoped %q must be reachable from BOTH stores; got %d: %#v", identity, len(got), got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsDirectAssigneeAfterTemplateFallback(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", sessions, work, []string{""})

	if len(got) != 1 || got[0].ID != "direct-assigned" {
		t.Fatalf("filtered work = %#v, want direct-assigned work preserved through template fallback", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsLegacyWorkflowRunTarget(t *testing.T) {
	cfg := &config.City{
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	work := []beads.Bead{{
		ID:       "legacy-workflow-root",
		Status:   "in_progress",
		Assignee: "worker-dead",
		Metadata: map[string]string{
			"gc.kind":       "workflow",
			"gc.run_target": "worker",
		},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, "", nil, work, []string{""})

	if len(got) != 1 || got[0].ID != "legacy-workflow-root" {
		t.Fatalf("filtered work = %#v, want legacy workflow root preserved through run_target fallback", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandKeepsPersistedBoundRoute(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "gascity-packs")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "gascity-packs", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "implementation-worker",
			Dir:  "gascity-packs",
		}},
	}
	sessionName := "gc__implementation-worker-mc-xbvk5"
	sessions := []beads.Bead{{
		ID:     "mc-xbvk5",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "gascity-packs/gc.implementation-worker",
			"session_name": sessionName,
		},
	}}
	work := []beads.Bead{{
		ID:       "gp-qx0o",
		Status:   "in_progress",
		Assignee: sessionName,
		Metadata: map[string]string{
			"gc.routed_to": "gascity-packs/gc.implementation-worker",
		},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessions, work, []string{"gascity-packs"})

	if len(got) != 1 || got[0].ID != "gp-qx0o" {
		t.Fatalf("filtered work = %#v, want persisted bound route preserved", got)
	}
}

func TestFilterAssignedWorkBeadsForPoolDemandDropsDirectAssigneeFromUnreachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
		}},
	}
	sessions := []beads.Bead{{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "worker",
			"session_name": "worker-session",
		},
	}}
	work := []beads.Bead{{
		ID:       "rig-direct-assigned",
		Status:   "in_progress",
		Assignee: "session-1",
		Metadata: map[string]string{},
	}}

	got := filterAssignedWorkBeadsForPoolDemand(cfg, cityPath, sessions, work, []string{"riga"})

	if len(got) != 0 {
		t.Fatalf("filtered work = %#v, want unreachable rig-store direct assignment dropped", got)
	}
}

// TestSessionHasOpenAssignedWorkUsesOnlyReachableStore covers the IDENTITY-PHASE
// (default Dolt city, no graph-only capability) fallback of the close/recycle
// check: a rig-scoped session reaches only its one rig store, so city-store work
// does not count while rig-store work does. Under graph_store=sqlite the check
// instead consults the graph backend alone — see
// TestSessionHasOpenAssignedWorkGraphOnlyMatchesWorkerExecutionScope.
func TestSessionHasOpenAssignedWorkUsesOnlyReachableStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	if _, err := cityStore.Create(beads.Bead{
		ID:       "city-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create city work: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if has {
		t.Fatal("city-store assigned work should not count for a rig-scoped session in the identity phase")
	}

	if _, err := rigStore.Create(beads.Bead{
		ID:       "rig-work",
		Type:     "task",
		Status:   "open",
		Assignee: session.ID,
	}); err != nil {
		t.Fatalf("Create rig work: %v", err)
	}
	has, err = sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("rig-store assigned work should count for a rig-scoped session")
	}
}

// TestSessionAssignedWorkGuardsFederateForCityScopedSession is the cross-store
// regression for the reconciler guard path. A city-scoped (cross-store-eligible)
// session legitimately owns rig-store-routed work (vp-kvp), so every reachable-store
// guard the drain/close/recycle/stranded paths consult — the open-work check, the
// awake check, the stranded-bead lookup, and the stranded-work collector — must
// federate across the city store AND every rig store for it, exactly like
// openSessionReachableStoreRef's cross-store wildcard. Before the fix these guards
// resolved the city-scoped session to a single configured store and missed its
// rig-store work, so a live holder could be closed/drained/recycled or
// under-reported (#3453 re-regression). Rig-scoped sessions stay single-store in
// the identity phase (covered by TestSessionHasOpenAssignedWorkUsesOnlyReachableStore);
// under graph_store=sqlite the drain/recycle/wake checks are graph-only, matching
// the worker's execution scope (covered by
// TestSessionHasOpenAssignedWorkGraphOnlyMatchesWorkerExecutionScope).
func TestSessionAssignedWorkGuardsFederateForCityScopedSession(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name:  "auditor",
			Scope: "city",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{"riga": rigStore}
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "auditor",
			"session_name": "auditor-session",
		},
	}
	// Work lives ONLY in the rig store, assigned to the city-scoped session.
	rigWork, err := rigStore.Create(beads.Bead{
		Type:     "task",
		Status:   "in_progress",
		Assignee: session.ID,
	})
	if err != nil {
		t.Fatalf("Create rig work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(rigWork.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark rig work in progress: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, rigStores, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("city-scoped session must see its rig-store work across stores (close/drain guard)")
	}

	awake, err := sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, cityStore, rigStores, session)
	if err != nil {
		t.Fatalf("sessionHasAwakeAssignedWorkForReachableStore: %v", err)
	}
	if !awake {
		t.Fatal("city-scoped session's in-progress rig-store work must keep it awake (recycle guard)")
	}

	bead, found, err := firstOpenAssignedWorkBeadForReachableStore(cityPath, cfg, cityStore, rigStores, session)
	if err != nil {
		t.Fatalf("firstOpenAssignedWorkBeadForReachableStore: %v", err)
	}
	if !found || bead.ID != rigWork.ID {
		t.Fatalf("stranded-bead lookup must find rig-store work for a city-scoped session; found=%v bead=%q want=%q", found, bead.ID, rigWork.ID)
	}

	stranded, err := collectSessionAssignedWork(cityPath, cfg, cityStore, rigStores, session)
	if err != nil {
		t.Fatalf("collectSessionAssignedWork: %v", err)
	}
	if len(stranded) != 1 || stranded[0].bead.ID != rigWork.ID {
		t.Fatalf("stranded-work collector must include rig-store work for a city-scoped session; got %#v", stranded)
	}
}

func TestSessionHasOpenAssignedWorkMatchesConfiguredNamedSessionRuntimeFallback(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "worker",
			BindingName: "pack",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "worker",
			BindingName: "pack",
			Mode:        "on_demand",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "pack.worker")
	store := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":                   "pack.worker",
			"session_name":               sessionName,
			namedSessionMetadataKey:      "true",
			namedSessionModeMetadata:     "on_demand",
			namedSessionIdentityMetadata: "",
		},
	}
	if _, err := store.Create(beads.Bead{
		ID:       "named-work",
		Type:     "task",
		Status:   "open",
		Assignee: "pack.worker",
	}); err != nil {
		t.Fatalf("Create named work: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore("", cfg, store, nil, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("configured named-session runtime-name fallback assignment should count as open assigned work")
	}
}

func TestSessionAssignmentIdentifiersForConfigConfiguredNamedSessionFallbackIsConservative(t *testing.T) {
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:        "worker",
			BindingName: "pack",
		}},
		NamedSessions: []config.NamedSession{{
			Template:    "worker",
			BindingName: "pack",
			Mode:        "on_demand",
		}},
	}
	sessionName := config.NamedSessionRuntimeName(cfg.EffectiveCityName(), cfg.Workspace, "pack.worker")

	tests := []struct {
		name    string
		session beads.Bead
	}{
		{
			name: "identity metadata already present",
			session: beads.Bead{
				ID: "session-with-identity",
				Metadata: map[string]string{
					"template":                   "pack.worker",
					"session_name":               sessionName,
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "pack.other",
				},
			},
		},
		{
			name: "template mismatch",
			session: beads.Bead{
				ID: "session-template-mismatch",
				Metadata: map[string]string{
					"template":                   "pack.other",
					"session_name":               sessionName,
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "",
				},
			},
		},
		{
			name: "runtime name mismatch",
			session: beads.Bead{
				ID: "session-runtime-mismatch",
				Metadata: map[string]string{
					"template":                   "pack.worker",
					"session_name":               "different-session",
					namedSessionMetadataKey:      "true",
					namedSessionIdentityMetadata: "",
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			for _, identifier := range sessionAssignmentIdentifiersForConfig(tt.session, cfg) {
				if identifier == "pack.worker" {
					t.Fatalf("identifiers include configured identity %q for conservative mismatch case: %v", identifier, sessionAssignmentIdentifiersForConfig(tt.session, cfg))
				}
			}
		})
	}
}

func TestAgentReachesWorkflowStore(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "alpha")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	hqAgent := &config.Agent{Name: "mayor"}
	rigAgent := &config.Agent{Name: "polecat", Dir: "alpha"}

	cases := []struct {
		name     string
		storeRef string
		agent    *config.Agent
		want     bool
	}{
		{name: "hq agent reaches city store", storeRef: "city:test-city", agent: hqAgent, want: true},
		{name: "hq agent cannot reach rig store", storeRef: "rig:alpha", agent: hqAgent, want: false},
		{name: "rig agent reaches own rig store", storeRef: "rig:alpha", agent: rigAgent, want: true},
		{name: "rig agent cannot reach city store", storeRef: "city:test-city", agent: rigAgent, want: false},
		{name: "rig agent cannot reach a different rig", storeRef: "rig:beta", agent: rigAgent, want: false},
		{name: "empty storeRef is unreachable for rig agent", storeRef: "", agent: rigAgent, want: false},
		{name: "empty storeRef is unreachable for hq agent", storeRef: "", agent: hqAgent, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := agentutil.AgentReachesWorkflowStore(tc.storeRef, tc.agent, cityPath, cfg); got != tc.want {
				t.Fatalf("AgentReachesWorkflowStore(%q, %q) = %v, want %v", tc.storeRef, tc.agent.Name, got, tc.want)
			}
		})
	}

	if !agentutil.AgentReachesWorkflowStore("city:test-city", nil, cityPath, cfg) {
		t.Fatal("nil agent should permissively reach any store")
	}
	if !agentutil.AgentReachesWorkflowStore("rig:alpha", rigAgent, cityPath, nil) {
		t.Fatal("nil cfg should permissively reach any store")
	}
}

func TestAgentReachableStoreLabel(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "alpha")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "alpha", Path: rigPath}},
	}
	hqAgent := &config.Agent{Name: "mayor"}
	rigAgent := &config.Agent{Name: "polecat", Dir: "alpha"}

	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "test-city", cfg); got != "city:test-city" {
		t.Errorf("hq agent label = %q, want city:test-city", got)
	}
	if got := agentutil.AgentReachableStoreLabel(rigAgent, cityPath, "test-city", cfg); got != "rig:alpha" {
		t.Errorf("rig agent label = %q, want rig:alpha", got)
	}
	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "", cfg); got != "city:city" {
		t.Errorf("hq agent label with empty cityName = %q, want city:city", got)
	}
	if got := agentutil.AgentReachableStoreLabel(nil, cityPath, "test-city", cfg); got != "" {
		t.Errorf("nil agent label = %q, want empty", got)
	}
	if got := agentutil.AgentReachableStoreLabel(hqAgent, cityPath, "test-city", nil); got != "" {
		t.Errorf("nil cfg label = %q, want empty", got)
	}
}

func TestSessionHasOpenAssignedWorkIncludesReachableAssignedWisp(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	cfg := &config.City{
		Rigs: []config.Rig{{Name: "riga", Path: rigPath}},
		Agents: []config.Agent{{
			Name: "worker",
			Dir:  "riga",
		}},
	}
	cityStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	wisp, err := rigStore.Create(beads.Bead{
		ID:        "rig-wisp-work",
		Title:     "active workflow step",
		Type:      "task",
		Status:    "in_progress",
		Assignee:  session.Metadata["session_name"],
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create rig wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := rigStore.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark rig wisp in progress: %v", err)
	}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, cityStore, map[string]beads.Store{"riga": rigStore}, session)
	if err != nil {
		t.Fatalf("sessionHasOpenAssignedWorkForReachableStore: %v", err)
	}
	if !has {
		t.Fatal("reachable assigned wisp work should count before closing a session")
	}
}

func TestFirstOpenAssignedWorkBeadIncludesAssignedWisp(t *testing.T) {
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "active workflow step",
		Type:      "task",
		Assignee:  "worker-session",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark wisp in progress: %v", err)
	}

	got, found, err := firstOpenAssignedWorkBeadInStoreByIdentifiers(store, []string{"worker-session"})
	if err != nil {
		t.Fatalf("firstOpenAssignedWorkBeadInStoreByIdentifiers: %v", err)
	}
	if !found {
		t.Fatal("assigned wisp work should be found for session diagnostics")
	}
	if got.ID != wisp.ID {
		t.Fatalf("first assigned work ID = %q, want %q", got.ID, wisp.ID)
	}
}

func TestResolveTaskWorkDirIncludesAssignedWisp(t *testing.T) {
	workDir := t.TempDir()
	store := beads.NewMemStore()
	wisp, err := store.Create(beads.Bead{
		Title:     "active workflow step",
		Type:      "task",
		Assignee:  "worker-session",
		Metadata:  map[string]string{"work_dir": workDir},
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create wisp work: %v", err)
	}
	inProgress := "in_progress"
	if err := store.Update(wisp.ID, beads.UpdateOpts{Status: &inProgress}); err != nil {
		t.Fatalf("mark wisp in progress: %v", err)
	}

	if got := resolveTaskWorkDir(store, "worker-session"); got != workDir {
		t.Fatalf("resolveTaskWorkDir = %q, want assigned wisp work_dir %q", got, workDir)
	}
}

// TestSessionHasOpenAssignedWorkGraphOnlyMatchesWorkerExecutionScope: under
// graph_store=sqlite the close/recycle check consults the graph backend ALONE,
// mirroring the worker's execution loop (a worker executes only graph nodes).
// Graph work assigned to the worker counts — so it is not closed out from under it
// (the strand). Dolt/rig work the graph-only worker can never execute does NOT
// count — counting it would pin the worker alive for unrunnable work (the livelock:
// kept alive, never works).
func TestSessionHasOpenAssignedWorkGraphOnlyMatchesWorkerExecutionScope(t *testing.T) {
	cityPath := t.TempDir()
	rigPath := filepath.Join(cityPath, "riga")
	// Class-aware wiring (post-coordrouter): the graph step physically lives in the
	// dedicated graph store at .gc/beads.sqlite, resolved via resolveGraphStore from
	// the relocated graph class config — not in a capability-bearing store double.
	cfg := graphClassSQLiteCfg()
	cfg.Rigs = []config.Rig{{Name: "riga", Path: rigPath}}
	cfg.Agents = []config.Agent{{Name: "worker", Dir: "riga"}}
	session := beads.Bead{
		ID:     "session-1",
		Type:   sessionBeadType,
		Status: "open",
		Metadata: map[string]string{
			"template":     "riga/worker",
			"session_name": "worker-session",
		},
	}
	graphStore, ok := openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}
	graphStep, err := graphStore.Create(beads.Bead{Title: "graph step", Assignee: session.ID})
	if err != nil {
		t.Fatalf("create graph step: %v", err)
	}

	// The primary store is the work leg; a graph-only worker never executes Dolt/rig
	// work, so the recycle check must ignore rig work assigned to the same session.
	workStore := beads.NewMemStore()
	rigStore := beads.NewMemStore()
	if _, err := rigStore.Create(beads.Bead{ID: "rig-work", Type: "task", Status: "open", Assignee: session.ID}); err != nil {
		t.Fatalf("create rig work: %v", err)
	}
	rigStores := map[string]beads.Store{"riga": rigStore}

	has, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, workStore, rigStores, session)
	if err != nil {
		t.Fatalf("check with graph work: %v", err)
	}
	if !has {
		t.Fatal("graph-store assigned work must count — closing out from under it is the strand bug")
	}

	// Close the graph step; only the unrunnable Dolt/rig work remains.
	closed := "closed"
	if err := graphStore.Update(graphStep.ID, beads.UpdateOpts{Status: &closed}); err != nil {
		t.Fatalf("close graph step: %v", err)
	}
	has, err = sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, workStore, rigStores, session)
	if err != nil {
		t.Fatalf("check without graph work: %v", err)
	}
	if has {
		t.Fatal("Dolt/rig work a graph-only worker can't execute must NOT count — counting it livelocks the worker (kept alive, never works)")
	}
}

// graphOnlyProbeSession builds a graph_store=sqlite city + a session and the
// dedicated graph store at <cityPath>/.gc/beads.sqlite, shared by the graph-only
// probe regressions below.
func graphOnlyProbeSession(t *testing.T) (cityPath string, cfg *config.City, graph beads.Store, session beads.Bead) {
	t.Helper()
	cityPath = t.TempDir()
	cfg = graphClassSQLiteCfg()
	cfg.Rigs = []config.Rig{{Name: "riga", Path: filepath.Join(cityPath, "riga")}}
	cfg.Agents = []config.Agent{{Name: "worker", Dir: "riga"}}
	session = beads.Bead{
		ID:       "session-1",
		Type:     sessionBeadType,
		Status:   "open",
		Metadata: map[string]string{"template": "riga/worker", "session_name": "worker-session"},
	}
	var ok bool
	graph, ok = openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}
	return cityPath, cfg, graph, session
}

// TestSessionGraphOnlyProbesCoverEphemeralWisp pins the tier coverage of the
// graph-only close/wake probes: an EPHEMERAL (wisp-tier) ready graph step assigned to
// the session must count for BOTH the open-work close gate and the awake/wake check.
// The default ListQuery/ReadyQuery TierMode (TierIssues) filters Ephemeral rows, so
// without TierBoth the session would be closed/recycled out from under a routed
// cleanup/finalize wisp — stranding it. (Regression for the GB tier blocker.)
func TestSessionGraphOnlyProbesCoverEphemeralWisp(t *testing.T) {
	cityPath, cfg, graph, session := graphOnlyProbeSession(t)

	step, err := graph.Create(beads.Bead{Title: "ephemeral cleanup wisp", Ephemeral: true})
	if err != nil {
		t.Fatalf("create wisp: %v", err)
	}
	assignee := session.ID
	if err := graph.Update(step.ID, beads.UpdateOpts{Assignee: &assignee}); err != nil {
		t.Fatalf("assign wisp: %v", err)
	}

	hasOpen, err := sessionHasOpenAssignedWorkForReachableStore(cityPath, cfg, beads.NewMemStore(), nil, session)
	if err != nil {
		t.Fatalf("open-work check: %v", err)
	}
	if !hasOpen {
		t.Fatal("an assigned ready ephemeral-wisp graph step must count as open assigned work (TierBoth) — else the close gate strands it")
	}

	hasAwake, err := sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, beads.NewMemStore(), nil, session)
	if err != nil {
		t.Fatalf("awake check: %v", err)
	}
	if !hasAwake {
		t.Fatal("a ready ephemeral-wisp graph step must keep the session awake (graph.Ready TierBoth) — else drain-cancel/recycle strands it")
	}
}

// TestSessionGraphOnlyAwakeCoversInProgress pins the in_progress arm the OLD ready-only
// path missed (SQLiteStore.Ready requires status=open, so an actively-running
// in_progress graph node was invisible and the worker could be recycled mid-execution).
// graphStoreHasAwakeAssignedWork now runs an in_progress List on top of Ready.
func TestSessionGraphOnlyAwakeCoversInProgress(t *testing.T) {
	cityPath, cfg, graph, session := graphOnlyProbeSession(t)

	step, err := graph.Create(beads.Bead{Title: "running graph node"})
	if err != nil {
		t.Fatalf("create step: %v", err)
	}
	inProgress, assignee := "in_progress", session.ID
	if err := graph.Update(step.ID, beads.UpdateOpts{Status: &inProgress, Assignee: &assignee}); err != nil {
		t.Fatalf("start step: %v", err)
	}

	hasAwake, err := sessionHasAwakeAssignedWorkForReachableStore(cityPath, cfg, beads.NewMemStore(), nil, session)
	if err != nil {
		t.Fatalf("awake check: %v", err)
	}
	if !hasAwake {
		t.Fatal("an in_progress graph step must keep the session awake — recycling it mid-execution is the strand")
	}
}
