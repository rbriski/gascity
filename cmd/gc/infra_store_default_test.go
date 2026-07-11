package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/fsys"
)

// TestInitShouldSeedInfraStoreDefault pins the owner decision that NEW cities
// default to the domain/infra two-store split: initShouldSeedInfraStore returns
// true with no env override, false only when GC_INFRA_STORE_SPLIT is explicitly
// falsy, and false when the per-invocation --single-store opt-out (forceSingleStore)
// is set. This is the single gate `gc init` consults, so it locks the flip in.
func TestInitShouldSeedInfraStoreDefault(t *testing.T) {
	cases := []struct {
		name             string
		env              string // "" means leave GC_INFRA_STORE_SPLIT unset
		setEnv           bool
		forceSingleStore bool
		want             bool
	}{
		{name: "default_no_env_two_store", want: true},
		{name: "explicit_0_single_store", env: "0", setEnv: true, want: false},
		{name: "explicit_false_single_store", env: "false", setEnv: true, want: false},
		{name: "explicit_no_single_store", env: "no", setEnv: true, want: false},
		{name: "explicit_off_single_store", env: "off", setEnv: true, want: false},
		{name: "explicit_1_two_store", env: "1", setEnv: true, want: true},
		{name: "explicit_true_two_store", env: "true", setEnv: true, want: true},
		{name: "empty_env_two_store", env: "", setEnv: true, want: true},
		{name: "force_single_store_flag_overrides_default", forceSingleStore: true, want: false},
		{name: "force_single_store_flag_overrides_env_1", env: "1", setEnv: true, forceSingleStore: true, want: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.setEnv {
				t.Setenv("GC_INFRA_STORE_SPLIT", tc.env)
			} else {
				// t.Setenv restores on cleanup; an empty value with setEnv=false
				// means "explicitly unset for this case".
				t.Setenv("GC_INFRA_STORE_SPLIT", "")
			}
			if got := initShouldSeedInfraStore(tc.forceSingleStore); got != tc.want {
				t.Errorf("initShouldSeedInfraStore(%v) with GC_INFRA_STORE_SPLIT=%q = %v, want %v",
					tc.forceSingleStore, tc.env, got, tc.want)
			}
		})
	}
}

// TestDoInitSeedsInfraStoreByDefault pins the end-to-end default at the `gc init`
// boundary: a plain init on a bd-backed city seeds the .gc/infra scope (two-store),
// while --single-store (wiz.singleStore) suppresses it (single-store). It runs the
// deferred seed only (GC_DOLT=skip), so it writes the canonical infra scope config
// without needing a live Dolt server — cityHasInfraStore is the activation marker.
func TestDoInitSeedsInfraStoreByDefault(t *testing.T) {
	t.Run("default_seeds_infra_scope", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		t.Setenv("GC_DOLT", "skip")
		configureIsolatedRuntimeEnv(t)

		cityPath := filepath.Join(t.TempDir(), "two-store-city")
		var stdout, stderr bytes.Buffer
		if code := doInit(fsys.OSFS{}, cityPath, defaultWizardConfig(), "", &stdout, &stderr, false); code != 0 {
			t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
		}
		if !cityHasInfraStore(cityPath) {
			t.Fatalf("plain gc init did not seed the infra scope; cityHasInfraStore = false (two-store is the default)")
		}
	})

	t.Run("single_store_flag_suppresses_infra_scope", func(t *testing.T) {
		t.Setenv("GC_BEADS", "bd")
		t.Setenv("GC_DOLT", "skip")
		configureIsolatedRuntimeEnv(t)

		cityPath := filepath.Join(t.TempDir(), "single-store-city")
		wiz := defaultWizardConfig()
		wiz.singleStore = true // the effect of `gc init --single-store`
		var stdout, stderr bytes.Buffer
		if code := doInit(fsys.OSFS{}, cityPath, wiz, "", &stdout, &stderr, false); code != 0 {
			t.Fatalf("doInit = %d, want 0; stderr: %s", code, stderr.String())
		}
		if cityHasInfraStore(cityPath) {
			t.Fatalf("gc init --single-store seeded an infra scope; cityHasInfraStore = true (must stay single-store)")
		}
	})
}
