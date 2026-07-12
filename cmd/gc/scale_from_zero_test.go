package main

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/runtime"
)

type localMockProvider struct {
	runtime.Provider
}

func (m *localMockProvider) IsRunning(_ string) bool { return false }

func TestBuildDesiredState_ScaleFromZero_CrossRig(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Setup config: one pool agent on a rig, min=0.
	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Route a bead to the planner in the CITY store.
	// Native check for rig-A would miss this if not aggregated.
	_, err := cityStore.Create(beads.Bead{
		ID:     "bead-1",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": qualifiedName,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	// Call buildDesiredStateWithSessionBeads.
	// It should:
	// 1. Detect that 'planner' is cold (no sessions in city or rig stores).
	// 2. Run a native probe across ALL stores (city + rig-A).
	// 3. Find bead-1 in the city store.
	// 4. Set demand to 1 (max of custom 0 and native 1).
	// 5. Materialize a new session bead.
	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	demand := result.ScaleCheckCounts[qualifiedName]
	if demand != 1 {
		t.Errorf("expected demand 1, got %d", demand)
	}

	if len(result.State) != 1 {
		t.Errorf("expected 1 desired session, got %d", len(result.State))
	}
}

func TestBuildDesiredStateReadySnapshotsAcceptUncomparableStores(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	maxSess := 2
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "planner",
			Dir:               "rig-A",
			Provider:          "mock",
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		}},
		Rigs: []config.Rig{{Name: "rig-A", Path: rigPath}},
		Providers: map[string]config.ProviderSpec{
			"mock": {Command: "true"},
		},
	}
	cityBacking := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	rigBacking := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	if _, err := rigBacking.Create(beads.Bead{
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "rig-A/planner",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cityStore := uncomparableReadyStore{Store: cityBacking, marker: []byte("city")}
	rigStore := uncomparableReadyStore{Store: rigBacking, marker: []byte("rig")}

	defer func() {
		if recovered := recover(); recovered != nil {
			t.Fatalf("full desired-state pass compared or map-keyed an uncomparable Store: %v", recovered)
		}
	}()
	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, map[string]beads.Store{"rig-A": rigStore},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	if got := result.ScaleCheckCounts["rig-A/planner"]; got != 1 {
		t.Fatalf("scale demand = %d, want 1", got)
	}
	if got := len(cityBacking.readyQueries); got != 1 {
		t.Fatalf("untouched city Ready reads = %d, want 1", got)
	}
	if got := len(rigBacking.readyQueries); got != 1 {
		t.Fatalf("untouched rig Ready reads = %d, want 1", got)
	}
}

func TestBuildDesiredStateReadySnapshotsShareSameUncomparableStoreValue(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "rigs", "rig-A")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	maxSess, minSess := 2, 0
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "planner",
			Dir:               "rig-A",
			Provider:          "mock",
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		}},
		Rigs:      []config.Rig{{Name: "rig-A", Path: rigPath}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	if _, err := backing.Create(beads.Bead{
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "rig-A/planner",
		},
	}); err != nil {
		t.Fatal(err)
	}
	aliased := uncomparableReadyStore{Store: backing, marker: []byte("same-value")}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		aliased, map[string]beads.Store{"rig-A": aliased},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	if got := result.ScaleCheckCounts["rig-A/planner"]; got != 1 {
		t.Fatalf("scale demand = %d, want one physical bead counted once", got)
	}
	if got := len(backing.readyQueries); got != 1 {
		t.Fatalf("aliased uncomparable store Ready reads = %d, want one logical generation", got)
	}
}

func TestBuildDesiredStateReadySnapshotsSharePointerAliasedRigStores(t *testing.T) {
	tmpDir := t.TempDir()
	maxSess, minSess := 2, 0
	cfg := &config.City{Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}}}
	for _, rigName := range []string{"rig-A", "rig-B"} {
		rigPath := filepath.Join(tmpDir, "rigs", rigName)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: rigName, Path: rigPath})
		cfg.Agents = append(cfg.Agents, config.Agent{
			Name:              "planner",
			Dir:               rigName,
			Provider:          "mock",
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		})
	}
	shared := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	for _, target := range []string{"rig-A/planner", "rig-B/planner"} {
		if _, err := shared.Create(beads.Bead{
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": target},
		}); err != nil {
			t.Fatal(err)
		}
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		beads.NewMemStore(), map[string]beads.Store{"rig-A": shared, "rig-B": shared},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	for _, target := range []string{"rig-A/planner", "rig-B/planner"} {
		if got := result.ScaleCheckCounts[target]; got != 1 {
			t.Errorf("scale demand for %q = %d, want 1", target, got)
		}
	}
	if got := len(shared.readyQueries); got != 1 {
		t.Fatalf("pointer-aliased rig Ready reads = %d, want one logical generation", got)
	}
}

func TestBuildDesiredStateCustomScaleCheckSharedRigStoreKeepsHomeProvenance(t *testing.T) {
	tmpDir := t.TempDir()
	maxSess, minSess := 1, 0
	cfg := &config.City{Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}}}
	shared := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	wantIDs := make(map[string]string)
	for _, rigName := range []string{"rig-A", "rig-B"} {
		rigPath := filepath.Join(tmpDir, "rigs", rigName)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatal(err)
		}
		template := rigName + "/planner"
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: rigName, Path: rigPath})
		cfg.Agents = append(cfg.Agents, config.Agent{
			Name:              "planner",
			Dir:               rigName,
			Provider:          "mock",
			ScaleCheck:        "printf 0",
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		})
		created, err := shared.Create(beads.Bead{
			Title:    template,
			Type:     "task",
			Status:   "open",
			Metadata: map[string]string{"gc.routed_to": template},
		})
		if err != nil {
			t.Fatal(err)
		}
		wantIDs[template] = created.ID
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		beads.NewMemStore(), map[string]beads.Store{"rig-A": shared, "rig-B": shared},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	seen := make(map[string]bool)
	for _, desired := range result.State {
		template := desired.TemplateName
		wantID, ok := wantIDs[template]
		if !ok {
			continue
		}
		seen[template] = true
		if got := desired.Env["GC_TRIGGER_BEAD_ID"]; got != wantID {
			t.Errorf("%s trigger bead = %q, want %q", template, got, wantID)
		}
		wantRef := strings.Split(template, "/")[0]
		if got := desired.Env["GC_TRIGGER_BEAD_STORE_REF"]; got != wantRef {
			t.Errorf("%s trigger store ref = %q, want home scope %q", template, got, wantRef)
		}
	}
	for template := range wantIDs {
		if !seen[template] {
			t.Errorf("no desired session created for %s; state=%v", template, result.State)
		}
	}
	if got := len(shared.readyQueries); got != 1 {
		t.Fatalf("shared pointer Ready reads = %d, want one coherent generation", got)
	}
}

func TestBuildDesiredStateOpenRouteMigrationListErrorDoesNotSuppressDrains(t *testing.T) {
	tmpDir := t.TempDir()
	minSess, maxSess := 0, 1
	cfg := &config.City{
		Agents: []config.Agent{{
			Name:              "worker",
			Provider:          "mock",
			MinActiveSessions: &minSess,
			MaxActiveSessions: &maxSess,
		}},
		Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}},
	}
	wantErr := errors.New("migration-only open list unavailable")
	store := &openListAfterFirstErrorStore{MemStore: beads.NewMemStore(), err: wantErr}
	var stderr strings.Builder

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		store, nil, newSessionBeadSnapshot(nil), nil, &stderr,
	)
	if result.StoreQueryPartial {
		t.Fatalf("StoreQueryPartial = true, want legacy drain behavior for migration-only List error; stderr=%s", stderr.String())
	}
	if !strings.Contains(stderr.String(), wantErr.Error()) {
		t.Fatalf("stderr = %q, want migration List diagnostic", stderr.String())
	}
}

func TestBuildDesiredStateReadyStoreScopesTreatRigNamesAsOpaque(t *testing.T) {
	tmpDir := t.TempDir()
	maxSess := 1
	minSess := 0
	type rigFixture struct {
		name      string
		agent     string
		legacy    string
		canonical string
		backing   *readyQueryRecordingStore
		store     *canonicalizationAttemptStore
		beadID    string
	}
	fixtures := []*rigFixture{
		{name: "city", agent: "worker-city", legacy: "city/core.worker-city", canonical: "city/worker-city"},
		{name: "foo", agent: "worker-foo", legacy: "foo/core.worker-foo", canonical: "foo/worker-foo"},
		{name: "rig:foo", agent: "worker-prefixed", legacy: "rig:foo/core.worker-prefixed", canonical: "rig:foo/worker-prefixed"},
	}
	cfg := &config.City{Providers: map[string]config.ProviderSpec{"mock": {Command: "true"}}}
	rigStores := make(map[string]beads.Store, len(fixtures))
	for _, fixture := range fixtures {
		path := filepath.Join(tmpDir, "rigs", strings.ReplaceAll(fixture.name, ":", "_"))
		if err := os.MkdirAll(path, 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.Rigs = append(cfg.Rigs, config.Rig{Name: fixture.name, Path: path})
		cfg.Agents = append(cfg.Agents, config.Agent{
			Name:              fixture.agent,
			Dir:               fixture.name,
			Provider:          "mock",
			MinActiveSessions: &minSess,
			MaxActiveSessions: &maxSess,
		})
		fixture.backing = &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
		created, err := fixture.backing.Create(beads.Bead{
			Title:  fixture.name + " legacy work",
			Type:   "task",
			Status: "open",
			Metadata: map[string]string{
				"gc.routed_to": fixture.legacy,
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		fixture.beadID = created.ID
		fixture.store = &canonicalizationAttemptStore{readyQueryRecordingStore: fixture.backing, commit: true}
		rigStores[fixture.name] = fixture.store
	}
	cityBacking := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	if _, err := cityBacking.Create(beads.Bead{
		Title:  "city-scope unrelated work",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.routed_to": "unrelated",
		},
	}); err != nil {
		t.Fatal(err)
	}
	cityStore := &canonicalizationAttemptStore{readyQueryRecordingStore: cityBacking, commit: true}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	for _, fixture := range fixtures {
		if got := result.ScaleCheckCounts[fixture.canonical]; got != 1 {
			t.Errorf("scale demand for %q = %d, want 1", fixture.canonical, got)
		}
		persisted, err := fixture.backing.Get(fixture.beadID)
		if err != nil {
			t.Fatal(err)
		}
		if route := persisted.Metadata["gc.routed_to"]; route != fixture.canonical {
			t.Errorf("%q persisted route = %q, want %q", fixture.name, route, fixture.canonical)
		}
		if got := fixture.store.updates.Load(); got != 1 {
			t.Errorf("%q Update calls = %d, want exactly its own repair", fixture.name, got)
		}
		if got := len(fixture.backing.readyQueries); got != 2 {
			t.Errorf("%q Ready calls = %d, want pre+post", fixture.name, got)
		}
	}
	if got := cityStore.updates.Load(); got != 0 {
		t.Errorf("city-scope Update calls = %d, want 0 (rig named city must not capture city writer)", got)
	}
	if got := len(cityBacking.readyQueries); got != 1 {
		t.Errorf("untouched city-scope Ready calls = %d, want 1", got)
	}
}

func TestBuildDesiredStateControlDemandUsesPostCanonicalizationOpenSnapshot(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	maxSess := 1
	minSess := 0
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			Dir:               "rig-A",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		}},
		Rigs: []config.Rig{{Name: "rig-A", Path: rigPath}},
	}
	backing := beads.NewMemStore()
	blocker, err := backing.Create(beads.Bead{Title: "unfinished worker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	control, err := backing.Create(beads.Bead{
		Title:  "blocked retry",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "retry",
			"gc.routed_to": "rig-A/core.control-dispatcher",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.DepAdd(control.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	store := beads.NewCachingStoreForTest(backing, nil)
	if err := store.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		store, map[string]beads.Store{"rig-A": store},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	const canonical = "rig-A/control-dispatcher"
	if got := result.ScaleCheckCounts[canonical]; got != 1 {
		t.Fatalf("control demand = %d, want 1 from post-repair blocked open work", got)
	}
	got, err := backing.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route := got.Metadata["gc.routed_to"]; route != canonical {
		t.Fatalf("persisted route = %q, want %q", route, canonical)
	}
}

func TestBuildDesiredStateControlDemandObservesCanonicalizationCommitThenError(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	maxSess := 1
	minSess := 0
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			Dir:               "rig-A",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		}},
		Rigs: []config.Rig{{Name: "rig-A", Path: rigPath}},
	}
	backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	blocker, err := backing.Create(beads.Bead{Title: "unfinished worker", Type: "task", Status: "open"})
	if err != nil {
		t.Fatal(err)
	}
	control, err := backing.Create(beads.Bead{
		Title:  "blocked retry",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.kind":      "retry",
			"gc.routed_to": "rig-A/core.control-dispatcher",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := backing.DepAdd(control.ID, blocker.ID, "blocks"); err != nil {
		t.Fatal(err)
	}
	store := &canonicalizationAttemptStore{
		readyQueryRecordingStore: backing,
		commit:                   true,
		updateErr:                errors.New("write committed but acknowledgement was lost"),
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		beads.NewMemStore(), map[string]beads.Store{"rig-A": store},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	const canonical = "rig-A/control-dispatcher"
	if got := result.ScaleCheckCounts[canonical]; got != 1 {
		t.Fatalf("control demand = %d, want 1 from authoritative post-error open snapshot", got)
	}
	got, err := backing.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if route := got.Metadata["gc.routed_to"]; route != canonical {
		t.Fatalf("persisted route = %q, want %q", route, canonical)
	}
	if got := len(backing.readyQueries); got != 2 {
		t.Fatalf("rig Ready reads = %d, want pre+post = 2", got)
	}
}

func TestBuildDesiredStateControlRouteRepairCommitThenErrorUsesPostGeneration(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := filepath.Join(tmpDir, "rigs", "rig-A")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	maxSess, minSess := 1, 0
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			Dir:               "rig-A",
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		}},
		Rigs: []config.Rig{{Name: "rig-A", Path: rigPath}},
	}
	backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	control, err := backing.Create(beads.Bead{
		Title:  "misrouted finalizer",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "wrong-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:rig-A",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &canonicalizationAttemptStore{
		readyQueryRecordingStore: backing,
		commit:                   true,
		updateErr:                errors.New("route repair committed but acknowledgement was lost"),
	}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		beads.NewMemStore(), map[string]beads.Store{"rig-A": store},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	const wantRoute = "rig-A/core.control-dispatcher"
	if got := result.ScaleCheckCounts[wantRoute]; got != 1 {
		t.Fatalf("control demand = %d, want 1 from authoritative post-error route", got)
	}
	persisted, err := backing.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Metadata[beadmeta.RoutedToMetadataKey]; got != wantRoute {
		t.Fatalf("persisted route = %q, want committed %q", got, wantRoute)
	}
}

func TestBuildDesiredStateControlRouteRepairKeepsOpaqueRigName(t *testing.T) {
	tmpDir := t.TempDir()
	const rigName = "rig:foo"
	rigPath := filepath.Join(tmpDir, "rigs", "rig_foo")
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}
	maxSess, minSess := 1, 0
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Agents: []config.Agent{{
			Name:              config.ControlDispatcherAgentName,
			BindingName:       "core",
			Dir:               rigName,
			StartCommand:      config.ControlDispatcherStartCommandFor("{{.Agent}}"),
			MaxActiveSessions: &maxSess,
			MinActiveSessions: &minSess,
		}},
		Rigs: []config.Rig{{Name: rigName, Path: rigPath}},
	}
	backing := &readyQueryRecordingStore{MemStore: beads.NewMemStore()}
	control, err := backing.Create(beads.Bead{
		Title:  "misrouted opaque-rig finalizer",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflowFinalize,
			beadmeta.RoutedToMetadataKey:     "wrong-dispatcher",
			beadmeta.RootStoreRefMetadataKey: "rig:rig:foo",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := &canonicalizationAttemptStore{readyQueryRecordingStore: backing, commit: true}

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		beads.NewMemStore(), map[string]beads.Store{rigName: store},
		newSessionBeadSnapshot(nil), nil, os.Stderr,
	)
	const wantRoute = "rig:foo/core.control-dispatcher"
	if got := result.ScaleCheckCounts[wantRoute]; got != 1 {
		t.Fatalf("control demand = %d, want 1 for opaque rig owner", got)
	}
	persisted, err := backing.Get(control.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := persisted.Metadata[beadmeta.RoutedToMetadataKey]; got != wantRoute {
		t.Fatalf("persisted route = %q, want %q", got, wantRoute)
	}
}

// TestBuildDesiredState_ScaleFromZero_ClampsWakeDemandToOne proves the cold-pool
// wake probe only wakes the pool from zero (contributes at most 1) and never
// scales to the full routed-bead count. With the clamp removed, the cross-store
// probe would report demand 3 (one per routed bead) instead of 1.
func TestBuildDesiredState_ScaleFromZero_ClampsWakeDemandToOne(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Route THREE beads to the planner in the CITY store. The cross-store cold
	// probe sees all three; the clamp must reduce the wake contribution to 1.
	for _, id := range []string{"bead-0", "bead-1", "bead-2"} {
		if _, err := cityStore.Create(beads.Bead{
			ID:     id,
			Status: "open",
			Type:   "task",
			Metadata: map[string]string{
				"gc.routed_to": qualifiedName,
			},
		}); err != nil {
			t.Fatal(err)
		}
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// Wake-from-zero: demand is clamped to 1 (max of custom 0 and clamped 1),
	// NOT the routed-bead count of 3.
	if demand := result.ScaleCheckCounts[qualifiedName]; demand != 1 {
		t.Errorf("expected wake demand clamped to 1, got %d", demand)
	}
	if len(result.State) != 1 {
		t.Errorf("expected 1 desired session, got %d", len(result.State))
	}
}

func TestBuildDesiredState_ScaleFromZero_IncludesRigSessions(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	// Setup config: one pool agent on rig-A, min=0.
	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0",
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Create a running session bead in the RIG store.
	// City store snapshot will miss this.
	_, err := rigAStore.Create(beads.Bead{
		ID:     "session-1",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     qualifiedName,
			"session_name": "planner-1",
			"state":        "active",
			"pool_slot":    "1",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// Route demand to the city store.
	_, err = cityStore.Create(beads.Bead{
		ID:     "bead-1",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": qualifiedName,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	// Call buildDesiredStateWithSessionBeads.
	// It should:
	// 1. Correctly detect that 'planner' has 1 running session (in rig-A store).
	// 2. NOT treat it as "cold" (isCold = false because runningSessions = 1).
	// 3. Skip the native probe because ScaleCheck is not empty and it's not cold.
	// 4. Use custom check (printf 0) -> demand 0.
	// 5. Resulting demand should be 0.
	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	demand := result.ScaleCheckCounts[qualifiedName]
	if demand != 0 {
		t.Errorf("expected demand 0 (custom check only), got %d", demand)
	}
}

// TestBuildDesiredState_ScaleFromZero_UnqualifiedTemplateDoesNotSuppressCold
// proves the cold detection counts only the agent's qualified template. A stray
// pool session bead carrying the unqualified base name ("planner", e.g. a
// same-base-name pool in another rig or a legacy bead) must NOT count toward
// rig-A/planner's running sessions, so rig-A/planner stays cold and its
// cold-wake probe still fires. With the bare-name match present, the stray bead
// would suppress the probe and demand would be 0.
func TestBuildDesiredState_ScaleFromZero_UnqualifiedTemplateDoesNotSuppressCold(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Stray pool session bead carrying the UNQUALIFIED base name "planner"
	// (not "rig-A/planner"). It must not be attributed to rig-A/planner.
	if _, err := rigAStore.Create(beads.Bead{
		ID:     "stray-session",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "planner",
			"session_name": "planner-1",
			"state":        "active",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Route demand to rig-A/planner in the city store.
	if _, err := cityStore.Create(beads.Bead{
		ID:     "bead-1",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": qualifiedName,
		},
	}); err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// rig-A/planner is genuinely cold (the bare "planner" bead is not its
	// session), so the cold-wake probe fires on the city-routed demand.
	if demand := result.ScaleCheckCounts[qualifiedName]; demand != 1 {
		t.Errorf("expected demand 1 (stray unqualified session must not suppress cold), got %d", demand)
	}
}

// TestBuildDesiredState_ScaleFromZero_LegacyBoundTemplateSuppressesCold proves
// the cold detection counts an adopted session bead persisted under a removed
// binding ("rig-A/gc.planner") as a running session of the current unbound
// rig-A/planner agent. The identities are equivalent after bound→unbound
// migration, so the pool is NOT cold and the cold-wake probe must not fire —
// otherwise every tick over-probes and transiently over-wakes a pool that
// already has a live adopted session. The bare-name distinctness guarantee of
// TestBuildDesiredState_ScaleFromZero_UnqualifiedTemplateDoesNotSuppressCold
// must survive this widening.
func TestBuildDesiredState_ScaleFromZero_LegacyBoundTemplateSuppressesCold(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	qualifiedName := "rig-A/planner"

	// Adopted pool session bead persisted under the removed binding. Its
	// stored template is the legacy bound identity of the SAME agent, so it
	// must count toward rig-A/planner's running sessions.
	if _, err := rigAStore.Create(beads.Bead{
		ID:     "adopted-session",
		Status: "open",
		Type:   sessionBeadType,
		Metadata: map[string]string{
			"template":     "rig-A/gc.planner",
			"session_name": "planner-legacy-1",
			"state":        "active",
			"pool_slot":    "1",
		},
	}); err != nil {
		t.Fatal(err)
	}

	// Route demand to rig-A/planner in the city store.
	if _, err := cityStore.Create(beads.Bead{
		ID:     "bead-1",
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": qualifiedName,
		},
	}); err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // Empty city store snapshot

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// The adopted legacy-bound session counts as running → pool is not cold →
	// the custom check's 0 stands and no cold-wake probe inflates demand.
	if demand := result.ScaleCheckCounts[qualifiedName]; demand != 0 {
		t.Errorf("expected demand 0 (adopted legacy-bound session suppresses cold probe), got %d", demand)
	}
}

// TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPool
// proves the BF-1 review finding is closed: open, unassigned work routed to the
// legacy bound form of a now-unbound pool agent ("rig-A/gc.planner") must wake
// and be claimable by the canonical "rig-A/planner" pool. The canonical
// pool-demand probe matches gc.routed_to against the canonical target by raw
// string, so before the reconciler canonicalizes the route the cold pool never
// sees the demand and migration-era ready work stays stuck at zero. After the
// re-home the cold-wake probe counts it (clamped to 1) and the persisted route
// is canonical so the canonical worker's work_query/claim path can surface it.
func TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPool(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	cityStore := beads.NewMemStore()
	rigAStore := beads.NewMemStore()
	rigStores := map[string]beads.Store{
		"rig-A": rigAStore,
	}

	const legacyRoute = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	// Open, unassigned demand still routed to the removed bound identity. No live
	// session owns it (empty assignee, open status), so it is pure migration-era
	// ready work that the canonical pool cannot see until its route is rewritten.
	created, err := cityStore.Create(beads.Bead{
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": legacyRoute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	sessionBeads := &sessionBeadSnapshot{} // cold pool: no running sessions

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// The legacy-routed demand is canonicalized to rig-A/planner, so the cold-wake
	// probe now sees it and wakes the pool from zero (clamped to 1).
	if demand := result.ScaleCheckCounts[canonical]; demand != 1 {
		t.Errorf("expected demand 1 (legacy-routed work canonicalized wakes cold pool), got %d", demand)
	}

	// The persisted route is canonical, so the canonical worker's work_query and
	// the claim predicate (raw-string gc.routed_to match) can surface and claim it.
	got, err := cityStore.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", created.ID, err)
	}
	if routed := got.Metadata["gc.routed_to"]; routed != canonical {
		t.Errorf("gc.routed_to = %q, want %q (re-homed to canonical)", routed, canonical)
	}
}

// TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPoolCachingStore
// pins BC-1: within one reconcile pass, canonicalizeLegacyBoundUnassignedRoutedWork
// rewrites gc.routed_to on open ready work between the assigned-work ready probe
// and the later scale-check probe. On a production-style CachingStore (explicit
// cached/live handles) the scale-check must read the POST-rewrite route, not a
// live snapshot memoized before the write, or the canonical cold pool never
// wakes. The MemStore sibling test above cannot catch this: a plain store has no
// cached/live handle split, so its controller-demand read re-reads current state
// instead of returning the pre-write live memo.
func TestBuildDesiredState_ScaleFromZero_LegacyBoundUnassignedRoutedWorkWakesCanonicalPoolCachingStore(t *testing.T) {
	tmpDir := t.TempDir()
	rigPath := tmpDir + "/rigs/rig-A"
	if err := os.MkdirAll(rigPath, 0o755); err != nil {
		t.Fatal(err)
	}

	maxSess := 5
	minSess := 0
	cfg := &config.City{
		Agents: []config.Agent{
			{
				Name:              "planner",
				MaxActiveSessions: &maxSess,
				MinActiveSessions: &minSess,
				ScaleCheck:        "printf 0", // custom check returns 0
				Dir:               "rig-A",
				Provider:          "mock",
			},
		},
		Rigs: []config.Rig{
			{Name: "rig-A", Path: rigPath},
		},
		Providers: map[string]config.ProviderSpec{
			"mock": {
				Command: "true",
			},
		},
	}

	const legacyRoute = "rig-A/gc.planner"
	const canonical = "rig-A/planner"

	// City store is a production-style CachingStore with explicit cached/live
	// handles. Seed the open, unassigned, legacy-routed demand into the backing
	// store and prime the cache, mirroring a live city where the bead predates
	// this tick. No live session owns it, so it is pure migration-era ready work.
	backing := beads.NewMemStore()
	created, err := backing.Create(beads.Bead{
		Status: "open",
		Type:   "task",
		Metadata: map[string]string{
			"gc.routed_to": legacyRoute,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	cityStore := beads.NewCachingStoreForTest(backing, nil)
	if err := cityStore.PrimeActive(); err != nil {
		t.Fatalf("PrimeActive: %v", err)
	}

	rigStores := map[string]beads.Store{
		"rig-A": beads.NewMemStore(),
	}

	sessionBeads := &sessionBeadSnapshot{} // cold pool: no running sessions

	result := buildDesiredStateWithSessionBeads(
		"test-city", tmpDir, time.Now(), cfg, &localMockProvider{},
		cityStore, rigStores, sessionBeads, nil, os.Stderr,
	)

	// The scale-check probe runs after the same-pass canonicalization write, so
	// it must observe the canonical route through the CachingStore and wake the
	// cold pool (clamped to 1). A stale pre-write live snapshot would bucket the
	// demand under the legacy route and leave this at 0.
	if demand := result.ScaleCheckCounts[canonical]; demand != 1 {
		t.Errorf("expected demand 1 (canonicalized legacy route wakes cold pool via CachingStore), got %d", demand)
	}

	// The persisted route is canonical.
	got, err := cityStore.Get(created.ID)
	if err != nil {
		t.Fatalf("Get(%s): %v", created.ID, err)
	}
	if routed := got.Metadata["gc.routed_to"]; routed != canonical {
		t.Errorf("gc.routed_to = %q, want %q (re-homed to canonical)", routed, canonical)
	}
}
