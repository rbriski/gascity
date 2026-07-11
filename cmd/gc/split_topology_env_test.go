package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/beads/splittest"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/sling"
)

// This file is the P1 two-topology fixture for the split-store conformance
// harness.
//
// WHY THIS EXISTS: the split-store bug class — code answering "which store owns
// this class of bead?" differently on different paths — has produced 18 audited
// landmines on this branch, and in the same week a live production incident: the
// isCold demand gate left rig pool demand blind to infra-resident routed wisps
// on warm ticks, so the fleet spun a spawn/drain treadmill (workers spawned on
// cold wake, found "no work" through the store-blind warm path, and drained).
// Every one of those bugs is a per-call-site divergence from the single dispatch
// point, resolveClassStore (cmd/gc/class_store.go). The only durable defense is
// to run the SAME invariants over BOTH store topologies, so a path that
// hard-codes one store fails the other topology immediately.
//
// Two lessons the original harness plan under-covered are first-class here:
//
//  1. WISP/EPHEMERAL TIER. Production molecules materialize as ephemeral wisps
//     (the bd wisps table; ids like gcg-wisp-0042), NOT durable main-tier rows.
//     A fixture that only seeds durable beads proves nothing about the tier
//     production actually runs on, so this fixture mints real ephemeral wisps
//     through the production policy stack (splitEnv.mintWisp).
//  2. WARM TICKS. Demand/readiness invariants must hold on WARM ticks, not just
//     cold wake — the treadmill above was invisible to any cold-only probe. The
//     fixture itself is tick-agnostic; P3 conformance invariants built on
//     forEachTopology must exercise the warm-path readers (activeStores,
//     collectAssignedWorkBeadsWithStores, collectOpenUnassignedRoutedWork), not
//     only the cold-wake ones.
//
// The fixture generalizes splitMoleculeStores (e12_graph_tail_split_test.go)
// and newMigrateFastFixture (infra_store_migrate_test.go): the same production
// policy wrappers, but over splittest.NewSplitStores STRICT MemStore leaves —
// cross-store DepAdd and foreign-prefix creates fail loud the way bd/Dolt
// fails in production, instead of passing on MemStore leniency — plus an
// on-disk city marker so the cityHasInfraStore presence check — THE
// split/single boundary — is real. Fidelity gap (accepted, same as those
// fixtures): the leaves are MemStores, not Dolt/CachingStore; the real store
// opener is exercised only by the managed-Dolt integration tests.

// splitEnv is the two-topology store fixture. Every field is the production
// shape for its topology:
//
//   - split=true: work is a policy-wrapped strict MemStore (the domain/work
//     store) and infra is a distinct wrapInfraStoreWithBeadPolicies-wrapped
//     strict id-honoring MemStore (the coordination store), with
//     prefix-disjoint id spaces (work mints the store default, infra mints
//     reserved gcg- ids) and an on-disk infra marker so
//     cityHasInfraStore(cityPath) is true.
//   - split=false: infra is nil and every class collapses to work through
//     resolveClassStore's single-store arm (class_store.go), byte-identical to
//     upstream single-store Gas City; no marker, so cityHasInfraStore is false.
//
// store is the policy-wrapped front door a production call site holds as "the
// bead store" (the city work store); per-class ownership is derived from it via
// resolveClassStore, never assumed — use classStore/graphStore.
type splitEnv struct {
	cityPath string
	cfg      *config.City
	work     beads.Store
	infra    beads.Store
	store    beads.Store
	split    bool
}

// newSplitEnv builds one topology of the fixture. The leaves come from
// splittest.NewSplitStores — strict, prefix-disjoint MemStore doubles — and
// are wrapped here in the production policy stack, the composition the kit
// documents for cmd/gc fixtures. The single-store topology keeps only the
// strict work leaf (a legacy city has one store; the discarded infra leaf
// costs nothing).
//
// The cfg keeps bd-1.0.5 storage semantics (unlike the e12 fixture cfg, which
// never creates wisps): under bd-1.0.4 defaults the wisp policy falls back to
// the history tier, and the whole point of the wisp-tier coverage is that
// production wisps are EPHEMERAL (defaultBeadStorage in bead_policy_store.go).
func newSplitEnv(t *testing.T, split bool) splitEnv {
	t.Helper()
	cityPath := t.TempDir()
	writeSplitTopologyCityConfig(t, cityPath)
	cfg := &config.City{
		Workspace: config.Workspace{Name: "split-topology-city", Prefix: "ga"},
		Beads: config.BeadsConfig{
			Provider:        "file",
			BDCompatibility: config.BeadsBDCompatibility105,
		},
	}
	workLeaf, infraLeaf := splittest.NewSplitStores(t)
	work := wrapStoreWithBeadPolicies(workLeaf, cfg)
	e := splitEnv{cityPath: cityPath, cfg: cfg, work: work, store: work, split: split}
	if split {
		seedSplitCityInfraMarker(t, cityPath)
		e.infra = wrapInfraStoreWithBeadPolicies(infraLeaf, cfg)
	}
	return e
}

// writeSplitTopologyCityConfig writes the fixture's city.toml so code under
// test that loads config from cityPath sees the same city the in-memory cfg
// describes: file provider (no dolt/bd in the sandbox) and bd-1.0.5 storage
// semantics (ephemeral wisp tier).
func writeSplitTopologyCityConfig(t *testing.T, cityPath string) {
	t.Helper()
	content := "[workspace]\n" +
		"name = \"split-topology-city\"\n" +
		"prefix = \"ga\"\n" +
		"\n" +
		"[beads]\n" +
		"provider = \"file\"\n" +
		"bd_compatibility = \"bd-1.0.5\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write split-topology city.toml: %v", err)
	}
}

// forEachTopology runs the same invariant body over both store topologies.
// This is the core defense against the split-store bug class: an invariant
// that encodes "which store owns this class" correctly holds in both subtests,
// while a path that hard-codes one store (the way build_desired_state.go's
// activeStores and coordClassStoreCandidates hard-code city+rigs today) fails
// the split subtest.
func forEachTopology(t *testing.T, fn func(t *testing.T, e splitEnv)) {
	t.Helper()
	t.Run("single-store", func(t *testing.T) {
		fn(t, newSplitEnv(t, false))
	})
	t.Run("split", func(t *testing.T) {
		fn(t, newSplitEnv(t, true))
	})
}

// classStore resolves the owning store for a coordination class through the
// production dispatch point. Fixture consumers must route through this (or a
// production seam under test) instead of touching e.work/e.infra directly, so
// the invariant exercises the same ownership decision production makes.
func (e splitEnv) classStore(class string) beads.Store {
	return resolveClassStore(e.work, e.infra, e.cfg, e.cityPath, class, events.Discard)
}

// graphStore is the graph-class front door: the infra store on the split
// topology, the work store on the single-store topology. Wisps and molecule
// roots are graph-class, so this is the store that owns everything mintWisp
// creates.
func (e splitEnv) graphStore() beads.Store {
	return e.classStore(config.BeadClassGraph)
}

// splitWispIDSeq feeds deterministic, process-unique wisp id suffixes.
var splitWispIDSeq atomic.Int64

// splitWispID mints the next wisp bead id in the shape bd's wisp tier mints on
// the infra scope: <issue_prefix>-wisp-<suffix> (beads
// internal/storage/dolt/wisps.go wispPrefix), e.g. gcg-wisp-0042. The numeric
// suffix is deliberate: production suffixes are short alnum hashes ("dv78"),
// and both shapes make the config-free sling.BeadPrefix heuristic report
// "gcg-wisp" — which is NOT a reserved class prefix — while an ordinary infra
// id ("gcg-1a2b3c4d") reports the reserved "gcg". A random suffix could flip
// between the two shapes run to run; the numeric one pins the production
// (hash-like) routing shape deterministically. Any by-id path that routes on
// IsReservedClassPrefix(BeadPrefix(id)) therefore sees wisp ids differently
// from other infra ids — a seam the P3 conformance invariants must probe, not
// paper over.
func splitWispID() string {
	return fmt.Sprintf("%s-wisp-%04d", config.InfraScopePrefix, splitWispIDSeq.Add(1))
}

// mintWisp creates an EPHEMERAL wisp root bead through the production policy
// front door, the way production molecule materialization does: a graph-class
// create carrying gc.kind=wisp, which policyForCreate classifies as the wisp
// policy and lands on the ephemeral tier (bd's wisps table; Bead.Ephemeral on
// the MemStore leaves here). On the split topology it carries a gcg-wisp-
// shaped explicit id (the id-honoring infra leaf keeps it, mirroring bd's wisp
// minting under the infra scope prefix); on the single-store topology the work
// store mints its own id, matching a legacy city where wisps share the one
// store.
//
// Production molecules are wisps, not durable rows — invariants seeded only
// with durable beads have already missed a live incident (the warm-tick demand
// gate blind to routed wisps), so P3 invariants should prefer this over plain
// Create when the bead under test represents orchestration work.
func (e splitEnv) mintWisp(t *testing.T, title string) beads.Bead {
	t.Helper()
	b := beads.Bead{
		Title: title,
		Type:  "task", // graph.v2 wisps materialize as issue_type "task"
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: beadmeta.KindWisp,
		},
	}
	if e.split {
		b.ID = splitWispID()
	}
	created, err := e.graphStore().Create(b)
	if err != nil {
		t.Fatalf("minting wisp %q: %v", title, err)
	}
	// Fixture honesty (mirrors newMigrateFastFixture.createWork): the minted
	// bead must classify as infrastructure and must genuinely land on the
	// ephemeral tier, or every "wisp" invariant built on it is vacuous.
	if !coordclass.Classify(created).IsInfrastructure() {
		t.Fatalf("minted wisp %s classifies as work, want infrastructure (type=%q metadata=%v)",
			created.ID, created.Type, created.Metadata)
	}
	if !created.Ephemeral {
		t.Fatalf("minted wisp %s is not ephemeral; the fixture cfg must keep bd-1.0.5 storage semantics so the wisp policy maps to the ephemeral tier", created.ID)
	}
	return created
}

// splitTopologyClassTable is the minimal per-class ownership table, mirroring
// the class-accessor conformance tables in class_store_test.go: the five
// coordination classes route to the infra store on a split city; work stays on
// the work store.
var splitTopologyClassTable = []struct {
	class     string
	wantInfra bool
}{
	{config.BeadClassGraph, true},
	{config.BeadClassSessions, true},
	{config.BeadClassMessaging, true},
	{config.BeadClassOrders, true},
	{config.BeadClassNudges, true},
	{config.BeadClassWork, false},
}

// TestSplitEnvTopologies is the fixture self-test: it pins the properties every
// P3 conformance invariant will assume, in both topologies.
func TestSplitEnvTopologies(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		if !sameStorePtr(e.store, e.work) {
			t.Fatalf("splitEnv.store front door = %p, want the work store handle %p", e.store, e.work)
		}
		if e.split {
			assertSplitTopology(t, e)
		} else {
			assertSingleStoreTopology(t, e)
		}
		assertWispTier(t, e)
	})
}

// assertSplitTopology pins the split-city half: the presence boundary is on,
// the handles are distinct, class routing sends every coordination class to the
// infra store, and the two id spaces are prefix-disjoint.
func assertSplitTopology(t *testing.T, e splitEnv) {
	t.Helper()
	if !cityHasInfraStore(e.cityPath) {
		t.Fatalf("cityHasInfraStore(%q) = false on the split topology, want true (infra marker missing)", e.cityPath)
	}
	if e.infra == nil {
		t.Fatal("split topology has a nil infra store")
	}
	if sameStorePtr(e.work, e.infra) {
		t.Fatal("work and infra are the same handle on the split topology, want distinct stores")
	}
	for _, row := range splitTopologyClassTable {
		want, name := e.work, "work"
		if row.wantInfra {
			want, name = e.infra, "infra"
		}
		if got := e.classStore(row.class); !sameStorePtr(got, want) {
			t.Errorf("resolveClassStore(%q) on the split topology did not route to the %s store", row.class, name)
		}
	}
	// The named helpers production call sites use must agree with the table —
	// they are the same dispatch point, but a divergence here is exactly the
	// bug class this harness exists for.
	if got := resolveSessionStore(e.work, e.infra, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.infra) {
		t.Error("resolveSessionStore routed off the infra store on a split city")
	}
	if got := resolveGraphStore(e.work, e.infra, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.infra) {
		t.Error("resolveGraphStore routed off the infra store on a split city")
	}
	assertPrefixDisjoint(t, e)
}

// assertPrefixDisjoint pins the ID-prefix half of the boundary invariant: every
// infra-store bead carries a reserved class prefix (the infra scope mints
// gcg-...), and no work-store bead does. Cross-store by-id routing
// (claimableStore.storeForID, hookClaimTargetsInfra) rides on this.
func assertPrefixDisjoint(t *testing.T, e splitEnv) {
	t.Helper()
	workBead, err := e.work.Create(beads.Bead{Title: "work-class backlog item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if config.IsReservedClassPrefix(sling.BeadPrefix(workBead.ID)) {
		t.Errorf("work-store bead id %q carries a reserved class prefix; the id spaces must be disjoint", workBead.ID)
	}
	sessionBead, err := e.infra.Create(beads.Bead{
		Title:    "worker-1",
		Type:     session.BeadType,
		Labels:   []string{session.LabelSession},
		Metadata: map[string]string{"session_id": "sess-1"},
	})
	if err != nil {
		t.Fatalf("create session bead in infra store: %v", err)
	}
	if !strings.HasPrefix(sessionBead.ID, config.InfraScopePrefix+"-") {
		t.Errorf("infra-store bead id %q does not carry the infra scope prefix %q-", sessionBead.ID, config.InfraScopePrefix)
	}
	if !config.IsReservedClassPrefix(sling.BeadPrefix(sessionBead.ID)) {
		t.Errorf("infra-store bead id %q does not resolve to a reserved class prefix; by-id routing would send it to the work store", sessionBead.ID)
	}
	// Strictness is live through the policy stack: an explicitly infra-prefixed
	// create against the WORK front door is a residence-invariant violation and
	// must fail loud (the strict leaf mirrors bd rejecting a mismatched --id),
	// not mint a foreign-prefix row the way a plain MemStore would.
	if leaked, err := e.work.Create(beads.Bead{ID: config.MintInfraBeadID("leak"), Title: "misrouted infra bead", Type: "task"}); err == nil {
		t.Errorf("work front door accepted explicitly infra-prefixed create (minted %q); the strict residence guard is not wired", leaked.ID)
	}
}

// assertSingleStoreTopology pins the legacy half: no infra marker, no infra
// store, and resolveClassStore's single-store arm collapses EVERY class —
// known and unknown — to the work store, byte-identical to upstream
// single-store Gas City.
func assertSingleStoreTopology(t *testing.T, e splitEnv) {
	t.Helper()
	if cityHasInfraStore(e.cityPath) {
		t.Fatalf("cityHasInfraStore(%q) = true on the single-store topology, want false", e.cityPath)
	}
	if e.infra != nil {
		t.Fatalf("single-store topology has a non-nil infra store %p", e.infra)
	}
	classes := make([]string, 0, len(splitTopologyClassTable)+1)
	for _, row := range splitTopologyClassTable {
		classes = append(classes, row.class)
	}
	// The single-store arm is class-blind, so even a class string no switch arm
	// names must collapse to the work store.
	classes = append(classes, "not-a-known-class")
	for _, class := range classes {
		if got := e.classStore(class); !sameStorePtr(got, e.work) {
			t.Errorf("resolveClassStore(%q) on the single-store topology did not collapse to the work store", class)
		}
	}
	if got := resolveSessionStore(e.work, e.infra, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.work) {
		t.Error("resolveSessionStore did not collapse to the work store on a single-store city")
	}
	if got := resolveGraphStore(e.work, e.infra, e.cfg, e.cityPath, events.Discard); !sameStorePtr(got, e.work) {
		t.Error("resolveGraphStore did not collapse to the work store on a single-store city")
	}
}

// assertWispTier pins the wisp/ephemeral-tier half of the fixture in both
// topologies: the minted wisp is resident in its owning store only, is
// readable back through the policy front door, and genuinely sits on the
// ephemeral tier (visible through the front door's tier-expanded reads,
// invisible to a default main-tier read on the raw leaf).
func assertWispTier(t *testing.T, e splitEnv) {
	t.Helper()
	title := "single-store conformance wisp"
	if e.split {
		title = "split conformance wisp"
	}
	w := e.mintWisp(t, title)

	if e.split {
		if !strings.HasPrefix(w.ID, config.InfraScopePrefix+"-wisp-") {
			t.Fatalf("split-topology wisp id = %q, want a %s-wisp- shaped id", w.ID, config.InfraScopePrefix)
		}
		if _, err := e.infra.Get(w.ID); err != nil {
			t.Fatalf("minted wisp %s not resident in the infra store: %v", w.ID, err)
		}
		if _, err := e.work.Get(w.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Fatalf("minted wisp %s resolves in the WORK store (err=%v); wisps must be infra-resident on a split city", w.ID, err)
		}
	} else if config.IsReservedClassPrefix(sling.BeadPrefix(w.ID)) {
		t.Fatalf("single-store wisp id %q carries a reserved class prefix; a legacy city mints work-store ids", w.ID)
	}

	got, err := e.graphStore().Get(w.ID)
	if err != nil {
		t.Fatalf("minted wisp %s not readable through the graph front door: %v", w.ID, err)
	}
	if !got.Ephemeral {
		t.Fatalf("wisp %s read back through the front door lost the ephemeral flag", w.ID)
	}

	// Tier honesty: a default (TierIssues) List through the policy front door is
	// expanded to TierBoth, so the ephemeral wisp is visible; the same default
	// query on the raw leaf tier-filters it out. Together these prove the bead
	// landed on the wisp tier instead of the front door merely being permissive.
	frontDoorList, err := e.graphStore().List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("front-door list: %v", err)
	}
	if !beadListHasID(frontDoorList, w.ID) {
		t.Errorf("front-door default List does not surface ephemeral wisp %s; warm-tick readers using this path would be wisp-blind", w.ID)
	}
	leaf, _, ok := unwrapBeadPolicyStore(e.graphStore())
	if !ok {
		t.Fatalf("graph front door %T is not policy-wrapped", e.graphStore())
	}
	leafList, err := leaf.List(beads.ListQuery{AllowScan: true})
	if err != nil {
		t.Fatalf("leaf list: %v", err)
	}
	if beadListHasID(leafList, w.ID) {
		t.Errorf("raw-leaf default (main-tier) List surfaces wisp %s; the bead did not land on the ephemeral tier", w.ID)
	}
}

// beadListHasID reports whether the list contains a bead with the given id.
func beadListHasID(list []beads.Bead, id string) bool {
	for _, b := range list {
		if b.ID == id {
			return true
		}
	}
	return false
}
