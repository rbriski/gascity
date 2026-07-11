//go:build integration

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/orders"
	"github.com/gastownhall/gascity/internal/session"
)

// This is the E2.5 integration-tier of the domain/infra store boundary invariant
// (design step 2b). Where the fast tier (infra_store_boundary_invariant_test.go)
// runs the production wrapper stack over MemStore, this tier runs it over the
// REAL two-store shape against a live, gc-MANAGED Dolt sql-server: a city (hq)
// scope, a rig (fe) scope, AND a third .gc/infra (gcg) scope, each a distinct
// Dolt database on the same server. Every store is brought up through the true
// production lifecycle (seedInitInfraScope's canonical managed-Dolt scope config
// + startBeadsLifecycle's initAndHookDir infra bd-init) and opened through the
// true production open path (openCityStoreAt / openCityInfraStoreAt, incl.
// wrapStoreWithBeadPolicies / wrapInfraStoreWithBeadPolicies + reserved-prefix
// minting). It proves the split holds end-to-end against real Dolt, and that an
// existing single-store city (no infra scope) is left untouched.
//
// It reuses the package-exported assertStoreClassBoundary helper verbatim — that
// is exactly why the fast tier exported it.
//
// It rides the SAME managed-Dolt harness the passing cmd/gc process tests use
// (setupManagedBdWaitTestCity), so it is gated behind GC_FAST_UNIT=0 (run
// `make test-cmd-gc-process` for full coverage) and skips — never falsely fails —
// on a machine without a working bd/dolt toolchain.

func TestInfraStoreBoundaryInvariantIntegration(t *testing.T) {
	// Activate the split for the managed-Dolt city seeded by the harness: with
	// GC_INFRA_STORE_SPLIT=1, seedInitInfraScope writes the .gc/infra canonical
	// scope config (its own Dolt database), the same opt-in gc init uses.
	t.Setenv("GC_INFRA_STORE_SPLIT", "1")

	// setupManagedBdWaitTestCity stands up a real, gc-managed Dolt sql-server with
	// a city (hq) scope and a frontend rig (fe) scope — two live Dolt databases —
	// and publishes the managed-Dolt runtime state so store opens resolve the
	// server port. It gates on GC_FAST_UNIT=0 and skips when bd/dolt are absent.
	cityPath, _ := setupManagedBdWaitTestCity(t)

	cfg, _, err := loadCityConfigWithBuiltinPacks(cityPath)
	if err != nil {
		t.Fatalf("load city config: %v", err)
	}

	// seedInitInfraScope writes the .gc/infra scope's canonical managed-Dolt
	// config.yaml + metadata.json (issue_prefix=gcg, its own dolt_database), the
	// exact production infra-seed gc init performs. Writing config.yaml is what
	// makes cityHasInfraStore true and activates the split.
	if err := seedInitInfraScope(cityPath); err != nil {
		t.Fatalf("seedInitInfraScope: %v", err)
	}
	if !cityHasInfraStore(cityPath) {
		t.Fatal("cityHasInfraStore is false after seeding the infra scope; the split did not activate")
	}

	// bd-init the infra store's own Dolt database on the running managed server —
	// the exact call startBeadsLifecycle makes for the infra scope. This creates
	// the third Dolt database (gcg) alongside hq and fe.
	if err := initAndHookDir(cityPath, infraScopeRoot(cityPath), config.InfraScopePrefix); err != nil {
		t.Fatalf("initAndHookDir(infra scope): %v", err)
	}

	// Open both stores through the true production open path.
	workStore, err := openCityStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open city work store: %v", err)
	}
	infraStore, present, err := openCityInfraStoreAt(cityPath)
	if err != nil {
		t.Fatalf("open city infra store: %v", err)
	}
	if !present || infraStore == nil {
		t.Fatal("infra store not present after bd-initing the infra scope on a split city")
	}

	// Seed a representative infra bead mix through the production accessors, each
	// resolving its class store off the real infra store.
	seedRepresentativeInfraBeads(t, workStore, infraStore, cfg, cityPath)

	// The boundary invariant, end-to-end against real Dolt: the work store holds
	// no infra-class bead, the infra store holds no domain-class bead.
	assertStoreClassBoundary(t, "domain:hq", workStore, false)
	assertStoreClassBoundary(t, "infra", infraStore, true)

	// The ID-prefix half: every infra bead carries a reserved class prefix, no
	// work bead does.
	assertReservedPrefixBoundary(t, workStore, infraStore)

	// An EXISTING single-store city — one with no seeded infra scope — must stay
	// single-store: no infra scope, byte-identical routing.
	assertSingleStoreCityUntouched(t)
}

// seedRepresentativeInfraBeads creates one bead of each infra coordination class
// through the production class resolvers, plus a plain work bead, so the boundary
// assertion has a representative population on both sides.
func seedRepresentativeInfraBeads(t *testing.T, workStore, infraStore beads.Store, cfg *config.City, cityPath string) {
	t.Helper()

	// SESSION — resolveSessionStore → infra store.
	sessStore := resolveSessionStore(workStore, infraStore, cfg, cityPath, nil)
	if _, err := session.NewStore(beads.SessionStore{Store: sessStore}).CreateSession(session.CreateSpec{
		Title:     "worker-1",
		AgentName: "worker-1",
		Metadata:  map[string]string{"provider": "tmux", "template": "claude"},
	}); err != nil {
		t.Fatalf("session create: %v", err)
	}

	// MAIL — newCityMailProvider (messaging class) → infra store.
	mp := newCityMailProvider(workStore, infraStore, cfg, cityPath, nil)
	if _, err := mp.Send("human", "worker-1", "hello", "body text"); err != nil {
		t.Fatalf("mail send: %v", err)
	}

	// NUDGE — resolveNudgesStore → infra store.
	nudgeStore := resolveNudgesStore(workStore, infraStore, cfg, cityPath, nil)
	if _, _, err := ensureQueuedNudgeBead(beads.NudgesStore{Store: nudgeStore}, newQueuedNudge("worker-1", "please continue", time.Now().UTC())); err != nil {
		t.Fatalf("nudge enqueue: %v", err)
	}

	// ORDER-TRACKING — resolveOrderStore → infra store.
	orderStore := resolveOrderStore(workStore, infraStore, cfg, cityPath, nil)
	if _, err := orders.NewStore(beads.OrdersStore{Store: orderStore}).CreateRun("gate-alpha", orders.RunOpts{}); err != nil {
		t.Fatalf("order run create: %v", err)
	}

	// PLAIN WORK — stays in the work/domain store.
	if _, err := workStore.Create(beads.Bead{Title: "real backlog item", Type: "task"}); err != nil {
		t.Fatalf("plain task create: %v", err)
	}
}

// assertReservedPrefixBoundary asserts the ID-prefix half of the invariant over
// real stores: every infra bead carries a reserved class prefix, no work bead
// does.
func assertReservedPrefixBoundary(t *testing.T, workStore, infraStore beads.Store) {
	t.Helper()
	infra, err := infraStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("infra List: %v", err)
	}
	if len(infra) == 0 {
		t.Fatal("infra store holds no beads; the reserved-prefix boundary is vacuous")
	}
	for _, b := range infra {
		if !config.IsReservedClassPrefix(idPrefixSegment(b.ID)) {
			t.Errorf("infra bead %q (type=%q) lacks a reserved class prefix", b.ID, b.Type)
		}
	}
	work, err := workStore.List(beads.ListQuery{IncludeClosed: true, TierMode: beads.TierBoth, AllowScan: true})
	if err != nil {
		t.Fatalf("work List: %v", err)
	}
	for _, b := range work {
		if config.IsReservedClassPrefix(idPrefixSegment(b.ID)) {
			t.Errorf("work bead %q (type=%q) carries a reserved class prefix", b.ID, b.Type)
		}
	}
}

// assertSingleStoreCityUntouched confirms a managed-Dolt city WITHOUT the
// infra-store split stays single-store: cityHasInfraStore is false, no .gc/infra
// scope exists, and the infra opener reports absence (so class routing is
// identity and the city is byte-identical to upstream Gas City).
//
// It does not need a second live Dolt server: openCityInfraStoreResultAt
// short-circuits on cityHasInfraStore before touching Dolt, so a scaffolded
// managed-Dolt city with no seeded infra scope is sufficient to prove the split
// never leaked into a non-seeded city.
func assertSingleStoreCityUntouched(t *testing.T) {
	t.Helper()
	// This helper proves an EXISTING single-store city is untouched by the split.
	// It scaffolds via writeManagedBdWaitTestCityScaffold (not `gc init`), so it
	// never seeds the infra scope regardless of the default; the explicit
	// GC_INFRA_STORE_SPLIT=0 opt-out overrides the =1 the enclosing test set and
	// pins the single-store premise now that two-store is the `gc init` default.
	t.Setenv("GC_INFRA_STORE_SPLIT", "0")
	cityPath := shortSocketTempDir(t, "gc-single-store-")
	if _, err := writeManagedBdWaitTestCityScaffold(cityPath); err != nil {
		t.Fatalf("writeManagedBdWaitTestCityScaffold: %v", err)
	}
	materializeBuiltinPacksForTest(t, cityPath)

	// Deliberately DO NOT seed the infra scope: this is an existing single-store
	// city. cityHasInfraStore must stay false and the infra opener must report
	// absence, so class routing is identity and the city is byte-identical.
	if cityHasInfraStore(cityPath) {
		t.Fatal("single-store city unexpectedly reports an infra scope")
	}
	if _, err := os.Stat(infraScopeRoot(cityPath)); !os.IsNotExist(err) {
		t.Fatalf("single-store city has an .gc/infra dir (err=%v); it must be untouched", err)
	}
	if _, err := os.Stat(filepath.Join(infraScopeRoot(cityPath), ".beads", "config.yaml")); !os.IsNotExist(err) {
		t.Fatalf("single-store city has an infra scope config (err=%v); it must be untouched", err)
	}
	if _, present, err := openCityInfraStoreAt(cityPath); err != nil {
		t.Fatalf("open infra store (single-store): %v", err)
	} else if present {
		t.Fatal("single-store city reports an infra store present; the split leaked into a non-seeded city")
	}
}
