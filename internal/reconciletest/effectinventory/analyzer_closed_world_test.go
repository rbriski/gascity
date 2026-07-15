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
