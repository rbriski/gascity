package main

import (
	"bytes"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

// TestInferSling1ArgTarget_FormulaRejected exercises the extracted 1-arg
// target-inference helper directly (its pure --formula guard needs no store),
// demonstrating that hoisting the store-touching pre-core orchestration out of
// cmdSlingWithJSON makes it independently testable.
func TestInferSling1ArgTarget_FormulaRejected(t *testing.T) {
	target, _, errCode, errMsg := inferSling1ArgTarget(&config.City{}, "/tmp/nonexistent", "some-bead", true)
	if target != "" || errCode != "invalid_arguments" || errMsg == "" {
		t.Fatalf("isFormula 1-arg: got (target=%q code=%q msg=%q), want (\"\", invalid_arguments, non-empty)", target, errCode, errMsg)
	}
}

// TestSlingTargetIndexSeam proves the injectable slingTargetIndex seam makes the
// otherwise-random 1-arg default_sling_targets selection deterministic for tests
// and future sling characterization, and restores the production (rand) picker.
func TestSlingTargetIndexSeam(t *testing.T) {
	restore := SetSlingTargetIndexForTest(func(n int) int { return n - 1 }) // always the last target
	if got := slingTargetIndex(3); got != 2 {
		t.Fatalf("override: slingTargetIndex(3) = %d, want 2", got)
	}
	restore()
	// Restored picker returns a valid in-range index (production math/rand).
	for i := 0; i < 50; i++ {
		if got := slingTargetIndex(3); got < 0 || got > 2 {
			t.Fatalf("restored: slingTargetIndex(3) = %d, out of [0,3)", got)
		}
	}
}

// TestCmdSlingMultiDefaultTargets_DeterministicPick uses the seam to prove the
// exact target a 1-arg `gc sling <bead>` routes to from a multi-entry
// default_sling_targets list — a stronger assertion than the existing
// "accept either" test, now that the random pick is injectable.
func TestCmdSlingMultiDefaultTargets_DeterministicPick(t *testing.T) {
	for _, tc := range []struct {
		name string
		idx  int
		want string
	}{
		{"first", 0, "foundations/worker-a"},
		{"second", 1, "foundations/worker-b"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cityDir, rigDir := setupCmdSlingMultiDefaultTargetsFixture(t,
				[]string{"foundations/worker-a", "foundations/worker-b"})
			restore := SetSlingTargetIndexForTest(func(int) int { return tc.idx })
			defer restore()

			var stdout, stderr bytes.Buffer
			code := cmdSling(
				[]string{"fo-multi-work"},
				false, false, false,
				"", nil, "",
				true, false, false, "",
				false, false, false,
				"", "",
				&stdout, &stderr,
			)
			if code != 0 {
				t.Fatalf("cmdSling = %d, want 0; stderr=%s", code, stderr.String())
			}
			rigStore, err := openStoreAtForCity(rigDir, cityDir)
			if err != nil {
				t.Fatalf("openStoreAtForCity: %v", err)
			}
			routed, err := rigStore.Get("fo-multi-work")
			if err != nil {
				t.Fatalf("Get(fo-multi-work): %v", err)
			}
			if got := routed.Metadata["gc.routed_to"]; got != tc.want {
				t.Fatalf("idx=%d: gc.routed_to = %q, want %q", tc.idx, got, tc.want)
			}
		})
	}
}
