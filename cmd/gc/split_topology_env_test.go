package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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

	// Rig leg (only when built via newSplitEnvWithRig; nil/"" otherwise): one
	// registered rig with a rig-scoped default-probe pool template — the
	// newNoScaleCheckRigPoolCity shape (scale_from_zero_no_scalecheck_test.go,
	// the shape split_store_treadmill_test.go built ad hoc) generalized over
	// both topologies. rig is the policy-wrapped strict rig-prefixed work
	// store; rigStores is the map shape reconciler paths take; qualified is
	// the pool template identity routed wisps target (gc.routed_to).
	rig       beads.Store
	rigStores map[string]beads.Store
	rigName   string
	qualified string
}

// Rig-leg identity constants. The names match the treadmill/scale-from-zero
// fixtures so RCA-shaped scenarios read the same across the suite; the bead-id
// prefix is DERIVED from the rig name via config.Rig.EffectivePrefix ("ra"),
// so the cfg the code under test consults and the ids the rig store mints
// agree by construction.
const (
	splitEnvRigName   = "rig-A"
	splitEnvPoolAgent = "executor"
)

// splitEnvOptions selects the optional legs of the fixture.
type splitEnvOptions struct {
	// rig adds the rig leg: cfg wiring for one rig plus a rig-scoped pool
	// template, and a third strict store minting under the rig's prefix.
	rig bool
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
	return newSplitEnvWith(t, split, splitEnvOptions{})
}

// newSplitEnvWithRig is newSplitEnv plus the rig leg, for RCA-shaped scenarios
// (warm-tick demand, orphan release, assigned work) that need a rig-scoped
// pool working infra-resident routed wisps. The rig leg exists on BOTH
// topologies — rigs are orthogonal to the split, and the single-store subtest
// is what proves a rig-path fix keeps legacy byte-identity.
func newSplitEnvWithRig(t *testing.T, split bool) splitEnv {
	t.Helper()
	return newSplitEnvWith(t, split, splitEnvOptions{rig: true})
}

func newSplitEnvWith(t *testing.T, split bool, opts splitEnvOptions) splitEnv {
	t.Helper()
	cityPath := t.TempDir()
	rigPath := ""
	if opts.rig {
		rigPath = filepath.Join(cityPath, "rigs", splitEnvRigName)
		if err := os.MkdirAll(rigPath, 0o755); err != nil {
			t.Fatalf("mkdir rig path: %v", err)
		}
	}
	writeSplitTopologyCityConfig(t, cityPath, rigPath)
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
	if opts.rig {
		e.attachRigLeg(t, rigPath)
	}
	return e
}

// attachRigLeg wires the rig leg into cfg and the env: the config for one rig
// with a rig-scoped min=0 default-probe pool template (no scale_check — the
// exact pool shape of the treadmill RCA), and a strict rig-prefixed leaf under
// the same production policy wrap rig stores get from openStoreResultAtForCity.
func (e *splitEnv) attachRigLeg(t *testing.T, rigPath string) {
	t.Helper()
	maxSess, minSess := 5, 0
	e.cfg.Agents = []config.Agent{{
		Name:              splitEnvPoolAgent,
		MaxActiveSessions: &maxSess,
		MinActiveSessions: &minSess,
		// No ScaleCheck: default-probe pool.
		Dir:      splitEnvRigName,
		Provider: "mock",
	}}
	e.cfg.Rigs = []config.Rig{{Name: splitEnvRigName, Path: rigPath}}
	e.cfg.Providers = map[string]config.ProviderSpec{"mock": {Command: "true"}}
	rigLeaf := splittest.NewRigStore(t, e.cfg.Rigs[0].EffectivePrefix())
	e.rig = wrapStoreWithBeadPolicies(rigLeaf, e.cfg)
	e.rigStores = map[string]beads.Store{splitEnvRigName: e.rig}
	e.rigName = splitEnvRigName
	e.qualified = e.cfg.Agents[0].QualifiedName()
}

// writeSplitTopologyCityConfig writes the fixture's city.toml so code under
// test that loads config from cityPath sees the same city the in-memory cfg
// describes: file provider (no dolt/bd in the sandbox), bd-1.0.5 storage
// semantics (ephemeral wisp tier), and — when the rig leg is on (rigPath
// non-empty) — the same rig + pool-template wiring attachRigLeg puts in the
// in-memory cfg.
func writeSplitTopologyCityConfig(t *testing.T, cityPath, rigPath string) {
	t.Helper()
	content := "[workspace]\n" +
		"name = \"split-topology-city\"\n" +
		"prefix = \"ga\"\n" +
		"\n" +
		"[beads]\n" +
		"provider = \"file\"\n" +
		"bd_compatibility = \"bd-1.0.5\"\n"
	if rigPath != "" {
		content += "\n" +
			"[providers.mock]\n" +
			"command = \"true\"\n" +
			"\n" +
			"[[agent]]\n" +
			"name = " + fmt.Sprintf("%q", splitEnvPoolAgent) + "\n" +
			"dir = " + fmt.Sprintf("%q", splitEnvRigName) + "\n" +
			"provider = \"mock\"\n" +
			"max_active_sessions = 5\n" +
			"min_active_sessions = 0\n" +
			"\n" +
			"[[rigs]]\n" +
			"name = " + fmt.Sprintf("%q", splitEnvRigName) + "\n" +
			"path = " + fmt.Sprintf("%q", rigPath) + "\n"
	}
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

// forEachTopologyWithRig is forEachTopology over the rig-legged fixture, for
// invariants about rig-scoped pools working routed orchestration wisps (the
// treadmill RCA family: warm-tick demand, assigned-work reachability, orphan
// release). The single-store subtest doubles as the legacy byte-identity
// check for any rig-path fix under test.
func forEachTopologyWithRig(t *testing.T, fn func(t *testing.T, e splitEnv)) {
	t.Helper()
	t.Run("single-store", func(t *testing.T) {
		fn(t, newSplitEnvWithRig(t, false))
	})
	t.Run("split", func(t *testing.T) {
		fn(t, newSplitEnvWithRig(t, true))
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

// sessionsStore is the sessions-class front door — and therefore the
// reconciler's LEADING store (CityRuntime.buildDesiredState passes
// sessionsBeadStore()): the infra store on a split city, the work store on a
// legacy city. RCA-shaped reconciler scenarios (buildDesiredStateWithSessionBeads
// and friends) must lead with this store, exactly as production wires it.
func (e splitEnv) sessionsStore() beads.Store {
	return e.classStore(config.BeadClassSessions)
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
	return e.mintWispWith(t, wispOpts{title: title})
}

// wispOpts parameterizes mintWispWith so an RCA-shaped wisp state is mintable
// in one call. The zero value plus a title is plain mintWisp.
type wispOpts struct {
	title string
	// routedTo targets the wisp at a pool template (beadmeta.RoutedToMetadataKey,
	// e.g. e.qualified) — the routed-demand shape of the treadmill RCA.
	routedTo string
	// status is applied AFTER the create ("" keeps the store's create default,
	// "open"): stores mint open beads, so a claimed state is a post-create
	// mutation exactly as production claims are.
	status string
	// assignee is applied with status (the claiming session's identity) — the
	// claimed-wisp shape of the assigned-work/orphan-release RCA sites.
	assignee string
	// metadata is merged over the defaults (kind=wisp, routedTo) last, so a
	// scenario can layer extra keys; overriding gc.kind trips the fixture
	// honesty check below, loudly.
	metadata map[string]string
}

// mintWispWith is mintWisp with the full RCA-state option set. See mintWisp
// for why wisps (not durable beads) are the tier invariants must run on.
func (e splitEnv) mintWispWith(t *testing.T, opts wispOpts) beads.Bead {
	t.Helper()
	md := map[string]string{
		beadmeta.KindMetadataKey: beadmeta.KindWisp,
	}
	if opts.routedTo != "" {
		md[beadmeta.RoutedToMetadataKey] = opts.routedTo
	}
	for k, v := range opts.metadata {
		md[k] = v
	}
	b := beads.Bead{
		Title:    opts.title,
		Type:     "task", // graph.v2 wisps materialize as issue_type "task"
		Metadata: md,
	}
	if e.split {
		b.ID = splitWispID()
	}
	created, err := e.graphStore().Create(b)
	if err != nil {
		t.Fatalf("minting wisp %q: %v", opts.title, err)
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
	if opts.status == "" && opts.assignee == "" {
		return created
	}
	up := beads.UpdateOpts{}
	if opts.status != "" {
		up.Status = &opts.status
	}
	if opts.assignee != "" {
		up.Assignee = &opts.assignee
	}
	if err := e.graphStore().Update(created.ID, up); err != nil {
		t.Fatalf("staging wisp %s state (status=%q assignee=%q): %v", created.ID, opts.status, opts.assignee, err)
	}
	staged, err := e.graphStore().Get(created.ID)
	if err != nil {
		t.Fatalf("reloading staged wisp %s: %v", created.ID, err)
	}
	return staged
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
// P3 conformance invariant will assume, in both topologies. The plain env has
// no rig leg — reconciler-shaped scenarios opt in via newSplitEnvWithRig.
func TestSplitEnvTopologies(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		assertSplitEnvPins(t, e)
		if e.rig != nil || e.rigStores != nil || e.rigName != "" || e.qualified != "" {
			t.Fatalf("plain splitEnv grew a rig leg (rig=%p rigStores=%v rigName=%q qualified=%q); the rig leg must stay opt-in",
				e.rig, e.rigStores, e.rigName, e.qualified)
		}
	})
}

// TestSplitEnvTopologiesWithRigLeg is the rig-leg self-test: every base pin
// holds unchanged with the rig leg attached, plus the rig leg's own routing
// and disjointness properties.
func TestSplitEnvTopologiesWithRigLeg(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		assertSplitEnvPins(t, e)
		assertRigLeg(t, e)
	})
}

// assertSplitEnvPins runs the topology-appropriate base pins (class routing,
// prefix disjointness, wisp tier) shared by the plain and rig-legged envs.
func assertSplitEnvPins(t *testing.T, e splitEnv) {
	t.Helper()
	if !sameStorePtr(e.store, e.work) {
		t.Fatalf("splitEnv.store front door = %p, want the work store handle %p", e.store, e.work)
	}
	if e.split {
		assertSplitTopology(t, e)
	} else {
		assertSingleStoreTopology(t, e)
	}
	assertWispTier(t, e)
}

// assertRigLeg pins the rig leg: the cfg wiring is the RCA pool shape, the rig
// store is a genuine THIRD handle, class routing never resolves to it, and the
// three id spaces are pairwise disjoint with strict residence in all
// directions.
func assertRigLeg(t *testing.T, e splitEnv) {
	t.Helper()
	if e.rig == nil {
		t.Fatal("rig-legged env has a nil rig store")
	}
	if !sameStorePtr(e.rigStores[e.rigName], e.rig) {
		t.Fatalf("rigStores[%q] is not the rig store handle", e.rigName)
	}
	if want := splitEnvRigName + "/" + splitEnvPoolAgent; e.qualified != want {
		t.Fatalf("qualified pool template = %q, want %q", e.qualified, want)
	}

	// cfg wiring: one registered rig whose path exists, and one rig-scoped
	// min=0 default-probe pool template — the exact treadmill RCA pool shape.
	if len(e.cfg.Rigs) != 1 || e.cfg.Rigs[0].Name != e.rigName {
		t.Fatalf("cfg.Rigs = %+v, want exactly the %q rig", e.cfg.Rigs, e.rigName)
	}
	if _, err := os.Stat(e.cfg.Rigs[0].Path); err != nil {
		t.Fatalf("registered rig path %q not on disk: %v", e.cfg.Rigs[0].Path, err)
	}
	if len(e.cfg.Agents) != 1 {
		t.Fatalf("cfg.Agents = %+v, want exactly the pool template", e.cfg.Agents)
	}
	agent := e.cfg.Agents[0]
	if agent.Dir != e.rigName || agent.ScaleCheck != "" ||
		agent.MinActiveSessions == nil || *agent.MinActiveSessions != 0 ||
		agent.MaxActiveSessions == nil || *agent.MaxActiveSessions <= 0 {
		t.Fatalf("pool template %+v is not the rig-scoped min=0 default-probe shape", agent)
	}

	// The rig store is a third handle, distinct from both split-boundary stores.
	if sameStorePtr(e.rig, e.work) {
		t.Fatal("rig store aliases the work store")
	}
	if e.infra != nil && sameStorePtr(e.rig, e.infra) {
		t.Fatal("rig store aliases the infra store")
	}

	// Routing identity: resolveClassStore dispatches the split boundary only —
	// no coordination (or work) CLASS may ever resolve to a rig store. Rig
	// stores are addressed by rig NAME (store refs / rigStores), and a class
	// arm quietly returning one would be a new landmine of the audited kind.
	for _, row := range splitTopologyClassTable {
		if sameStorePtr(e.classStore(row.class), e.rig) {
			t.Errorf("resolveClassStore(%q) resolved to the RIG store", row.class)
		}
	}

	assertRigPrefixDisjoint(t, e)
}

// assertRigPrefixDisjoint pins the x3 id-space disjointness the rig leg adds:
// work, infra, and rig prefixes are pairwise distinct; the rig prefix is a
// work-shaped (non-reserved) prefix that agrees with cfg's EffectivePrefix;
// and residence is strict in every cross-store direction.
func assertRigPrefixDisjoint(t *testing.T, e splitEnv) {
	t.Helper()
	rigPrefix := e.cfg.Rigs[0].EffectivePrefix()
	if config.IsReservedClassPrefix(rigPrefix) {
		t.Fatalf("rig prefix %q is a reserved class prefix; rig beads are WORK beads", rigPrefix)
	}

	rigBead, err := e.rig.Create(beads.Bead{Title: "rig-scoped backlog item", Type: "task"})
	if err != nil {
		t.Fatalf("create rig bead: %v", err)
	}
	if !strings.HasPrefix(rigBead.ID, rigPrefix+"-") {
		t.Errorf("rig-store bead id %q does not carry the cfg-derived rig prefix %q-; by-id routing paths that consult cfg would disagree with the store", rigBead.ID, rigPrefix)
	}
	workBead, err := e.work.Create(beads.Bead{Title: "hq work item", Type: "task"})
	if err != nil {
		t.Fatalf("create work bead: %v", err)
	}
	if workPrefix := sling.BeadPrefix(workBead.ID); workPrefix == rigPrefix {
		t.Errorf("work and rig stores share the id prefix %q; the trio must stay pairwise disjoint", workPrefix)
	}

	// Residence: the rig bead resolves ONLY in the rig store.
	if _, err := e.work.Get(rigBead.ID); !errors.Is(err, beads.ErrNotFound) {
		t.Errorf("rig bead %s resolves in the WORK store (err=%v)", rigBead.ID, err)
	}
	if e.split {
		if _, err := e.infra.Get(rigBead.ID); !errors.Is(err, beads.ErrNotFound) {
			t.Errorf("rig bead %s resolves in the INFRA store (err=%v)", rigBead.ID, err)
		}
	}

	// Strict cross-prefix creates fail loud through the policy front doors in
	// the directions the base pin does not already cover: infra- and
	// work-shaped ids into the rig store, rig-shaped ids into work (and infra).
	if leaked, err := e.rig.Create(beads.Bead{ID: config.MintInfraBeadID("rigleak"), Title: "misrouted infra bead", Type: "task"}); err == nil {
		t.Errorf("rig front door accepted an infra-prefixed create (minted %q)", leaked.ID)
	}
	if leaked, err := e.rig.Create(beads.Bead{ID: sling.BeadPrefix(workBead.ID) + "-999", Title: "misrouted work bead", Type: "task"}); err == nil {
		t.Errorf("rig front door accepted a work-prefixed create (minted %q)", leaked.ID)
	}
	rigShapedID := rigPrefix + "-leak"
	if leaked, err := e.work.Create(beads.Bead{ID: rigShapedID, Title: "misrouted rig bead", Type: "task"}); err == nil {
		t.Errorf("work front door accepted a rig-prefixed create (minted %q)", leaked.ID)
	}
	if e.split {
		if leaked, err := e.infra.Create(beads.Bead{ID: rigShapedID, Title: "misrouted rig bead", Type: "task"}); err == nil {
			t.Errorf("infra front door accepted a rig-prefixed create (minted %q)", leaked.ID)
		}
	}
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

// TestSplitEnvFrontDoorDepAddStrictness pins the DepAdd half of the strict
// residence guard THROUGH the production policy stack (Create was pinned by
// assertPrefixDisjoint; DepAdd was the red-team gap). beadPolicyStore does not
// override DepAdd — the embedded Store delegates it to the strict leaf — so a
// cross-store dependency taken at a policy front door must fail with the
// bd-shaped "no issue found" error on the split topology. If a future policy
// wrapper grows a DepAdd override that resolves endpoints elsewhere (or
// swallows the failure), this pin breaks first, before a P3 invariant silently
// passes on leniency. The single-store subtest is the byte-identity half: one
// store resolves both endpoints, so the same call must succeed.
func TestSplitEnvFrontDoorDepAddStrictness(t *testing.T) {
	forEachTopology(t, func(t *testing.T, e splitEnv) {
		workBead, err := e.work.Create(beads.Bead{Title: "dep-strictness work bead", Type: "task"})
		if err != nil {
			t.Fatalf("create work bead: %v", err)
		}
		wisp := e.mintWisp(t, "dep-strictness wisp")

		err = e.work.DepAdd(workBead.ID, wisp.ID, "blocks")
		if !e.split {
			if err != nil {
				t.Fatalf("single-store front-door DepAdd(work → wisp) = %v, want success (one store resolves both endpoints)", err)
			}
			return
		}
		if err == nil || !strings.Contains(err.Error(), "no issue found") {
			t.Errorf("work front-door DepAdd(%s → %s) = %v, want the bd-shaped \"no issue found\" rejection through the policy stack", workBead.ID, wisp.ID, err)
		}
		// The convoy-tracks landmine direction: a graph-side bead referencing a
		// work-store bead through the graph front door must fail loud too.
		if err := e.graphStore().DepAdd(wisp.ID, workBead.ID, "tracks"); err == nil || !strings.Contains(err.Error(), "no issue found") {
			t.Errorf("graph front-door DepAdd(%s → %s) = %v, want the bd-shaped \"no issue found\" rejection through the policy stack", wisp.ID, workBead.ID, err)
		}
	})
}

// TestSplitEnvRigLegStagesWarmTickDemand proves the treadmill RCA driver state
// is stageable purely on the seam: a warm rig pool (live session) plus a
// routed wisp minted through the graph front door, fed to the production
// reconciler entry with the sessions store LEADING, exactly as
// CityRuntime.buildDesiredState wires it. Demand must read 1 in BOTH
// topologies — on split the wisp is infra-resident (the store-blind warm path
// read 0 here for hours on the live trace), on single-store it sits in the one
// store (legacy byte-identity).
func TestSplitEnvRigLegStagesWarmTickDemand(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		// Slot 2 (not 1): a pool whose first slot drained earlier is a valid
		// warm state, and the demand invariant must not key on slot numbering.
		sess, err := e.sessionsStore().Create(newWarmPoolSessionBead(e.qualified, "executor-2", "2"))
		if err != nil {
			t.Fatalf("create warm pool session bead: %v", err)
		}
		e.mintWispWith(t, wispOpts{title: "routed warm-tick demand wisp", routedTo: e.qualified})

		result := buildDesiredStateWithSessionBeads(
			"split-topology-city", e.cityPath, time.Now(), e.cfg, &localMockProvider{},
			e.sessionsStore(), e.rigStores, newSessionBeadSnapshot([]beads.Bead{sess}), nil, os.Stderr,
		)

		if got := result.ScaleCheckCounts[e.qualified]; got != 1 {
			t.Errorf("warm-tick routed-wisp demand = %d, want 1 (routed wisp in the leading store must stay visible while sessions run)", got)
		}
	})
}

// TestSplitEnvRigLegStagesClaimedWispPoolDemand proves the post-claim RCA
// state (treadmill site 2) is stageable in ONE mintWispWith call: a claimed
// in_progress infra wisp routed to the rig pool, run through the production
// pool-demand filter with the leading-store leg (store-ref ""). The topologies
// MUST diverge — the claimed wisp survives on a split city (its store is
// reachable from the rig-bound agent) and is dropped on a legacy city
// (byte-identity) — which is exactly the differential this fixture exists to
// hold on every such path.
func TestSplitEnvRigLegStagesClaimedWispPoolDemand(t *testing.T) {
	forEachTopologyWithRig(t, func(t *testing.T, e splitEnv) {
		wisp := e.mintWispWith(t, wispOpts{
			title:    "claimed routed wisp",
			routedTo: e.qualified,
			status:   "in_progress",
			assignee: "s-abc123",
		})
		if wisp.Status != "in_progress" || wisp.Assignee != "s-abc123" {
			t.Fatalf("mintWispWith staged status=%q assignee=%q, want in_progress/s-abc123", wisp.Status, wisp.Assignee)
		}

		filtered := filterAssignedWorkBeadsForPoolDemand(e.cfg, e.cityPath, nil, []beads.Bead{wisp}, []string{""})
		want := 0
		if e.split {
			want = 1
		}
		if len(filtered) != want {
			t.Errorf("pool-demand filter kept %d beads, want %d (claimed leading-store wisp must survive on split, drop on legacy)", len(filtered), want)
		}
	})
}
