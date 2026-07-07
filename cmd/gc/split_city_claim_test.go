package main

import "testing"

func TestClaimRoutesInfraMutationByPrefix(t *testing.T) {
	split := markSplitCity(t)   // has the .gc/infra activation marker
	single := t.TempDir()       // no marker

	cases := []struct {
		name     string
		cityPath string
		beadID   string
		want     bool
	}{
		{"split + gcg step routes to infra", split, "gcg-abc123", true},
		{"split + work bead stays on work", split, "ga-abc123", false},
		{"single-store + gcg stays on work", single, "gcg-abc123", false},
		{"single-store + work bead stays on work", single, "ga-abc123", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := hookClaimTargetsInfra(tc.cityPath, tc.beadID); got != tc.want {
				t.Fatalf("hookClaimTargetsInfra(%q, %q) = %v, want %v", tc.cityPath, tc.beadID, got, tc.want)
			}
		})
	}

	// The dir/env router keeps a work-class id (and single-store cities) on the
	// passed dir/env verbatim — no infra redirect.
	dir, env := hookClaimInfraDirEnv(single, nil, "gcg-abc123", "/work/dir", []string{"K=V"})
	if dir != "/work/dir" || len(env) != 1 || env[0] != "K=V" {
		t.Fatalf("single-store must not redirect: dir=%q env=%v", dir, env)
	}
}
