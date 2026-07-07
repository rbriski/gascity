//go:build integration

package main

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/sling"
)

// These are the P0 gate of the domain/infra store split (landmine #1 + #2 in
// engdocs/contributors/cross-store-split-landmines.md): a formula must be able to
// RUN on a split city — its graph-class control beads DISCOVERED by the
// control-dispatcher serve loop, and its routed graph step beads CLAIMED by a
// worker — even though those beads live in the infra store (<city>/.gc/infra),
// not the city/rig work store. Both ride the managed-Dolt harness
// (setupManagedBdWaitTestCity), so they are gated behind GC_FAST_UNIT=0 and skip
// (never falsely fail) without a working bd/dolt toolchain. Run:
//
//	GC_FAST_UNIT=0 GOCACHE=/tmp/gc-reloc-cache \
//	  go test -tags integration ./cmd/gc/ -run TestSplitCity_ -count=1 -timeout 12m

// appendToCityToml appends body to <cityPath>/city.toml. The managed-Dolt harness
// re-reads config on every loadCityConfig, so a post-copy rewrite is safe.
func appendToCityToml(t *testing.T, cityPath, body string) {
	t.Helper()
	path := filepath.Join(cityPath, "city.toml")
	existing, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read city.toml: %v", err)
	}
	if err := os.WriteFile(path, append(existing, []byte(body)...), 0o644); err != nil {
		t.Fatalf("append city.toml: %v", err)
	}
}

// activateManagedSplitCity seeds + bd-inits the infra scope through the exact
// production path gc init performs, activating the two-store split on the
// managed-Dolt city the harness stood up, and returns the loaded config. It
// mirrors the setup in infra_store_command_sweep_integration_test.go.
func activateManagedSplitCity(t *testing.T, cityPath string) *config.City {
	t.Helper()
	cfg, _, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}
	if err := seedInitInfraScope(cityPath); err != nil {
		t.Fatalf("seedInitInfraScope: %v", err)
	}
	if !cityHasInfraStore(cityPath) {
		t.Fatal("cityHasInfraStore is false after seeding the infra scope; the split did not activate")
	}
	if err := initAndHookDir(cityPath, infraScopeRoot(cityPath), config.InfraScopePrefix); err != nil {
		t.Fatalf("initAndHookDir(infra scope): %v", err)
	}
	// The infra store's absence may have been probed and cached before seeding.
	// Clear the per-process memo so the CLI seams re-open the now-present store.
	clearInfraStoreCacheKey(cityPath)
	t.Cleanup(func() { clearInfraStoreCacheKey(cityPath) })
	if cachedCityInfraStore(cityPath, cfg) == nil {
		t.Fatal("cachedCityInfraStore returned nil on a seeded split city; CLI seams would route infra reads to the work store")
	}
	return cfg
}

func TestSplitCity_DispatcherDiscoversInfraControlBeads(t *testing.T) {
	t.Setenv("GC_INFRA_STORE_SPLIT", "1")
	cityPath, _ := setupManagedBdWaitTestCity(t)

	// The harness scaffold ships no agents; register a city-scoped control-dispatcher.
	appendToCityToml(t, cityPath, testControlDispatcherAgentTOML(""))
	cfg := activateManagedSplitCity(t, cityPath)

	agentCfg, ok := resolveAgentIdentity(cfg, config.ControlDispatcherAgentName, currentRigContext(cfg))
	if !ok {
		t.Fatal("control-dispatcher agent not found in config after append")
	}
	route := agentCfg.QualifiedName()

	// Seed one READY, unassigned, routed control bead into the INFRA store via the
	// production graph-class front door: cliGraphStore returns the infra store on a
	// split city, and its bead-policy wrapper mints the reserved "gcg" id.
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	graph := cliGraphStore(work, cfg, cityPath)
	ctrl, err := graph.Create(beads.Bead{
		Title: "control: scope-check",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:     "scope-check",
			beadmeta.RoutedToMetadataKey: route,
		},
	})
	if err != nil {
		t.Fatalf("seed infra control bead: %v", err)
	}
	if !config.IsReservedClassPrefix(sling.BeadPrefix(ctrl.ID)) {
		t.Fatalf("seeded control bead %q is not infra-classed (want a %q-prefixed id)", ctrl.ID, config.InfraScopePrefix)
	}
	if _, err := work.Get(ctrl.ID); err == nil {
		t.Fatalf("control bead %q leaked into the work store; seeding did not isolate to infra", ctrl.ID)
	}

	// Drive the serve loop. Keep the discovery query REAL (it is the unit under
	// test — the real `bd ready` scan against the resolved scope), but bound it to a
	// single drain: a persistent ready control bead the stub does not close would
	// otherwise re-poll forever (dispatch_runtime.go:532 continues after processing).
	prevList := workflowServeList
	prevControl := controlDispatcherServe
	prevInterval := workflowServeIdlePollInterval
	prevAttempts := workflowServeIdlePollAttempts
	workflowServeIdlePollInterval = 0
	workflowServeIdlePollAttempts = 0
	t.Cleanup(func() {
		workflowServeList = prevList
		controlDispatcherServe = prevControl
		workflowServeIdlePollInterval = prevInterval
		workflowServeIdlePollAttempts = prevAttempts
	})

	calls := 0
	workflowServeList = func(q, dir string, env map[string]string) ([]hookBead, error) {
		calls++
		if calls > 1 {
			return nil, nil // terminate after the first real drain
		}
		return prevList(q, dir, env) // REAL shell `bd ready` scan against the serve-loop scope
	}

	type served struct{ store, id string }
	var (
		mu  sync.Mutex
		got []served
	)
	controlDispatcherServe = func(_, storePath, beadID string, _ io.Writer, _ io.Writer) error {
		mu.Lock()
		got = append(got, served{storePath, beadID})
		mu.Unlock()
		return nil
	}

	var stderr bytes.Buffer
	if err := runWorkflowServe(config.ControlDispatcherAgentName, false, io.Discard, &stderr); err != nil {
		t.Fatalf("runWorkflowServe: %v\nstderr: %s", err, stderr.String())
	}

	// The real query must have surfaced the infra-resident control bead, and the
	// serve loop must have handed it to the dispatcher scoped to the infra store —
	// so the real ProcessControl would open .gc/infra, not the work store.
	idx := slices.IndexFunc(got, func(s served) bool { return s.id == ctrl.ID })
	if idx < 0 {
		t.Fatalf("control bead %q not discovered on a split city; served=%+v\nstderr: %s", ctrl.ID, got, stderr.String())
	}
	if !samePath(got[idx].store, infraScopeRoot(cityPath)) {
		t.Fatalf("control bead %q served with storePath=%q, want infra scope %q", ctrl.ID, got[idx].store, infraScopeRoot(cityPath))
	}
}

func TestSplitCity_HookClaimFindsInfraStepBead(t *testing.T) {
	t.Setenv("GC_INFRA_STORE_SPLIT", "1")
	cityPath, _ := setupManagedBdWaitTestCity(t)

	// Register a plain worker. The routed-pool tier of the default work_query
	// probes gc.routed_to=<worker qualified name>.
	appendToCityToml(t, cityPath, "\n[[agent]]\nname = \"worker\"\nstart_command = \"true\"\nprompt_mode = \"none\"\nprocess_names = [\"gc\"]\n")
	cfg := activateManagedSplitCity(t, cityPath)

	agentCfg, ok := resolveAgentIdentity(cfg, "worker", currentRigContext(cfg))
	if !ok {
		t.Fatal("worker agent not found in config after append")
	}
	route := agentCfg.QualifiedName()

	// Seed a READY, unassigned, routed graph step bead into the INFRA store.
	// graph.v2 steps keep type "task", so bd/gc ready surfaces them.
	work, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open city store: %v", err)
	}
	graph := cliGraphStore(work, cfg, cityPath)
	step, err := graph.Create(beads.Bead{
		Title: "graph step",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:   route,
			beadmeta.RootBeadIDMetadataKey: "gcg-root-formula",
		},
	})
	if err != nil {
		t.Fatalf("seed infra step bead: %v", err)
	}
	if !config.IsReservedClassPrefix(sling.BeadPrefix(step.ID)) {
		t.Fatalf("seeded step %q is not infra-classed (want a %q-prefixed id)", step.ID, config.InfraScopePrefix)
	}
	if _, err := work.Get(step.ID); err == nil {
		t.Fatalf("step %q leaked into the work store; seeding did not isolate to infra", step.ID)
	}

	// The work_query shells `gc ready`, so a gc binary must be on PATH.
	gcDir := filepath.Dir(currentGCBinaryForTests(t))
	t.Setenv("PATH", gcDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	// Worker runtime identity (matches the fake-bd claim test pattern).
	t.Setenv("GC_TEMPLATE", "worker")
	t.Setenv("GC_ALIAS", "worker-1")
	t.Setenv("GC_SESSION_ID", "session-id-1")
	t.Setenv("GC_SESSION_NAME", "worker-1")
	t.Setenv("GC_SESSION_ORIGIN", "ephemeral")

	var stdout, stderr bytes.Buffer
	code := cmdHookWithOptions(nil, hookCommandOptions{Claim: true, JSON: true}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("cmdHookWithOptions(--claim) = %d, want 0 (claim of infra step); stdout=%q stderr=%s", code, stdout.String(), stderr.String())
	}
	var result hookClaimJSONResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		t.Fatalf("stdout is not JSON: %v\nraw: %s", err, stdout.String())
	}
	if result.Reason != "claimed" || result.BeadID != step.ID || result.Assignee != "worker-1" {
		t.Fatalf("unexpected claim result: %+v (want claimed %s by worker-1)", result, step.ID)
	}

	// The claim mutation must have landed in the INFRA store, not the work store.
	clearInfraStoreCacheKey(cityPath)
	infra := cachedCityInfraStore(cityPath, cfg)
	if infra == nil {
		t.Fatal("infra store nil after claim")
	}
	claimed, err := infra.Get(step.ID)
	if err != nil {
		t.Fatalf("re-read claimed step from infra: %v", err)
	}
	if claimed.Assignee != "worker-1" {
		t.Fatalf("infra step assignee = %q, want worker-1 (claim mutation did not land in infra)", claimed.Assignee)
	}
}
