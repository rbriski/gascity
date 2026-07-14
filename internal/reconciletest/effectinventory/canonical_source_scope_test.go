package effectinventory

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCanonicalProfileRejectsStaleRouteHopEvidenceDeterministically(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/canonicalroute",
	})
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	registry := Registry{Registrations: []SiteRegistration{routeHopRegistration(leaf,
		routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
			routeHop("DuplicateOwner", OperationCall, 3, HopDispatchExact, leaf)),
	)}}

	var baseline string
	for repetition := 0; repetition < 2; repetition++ {
		_, err := runCanonicalProfile(context.Background(), config, fixtureLinuxProfile(), registry)
		if err == nil {
			t.Fatal("runCanonicalProfile() error = nil, want stale route-hop rejection")
		}
		for _, want := range []string{
			`effect route-hop evidence failed for profile "linux/default"`,
			"stale ordinal 3",
			"loaded profile has only 2",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("runCanonicalProfile() error = %q, want %q", err, want)
			}
		}
		if baseline == "" {
			baseline = err.Error()
			continue
		}
		if err.Error() != baseline {
			t.Fatalf("canonical route-hop diagnostic changed\n got:\n%s\nwant:\n%s", err, baseline)
		}
	}
}

func TestRunCanonicalProfileRejectsStaleTargetGateEvidenceDeterministically(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/canonicalroute",
	})
	leaf := routeHopFixtureRef("", "Leaf", "routehops.go", nil)
	route := routeHopRoute(routeHopFixtureRef("", "DuplicateOwner", "routehops.go", nil),
		routeHop("DuplicateOwner", OperationCall, 2, HopDispatchExact, leaf))
	route.CurrentGate = GateRef{
		Kind:      GatePredicate,
		Predicate: ObjectRef{Package: routeHopFixturePackage, Name: "MissingPredicate"},
		Expected:  "true",
	}
	registry := Registry{
		Boundaries: []BoundaryDefinition{{
			ID:     "route-hop.fixture",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: routeHopFixturePackage, Name: "Leaf"},
			Match:  ObjectMatchExact,
		}},
		Registrations: []SiteRegistration{routeHopRegistration(leaf, route)},
	}

	var baseline string
	for repetition := 0; repetition < 2; repetition++ {
		_, err := runCanonicalProfile(context.Background(), config, fixtureLinuxProfile(), registry)
		if err == nil {
			t.Fatal("runCanonicalProfile() error = nil, want stale target/gate rejection")
		}
		for _, want := range []string{
			`effect target/gate evidence failed for profile "linux/default"`,
			"MissingPredicate",
			"does not exist",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("runCanonicalProfile() error = %q, want %q", err, want)
			}
		}
		if baseline == "" {
			baseline = err.Error()
			continue
		}
		if err.Error() != baseline {
			t.Fatalf("canonical target/gate diagnostic changed\n got:\n%s\nwant:\n%s", err, baseline)
		}
	}
}

func TestSourceScopeManifestDetectsContentDrift(t *testing.T) {
	repoRoot := t.TempDir()
	relative := "cmd/gc/main.go"
	filename := filepath.Join(repoRoot, filepath.FromSlash(relative))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filename, []byte("package main\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	fingerprints, err := fingerprintSourcePaths(repoRoot, []string{relative})
	if err != nil {
		t.Fatalf("fingerprintSourcePaths() error: %v", err)
	}
	runs := make([]canonicalProfileRun, len(canonicalAnalysisProfiles()))
	for index, profile := range canonicalAnalysisProfiles() {
		runs[index] = canonicalProfileRun{
			profile: profile.ID,
			files:   append([]sourceFingerprint(nil), fingerprints...),
		}
	}
	manifest, err := buildSourceScopeManifest(
		analysisConfig{ModulePath: canonicalModulePath, Patterns: canonicalProductionAnalysisPatterns()},
		canonicalSourceAuditRoots(),
		sourceAudit{files: append([]sourceFingerprint(nil), fingerprints...)},
		runs,
	)
	if err != nil {
		t.Fatalf("buildSourceScopeManifest() error: %v", err)
	}
	if err := verifySourceScopeManifest(repoRoot, manifest); err != nil {
		t.Fatalf("verifySourceScopeManifest() rejected unchanged scope: %v", err)
	}

	if err := os.WriteFile(filename, []byte("package main\n// changed\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err = verifySourceScopeManifest(repoRoot, manifest)
	if err == nil || !strings.Contains(err.Error(), "changed during analysis") {
		t.Fatalf("verifySourceScopeManifest() error = %v, want content-drift rejection", err)
	}
}

func TestSourceScopeDigestBindsEveryProfileFileSet(t *testing.T) {
	baseRuns := make([]canonicalProfileRun, len(canonicalAnalysisProfiles()))
	for index, profile := range canonicalAnalysisProfiles() {
		baseRuns[index] = canonicalProfileRun{
			profile: profile.ID,
			files:   []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")},
		}
	}
	build := func(runs []canonicalProfileRun) sourceScopeManifest {
		t.Helper()
		manifest, err := buildSourceScopeManifest(
			analysisConfig{ModulePath: canonicalModulePath, Patterns: canonicalProductionAnalysisPatterns()},
			canonicalSourceAuditRoots(),
			sourceAudit{files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}},
			runs,
		)
		if err != nil {
			t.Fatalf("buildSourceScopeManifest() error: %v", err)
		}
		return manifest
	}
	baseline := build(baseRuns)
	changedRuns := cloneCanonicalProfileRuns(baseRuns)
	changedRuns[3].files[0].digest = strings.Repeat("b", 64)
	changed := build(changedRuns)
	if changed.digest == baseline.digest {
		t.Fatalf("source-scope digest stayed %q after one profile's content changed", baseline.digest)
	}
}
