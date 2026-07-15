package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverClosedWorldProfileIgnoresUnreachableProvenanceSources(t *testing.T) {
	const packageName = "closedworld/dead"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true

	observed, err := discoverProfile(
		context.Background(),
		config,
		fixtureLinuxProfile(),
		closedWorldFixtureBoundaries(packageName),
	)
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, nil)
}

func TestDiscoverClosedWorldProfileInventoriesSourcesReachableFromMain(t *testing.T) {
	const packageName = "closedworld/reachable"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundaries := closedWorldFixtureBoundaries(packageName)

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), boundaries)
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundaries[0].ID, packageName, "main.go", "invokeCallback", nil, OperationCall, 1),
		fixtureCall(boundaries[0].ID, packageName, "main.go", "main", nil, OperationCall, 1),
		fixtureCall(boundaries[1].ID, packageName, "main.go", "receiveParameter", nil, OperationSelectReceive, 1),
		fixtureCall(boundaries[2].ID, packageName, "main.go", "receiveRegistered", nil, OperationSelectReceive, 1),
	})
}

func TestDiscoverClosedWorldProfileSeparatesRuntimeEffectsFromInitialization(t *testing.T) {
	const packageName = "closedworld/initseparation"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := closedWorldCallbackBoundary(packageName)

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "main.go", "runtimeRoute", nil, OperationCall, 1),
	})
}

func TestDiscoverClosedWorldProfileFailsClosedForInitReachableEffect(t *testing.T) {
	const packageName = "closedworld/initreachable"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := closedWorldCallbackBoundary(packageName)

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want init-route diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, want := range []string{
		"closedworld/initreachable..initRoute",
		"effectful package initialization route has no injective FunctionRef",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func TestDiscoverClosedWorldProfileTracesClosedCallableProvenanceShapes(t *testing.T) {
	const packageName = "closedworld/callableprovenance"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true

	observed, err := discoverProfile(
		context.Background(),
		config,
		fixtureLinuxProfile(),
		closedWorldCallableProvenanceBoundaries(packageName),
	)
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	boundaries := closedWorldCallableProvenanceBoundaries(packageName)
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundaries[0].ID, packageName, "main.go", "startReserved", []int{1}, OperationCall, 1),
		fixtureMethodCall(boundaries[1].ID, packageName, "main.go", "provider", "shutdown", nil, OperationCall, 1),
		fixtureMethodCall(boundaries[2].ID, packageName, "main.go", "directoryHooks", "install", []int{1}, OperationCall, 1),
		fixtureMethodCall(boundaries[2].ID, packageName, "main.go", "directoryHooks", "opening", nil, OperationCall, 1),
	})
}

func TestDiscoverClosedWorldProfileRejectsExternalCallableResult(t *testing.T) {
	const packageName = "closedworld/externalresult"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := BoundaryDefinition{
		ID:     "closed-world.external-result-effect",
		Kind:   KindProviderMutation,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Effect"},
		Match:  ObjectMatchExact,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want unresolved external callable-result diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, want := range []string{"main", "unresolved effect-compatible dynamic call"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func TestDiscoverClosedWorldProfileRejectsAliasedPrivateFieldWrite(t *testing.T) {
	const packageName = "closedworld/fieldalias"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := BoundaryDefinition{
		ID:     "closed-world.field-alias-effect",
		Kind:   KindProviderMutation,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Effect"},
		Match:  ObjectMatchExact,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil && len(observed) == 0 {
		t.Fatal("discoverProfile() silently pruned a callable written through an aliased private-field address")
	}
	if err == nil && (len(observed) != 1 || observed[0].BoundaryID != boundary.ID) {
		t.Fatalf("discoverProfile() observed = %#v, want the aliased boundary or a fail-closed diagnostic", observed)
	}
}

func closedWorldFixtureBoundaries(packageName string) []BoundaryDefinition {
	packagePath := fixtureModulePath + "/" + packageName
	return []BoundaryDefinition{
		{
			ID:     "closed-world.callback-effect",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: packagePath, Name: "CallbackEffect"},
			Match:  ObjectMatchExact,
		},
		{
			ID:     "closed-world.approved-channel",
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: packagePath, Name: "approved"},
			Match:  ObjectMatchChannel,
		},
		{
			ID:     "closed-world.registered-channel",
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: packagePath, Name: "RegisterInput"},
			Match:  ObjectMatchChannel,
			Input:  ValueSlot{Kind: SlotParameter, Index: 1},
		},
	}
}

func closedWorldCallbackBoundary(packageName string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:     "closed-world.callback-effect",
		Kind:   KindProviderMutation,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Effect"},
		Match:  ObjectMatchExact,
	}
}

func closedWorldCallableProvenanceBoundaries(packageName string) []BoundaryDefinition {
	packagePath := fixtureModulePath + "/" + packageName
	return []BoundaryDefinition{
		{
			ID:     "closed-world.error-effect",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: packagePath, Name: "ErrorEffect"},
			Match:  ObjectMatchExact,
		},
		{
			ID:     "closed-world.context-effect",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: packagePath, Name: "ContextEffect"},
			Match:  ObjectMatchExact,
		},
		{
			ID:     "closed-world.string-effect",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: packagePath, Name: "StringEffect"},
			Match:  ObjectMatchExact,
		},
	}
}

func fixtureMethodCall(boundaryID, packageName, file, receiver, method string, closure []int, operation OperationKind, ordinal int) observedKey {
	key := fixtureCall(boundaryID, packageName, file, method, closure, operation, ordinal)
	key.Matcher.Enclosing.Object.Receiver = receiver
	return key
}
