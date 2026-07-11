package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// Landmine #19 (probe-wisp-prefix): bd's wisp tier mints infra-scope ids as
// <issue_prefix>-wisp-<suffix> ("gcg-wisp-a7gc"), not <issue_prefix>-<suffix>.
// The by-id store-ownership predicates used to classify ids with the
// config-free sling.BeadPrefix heuristic, whose answer for a wisp id depends
// on the SUFFIX SHAPE: a digit-bearing suffix ("a7gc", "0042") parses to
// prefix "gcg-wisp" — not a reserved class prefix, so the id routed to the
// WORK store — while a letter-only suffix ("znyf") fails the bead-hash gate
// and falls back to the first dash segment "gcg", which IS reserved, routing
// to the infra store. Both shapes coexisted on the live incident city: the
// observed claim on gcg-wisp-znyf landed in the infra store, while the same
// claim on a digit-suffix sibling would have run `bd update --claim` against
// the work store and failed "not found".
//
// These tests pin every production by-id routing path to the namespace rule
// (config.IsReservedClassBeadID — first dash segment, matching the
// controller's beadEventConfiguredStoreLocked prefix+"-" scan): ALL wisp-tier
// infra ids route to the infra store on a split city, and single-store
// routing stays byte-identical.

// productionWispIDShapes are the id shapes bd's wisp tier actually mints
// under the infra scope, drawn from live observations (the maintainer-city
// molecule root gcg-wisp-a7gc and claimed step gcg-wisp-znyf), the bd docs'
// example suffix (dv78), and the conformance harness's deterministic numeric
// shape (splitWispID). The suffix shapes deliberately straddle the
// sling.BeadPrefix heuristic's hash gate.
var productionWispIDShapes = []struct {
	name string
	id   string
}{
	{"digit-bearing hash suffix (live molecule root)", "gcg-wisp-a7gc"},
	{"digit-bearing hash suffix (bd doc example)", "gcg-wisp-dv78"},
	{"numeric suffix (harness splitWispID shape)", "gcg-wisp-0042"},
	{"letter-only hash suffix (pre-fix lucky shape)", "gcg-wisp-znyf"},
}

// TestClaimableStoreRoutesWispIDsToInfra pins claimableStore.storeForID — the
// composite read side (Get, attached-workflow-root resolution) — on
// production-shaped wisp ids.
func TestClaimableStoreRoutesWispIDsToInfra(t *testing.T) {
	work := beads.NewMemStoreHonoringIDs()
	infra := beads.NewMemStoreHonoringIDs()

	split := &claimableStore{work: work, infra: infra}
	for _, tt := range productionWispIDShapes {
		if split.storeForID(tt.id) != beads.Store(infra) {
			t.Errorf("%s: storeForID(%q) routed to the WORK store; wisp-tier infra ids must route to the infra store on a split city", tt.name, tt.id)
		}
	}
	if split.storeForID("ga-jaudf8") != beads.Store(work) {
		t.Error("work-class id ga-jaudf8 must stay on the work store")
	}

	// Single-store: byte-identical — every id collapses to the work store.
	single := &claimableStore{work: work, infra: nil}
	for _, tt := range productionWispIDShapes {
		if single.storeForID(tt.id) != beads.Store(work) {
			t.Errorf("%s: storeForID(%q) must collapse to the work store on a single-store city", tt.name, tt.id)
		}
	}
}

// TestHookClaimTargetsInfraOnWispIDs pins the claim-time mutation router
// (gc hook --claim → splitCityHookClaimOps → hookClaimTargetsInfra): a wisp
// claim must land in the infra store, not the work store the winning
// hookStore points at.
func TestHookClaimTargetsInfraOnWispIDs(t *testing.T) {
	splitCity := filepath.Join(t.TempDir(), "split")
	seedSplitCityInfraMarker(t, splitCity)
	singleCity := t.TempDir()

	for _, tt := range productionWispIDShapes {
		if !hookClaimTargetsInfra(splitCity, tt.id) {
			t.Errorf("%s: hookClaimTargetsInfra(split, %q) = false; the claim mutation would run `bd update --claim` against the work store and fail \"not found\"", tt.name, tt.id)
		}
		if hookClaimTargetsInfra(singleCity, tt.id) {
			t.Errorf("%s: hookClaimTargetsInfra(single-store, %q) = true, want false (byte-identical single-store routing)", tt.name, tt.id)
		}
	}
	if hookClaimTargetsInfra(splitCity, "ga-jaudf8") {
		t.Error("work-class id ga-jaudf8 must not be routed to the infra store")
	}
}

// TestResolveBdScopeTargetRoutesWispIDsToInfra pins the `gc bd` front door: a
// by-id op on a wisp id (`gc bd update gcg-wisp-… --set-metadata …`) must exec
// bd against the infra scope on a split city.
func TestResolveBdScopeTargetRoutesWispIDsToInfra(t *testing.T) {
	// The infra arm routes by prefix without a work-store existence probe; fail
	// the probe so a regression that reintroduces one is caught (mirrors
	// TestResolveBdScopeTargetInfraArm).
	origProbe := bdBeadExists
	defer func() { bdBeadExists = origProbe }()
	bdBeadExists = func(string, execStoreTarget, string) bool { return false }
	t.Setenv("GC_RIG", "")

	cfg := &config.City{
		Workspace: config.Workspace{Name: "gascity"},
		Rigs: []config.Rig{
			{Name: "wren", Path: filepath.Join("rigs", "wren"), Prefix: "projectwrenunity"},
		},
	}

	splitCity := filepath.Join(t.TempDir(), "split")
	seedSplitCityInfraMarker(t, splitCity)
	singleCity := filepath.Join(t.TempDir(), "single")

	for _, tt := range productionWispIDShapes {
		got, err := resolveBdScopeTarget(cfg, splitCity, "", []string{"update", tt.id, "--set-metadata", "review.verdict=pass"}, false)
		if err != nil {
			t.Fatalf("%s: resolveBdScopeTarget() error = %v", tt.name, err)
		}
		want := execStoreTarget{
			ScopeRoot: infraScopeRoot(splitCity),
			ScopeKind: "infra",
			Prefix:    config.InfraScopePrefix,
		}
		if got != want {
			t.Errorf("%s: resolveBdScopeTarget(update %s) = %#v, want infra scope %#v", tt.name, tt.id, got, want)
		}

		single, err := resolveBdScopeTarget(cfg, singleCity, "", []string{"update", tt.id, "--set-metadata", "review.verdict=pass"}, false)
		if err != nil {
			t.Fatalf("%s: resolveBdScopeTarget(single-store) error = %v", tt.name, err)
		}
		if single.ScopeKind != "city" || single.ScopeRoot != singleCity {
			t.Errorf("%s: resolveBdScopeTarget(single-store) = %#v, want city scope %q (byte-identical)", tt.name, single, singleCity)
		}
	}
}

// TestSlingSourceStoreRootWispIDsResolveInfraScope pins the sling source-bead
// probe: `gc sling <gcg-wisp-…>` must open the infra store that actually
// holds the wisp on a split city, and keep the historical no-match answer on
// a single-store city.
func TestSlingSourceStoreRootWispIDsResolveInfraScope(t *testing.T) {
	splitCity := filepath.Join(t.TempDir(), "split")
	seedSplitCityInfraMarker(t, splitCity)
	singleCity := t.TempDir()
	cfg := &config.City{Workspace: config.Workspace{Name: "split-city", Prefix: "ga"}}

	wantDir := resolveStoreScopeRoot(splitCity, infraScopeRoot(splitCity))
	for _, tt := range productionWispIDShapes {
		storeDir, prefix, ok := slingSourceStoreRootForCandidate(cfg, splitCity, tt.id)
		if !ok || storeDir != wantDir {
			t.Errorf("%s: slingSourceStoreRootForCandidate(split, %q) = (%q, %q, %v), want (%q, _, true)", tt.name, tt.id, storeDir, prefix, ok, wantDir)
			continue
		}
		if prefix != config.InfraScopePrefix {
			t.Errorf("%s: slingSourceStoreRootForCandidate(split, %q) prefix = %q, want the owning reserved class prefix %q", tt.name, tt.id, prefix, config.InfraScopePrefix)
		}

		// Single-store: no infra arm, no HQ/rig prefix match — historical
		// no-candidate answer, byte-identical.
		if _, _, ok := slingSourceStoreRootForCandidate(cfg, singleCity, tt.id); ok {
			t.Errorf("%s: slingSourceStoreRootForCandidate(single-store, %q) matched a store, want no match", tt.name, tt.id)
		}
	}
}

// TestOpenRigAwareStoreRoutesWispIDsToInfraStore pins the `gc graph` front
// door: `gc graph <gcg-wisp-…>` must resolve the infra store that holds the
// wisp DAG instead of NotFound-ing against the city work store.
func TestOpenRigAwareStoreRoutesWispIDsToInfraStore(t *testing.T) {
	resetFlags(t)
	t.Setenv("GC_BEADS", "file")

	cityDir := setupCity(t, "graph-city")
	seedSplitCityInfraMarker(t, cityDir)

	infraLeaf := beads.NewMemStoreHonoringIDs()
	const wispID = "gcg-wisp-a7gc"
	mustCreateBead(t, infraLeaf, beads.Bead{ID: wispID, Title: "wisp root", Status: "open", Type: "task"})
	restore := swapCachedInfraStoreOpen(func(string) (beads.Store, bool, error) { return infraLeaf, true, nil })
	t.Cleanup(restore)
	t.Cleanup(func() { clearInfraStoreCacheKey(cityDir) })

	setCwd(t, cityDir)
	var stderr bytes.Buffer
	store, _, code := openRigAwareStore([]string{wispID}, &stderr)
	if code != 0 {
		t.Fatalf("openRigAwareStore() = %d, stderr = %s", code, stderr.String())
	}
	if _, err := store.Get(wispID); err != nil {
		t.Fatalf("gc graph resolved wisp id %s against the wrong store (Get: %v); the DAG lives in the infra store on a split city", wispID, err)
	}
}
