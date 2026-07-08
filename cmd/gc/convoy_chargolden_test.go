package main

import (
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/chartest"
)

// TestConvoyList_CharacterizationGolden freezes the current per-lane behavior of
// `gc convoy list` across the three routing lanes (remote / local-controller-
// alive / serverless). It is the pilot proving the three-lane harness end to
// end; later unification moves must reproduce each lane's golden byte-for-byte
// (human text) after canonicalization. Regenerate with -chartest-update.
//
// FINDING (surfaced by this pilot): with >1 convoy, `gc convoy list --json`
// emits the convoys array in NON-DETERMINISTIC order (the human table sorts by
// id; the --json renderer preserves the store/API iteration order, which is not
// stable). The pilot therefore seeds a single convoy — enough to prove the
// harness (cross-surface identity, A==B, lane telemetry, boundary counts);
// distinct-token numbering is unit-tested in internal/chartest. Multi-element
// JSON list-order is a shape-comparison concern for the differ (Phase 0.7) and
// must be pinned when convoy list actually migrates (Phase 2) — either the
// unified path sorts --json deterministically, or the differ compares the array
// order-insensitively under the locked "JSON shape+additive" safety bar.
func TestConvoyList_CharacterizationGolden(t *testing.T) {
	h := newCharHarness(t, "Alpha convoy")
	for _, lane := range h.lanes(t) {
		t.Run(lane.name, func(t *testing.T) {
			got := h.captureLane(t, lane).Golden()
			path := filepath.Join("testdata", "chargolden", "convoy-list-"+lane.name+".golden")
			chartest.CompareGolden(t, path, got)
		})
	}
}
