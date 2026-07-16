package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory"
)

const reconcilerEffectInventoryOwningTest = "TestReconcilerEffectInventoryOnBoundHead"

// TestReconcilerEffectInventoryOnBoundHead proves that the canonical catalog
// and the production effect graph agree on an exact, clean execution head.
func TestReconcilerEffectInventoryOnBoundHead(t *testing.T) {
	if testing.Short() {
		t.Skip("canonical effect inventory analyzes five production build profiles")
	}

	workingDir, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	repoRoot := filepath.Clean(filepath.Join(workingDir, "../.."))
	resolvedRoot, err := gitOutput(context.Background(), repoRoot, "rev-parse", "--show-toplevel")
	if err != nil {
		t.Fatalf("resolve repository root: %v", err)
	}
	if !sameCleanPath(resolvedRoot, repoRoot) {
		t.Fatalf("resolved repository root = %q, want %q", resolvedRoot, repoRoot)
	}
	status, err := gitOutput(context.Background(), repoRoot, "status", "--porcelain=v1", "--untracked-files=all", "--ignore-submodules=none")
	if err != nil {
		t.Fatalf("inspect repository status: %v", err)
	}
	if status != "" {
		t.Skip("bound-head effect inventory requires an exact clean Git head")
	}
	revision, err := gitOutput(context.Background(), repoRoot, "rev-parse", "--verify", "HEAD^{commit}")
	if err != nil {
		t.Fatalf("resolve HEAD revision: %v", err)
	}

	registry, err := effectinventory.CanonicalRegistry()
	if err != nil {
		t.Fatalf("CanonicalRegistry: %v", err)
	}
	compiled, report, err := effectinventory.CompileCanonicalRegistry(context.Background(), effectinventory.CanonicalCompileRequest{
		RepoRoot:            repoRoot,
		ExpectedGitRevision: revision,
		Registry:            registry,
		AsOf:                time.Date(2026, time.July, 15, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatalf("CompileCanonicalRegistry: %v", err)
	}

	assertBoundHeadEffectEvidence(t, revision, report)
	assertBoundHeadEffectCounts(t, compiled)
	assertBoundHeadEffectClassifications(t, compiled)
}

func sameCleanPath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	return leftErr == nil && rightErr == nil && filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute)
}

func assertBoundHeadEffectEvidence(t *testing.T, revision string, report effectinventory.CanonicalDiscoveryReport) {
	t.Helper()
	if report.GitRevision != revision {
		t.Errorf("discovery revision = %q, want bound HEAD %q", report.GitRevision, revision)
	}
	if !strings.HasSuffix(report.GitHeadIdentity, "@"+revision) {
		t.Errorf("discovery HEAD identity = %q, want suffix @%s", report.GitHeadIdentity, revision)
	}
	if report.BoundaryDigest == "" || report.SourceScopeDigest == "" || report.EvidenceDigest == "" {
		t.Errorf("discovery digests must all be populated: boundary=%q scope=%q evidence=%q", report.BoundaryDigest, report.SourceScopeDigest, report.EvidenceDigest)
	}

	wantProfiles := map[effectinventory.BuildProfileID]bool{
		effectinventory.BuildDarwinDefault:  true,
		effectinventory.BuildDarwinNative:   true,
		effectinventory.BuildLinuxDefault:   true,
		effectinventory.BuildLinuxNative:    true,
		effectinventory.BuildWindowsCompile: true,
	}
	if len(report.Profiles) != len(wantProfiles) {
		t.Fatalf("discovery profiles = %d, want %d", len(report.Profiles), len(wantProfiles))
	}
	for _, profile := range report.Profiles {
		if !wantProfiles[profile.Profile] {
			t.Errorf("unexpected discovery profile %q", profile.Profile)
		}
		delete(wantProfiles, profile.Profile)
		if len(profile.Sites) == 0 || len(profile.SourceFiles) == 0 {
			t.Errorf("profile %q has sites/files = %d/%d, want both non-zero", profile.Profile, len(profile.Sites), len(profile.SourceFiles))
		}
	}
	if len(wantProfiles) != 0 {
		t.Errorf("missing discovery profiles: %v", wantProfiles)
	}
}

func assertBoundHeadEffectCounts(t *testing.T, compiled effectinventory.CompiledRegistry) {
	t.Helper()
	const (
		wantBoundaries    = 80
		wantRegistrations = 1102
		wantRoutes        = 1153
	)
	if got := len(compiled.Boundaries); got != wantBoundaries {
		t.Errorf("effect boundaries = %d, want %d", got, wantBoundaries)
	}
	if got := len(compiled.Registrations); got != wantRegistrations {
		t.Errorf("physical effect registrations = %d, want %d", got, wantRegistrations)
	}

	boundaryKinds := make(map[string]effectinventory.EffectKind, len(compiled.Boundaries))
	for _, boundary := range compiled.Boundaries {
		boundaryKinds[boundary.ID] = boundary.Kind
	}
	registrationsByKind := make(map[effectinventory.EffectKind]int)
	routes := 0
	for _, registration := range compiled.Registrations {
		kind, ok := boundaryKinds[registration.BoundaryID]
		if !ok {
			t.Errorf("registration %q refers to absent boundary %q", registration.ID, registration.BoundaryID)
			continue
		}
		registrationsByKind[kind]++
		for _, profileCase := range registration.Cases {
			routes += len(profileCase.Routes)
		}
	}
	if routes != wantRoutes {
		t.Errorf("logical effect routes = %d, want %d", routes, wantRoutes)
	}
	for kind, want := range map[effectinventory.EffectKind]int{
		effectinventory.KindStoreMutation:    414,
		effectinventory.KindProviderMutation: 241,
		effectinventory.KindProcessMutation:  54,
		effectinventory.KindEventEmission:    90,
		effectinventory.KindWakeSource:       303,
	} {
		if got := registrationsByKind[kind]; got != want {
			t.Errorf("%s physical registrations = %d, want %d", kind, got, want)
		}
	}
}

func assertBoundHeadEffectClassifications(t *testing.T, compiled effectinventory.CompiledRegistry) {
	t.Helper()
	routeRecovery := 0
	rawStoreBypasses := 0
	providerInternalProcessEffects := 0
	for _, registration := range compiled.Registrations {
		if registration.BoundaryID == "beads.writer.SetMetadata" &&
			registration.Matcher.Enclosing.Object.Name == "restoreCarriedWorkRoutes" {
			routeRecovery++
			for _, profileCase := range registration.Cases {
				for _, route := range profileCase.Routes {
					definition := route.Definition
					if definition.StoreDomain != effectinventory.StoreDomainRouteRecovery ||
						definition.ActionFamily != effectinventory.FamilyRouteRecovery ||
						definition.ExecutingProcess != effectinventory.ProcessController ||
						definition.AccessPath != effectinventory.AccessRawStoreBypass ||
						definition.Disposition.Kind != effectinventory.DispositionReplaceAtGate ||
						!slices.Contains(definition.Disposition.Gates, effectinventory.TaskRef("P9.4")) {
						t.Errorf("route-recovery classification = %#v, want controller raw-store bypass replaced at P9.4", definition)
					}
				}
			}
		}
		if strings.HasPrefix(registration.Matcher.Enclosing.Object.Package, "github.com/gastownhall/gascity/internal/runtime") &&
			isProcessBoundary(compiled.Boundaries, registration.BoundaryID) {
			providerInternalProcessEffects++
		}
		for _, profileCase := range registration.Cases {
			for _, route := range profileCase.Routes {
				if route.Definition.AccessPath == effectinventory.AccessRawStoreBypass {
					rawStoreBypasses++
				}
				if !ownsBoundHeadInventoryTest(route.Definition.OwningTests) {
					t.Errorf("route %q does not cite %s as owning evidence", route.ID, reconcilerEffectInventoryOwningTest)
				}
			}
		}
	}
	if routeRecovery != 1 {
		t.Errorf("route-recovery physical registrations = %d, want 1", routeRecovery)
	}
	if rawStoreBypasses == 0 {
		t.Error("canonical inventory contains no raw-store bypass routes")
	}
	if providerInternalProcessEffects == 0 {
		t.Error("canonical inventory contains no provider-internal kill/signal effects")
	}
}

func isProcessBoundary(boundaries []effectinventory.BoundaryDefinition, boundaryID string) bool {
	for _, boundary := range boundaries {
		if boundary.ID == boundaryID {
			return boundary.Kind == effectinventory.KindProcessMutation
		}
	}
	return false
}

func ownsBoundHeadInventoryTest(tests []effectinventory.TestRef) bool {
	for _, test := range tests {
		if test.Package == "github.com/gastownhall/gascity/cmd/gc" && test.Name == reconcilerEffectInventoryOwningTest {
			return true
		}
	}
	return false
}
