package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileRejectsEffectInExplicitInitAsUnrepresentable(t *testing.T) {
	config := initBoundaryFixtureConfig(t, "explicit")

	observed, err := discoverProfile(context.Background(), config, initBoundaryLinuxProfile(), []BoundaryDefinition{initBoundaryEmitDefinition()})
	if err == nil {
		t.Fatalf("discoverProfile() = %#v, nil; want an unrepresentable package-initialization error", observed)
	}
	assertInitBoundaryDiagnostic(t, err, "explicit", "effectful package initialization", "FunctionRef")
}

func TestDiscoverProfileRejectsEffectfulHelperReachedByPackageInitializer(t *testing.T) {
	config := initBoundaryFixtureConfig(t, "globalhelper")

	observed, err := discoverProfile(context.Background(), config, initBoundaryLinuxProfile(), []BoundaryDefinition{initBoundaryEmitDefinition()})
	if err == nil {
		t.Fatalf("discoverProfile() = %#v, nil; want a package-initialization route error", observed)
	}
	assertInitBoundaryDiagnostic(t, err, "globalhelper", "Initialize", "package initialization")
}

func TestDiscoverProfileRejectsSameFileInitFunctionsInsteadOfColliding(t *testing.T) {
	config := initBoundaryFixtureConfig(t, "multiple")

	observed, err := discoverProfile(context.Background(), config, initBoundaryLinuxProfile(), []BoundaryDefinition{initBoundaryEmitDefinition()})
	if err == nil {
		t.Fatalf("discoverProfile() = %#v, nil; want same-file init functions to be rejected before they can share a locator", observed)
	}
	assertInitBoundaryDiagnostic(t, err, "multiple", "effectful package initialization", "FunctionRef")
}

func TestDiscoverProfileRejectsPromotedBoundaryReference(t *testing.T) {
	config := initBoundaryFixtureConfig(t, "promotion")
	boundary := initBoundaryMethodDefinition("Outer")

	observed, err := discoverProfile(context.Background(), config, initBoundaryLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatalf("discoverProfile() = %#v, nil; want promoted boundary reference to be rejected", observed)
	}
	assertInitBoundaryDiagnostic(t, err, "Outer.Affect", "promoted", "declaring receiver")
}

func TestDiscoverProfileResolvesBoundaryAtDeclaringReceiver(t *testing.T) {
	config := initBoundaryFixtureConfig(t, "promotion")
	boundary := initBoundaryMethodDefinition("Declaring")

	observed, err := discoverProfile(context.Background(), config, initBoundaryLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall("initboundary.affect", "initboundary/promotion", "promotion.go", "DirectRoute", nil, OperationCall, 1),
		fixtureCall("initboundary.affect", "initboundary/promotion", "promotion.go", "PromotedRoute", nil, OperationCall, 1),
	})
}

func initBoundaryFixtureConfig(t *testing.T, packageName string) analysisConfig {
	t.Helper()
	return fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/initboundary/" + packageName,
	})
}

func initBoundaryLinuxProfile() analysisProfile {
	return analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
}

func initBoundaryEmitDefinition() BoundaryDefinition {
	return BoundaryDefinition{
		ID:     "boundary.emit",
		Kind:   KindProviderMutation,
		Object: ObjectRef{Package: fixtureModulePath + "/boundary", Name: "Emit"},
		Match:  ObjectMatchExact,
	}
}

func initBoundaryMethodDefinition(receiver string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:   "initboundary.affect",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/initboundary/promotion",
			Receiver: receiver,
			Name:     "Affect",
		},
		Match: ObjectMatchExact,
	}
}

func assertInitBoundaryDiagnostic(t *testing.T, err error, fragments ...string) {
	t.Helper()
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("discoverProfile() error = %q, want fragment %q", err, fragment)
		}
	}
}
