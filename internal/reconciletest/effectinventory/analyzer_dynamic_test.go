package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileFindsClosedLocalFunctionValueOperations(t *testing.T) {
	const packageName = "dynamicops"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	boundary := BoundaryDefinition{
		ID:   "dynamic.emit",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package: fixtureModulePath + "/boundary",
			Name:    "Emit",
		},
		Match: ObjectMatchExact,
	}

	observed, err := discoverProfile(context.Background(), config, profile, []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}

	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "dynamicops.go", "ClosedFunctionValueOperations", nil, OperationCall, 1),
		fixtureCall(boundary.ID, packageName, "dynamicops.go", "ClosedFunctionValueOperations", nil, OperationGo, 1),
		fixtureCall(boundary.ID, packageName, "dynamicops.go", "ClosedFunctionValueOperations", nil, OperationDefer, 1),
	})
}

func TestDiscoverProfileRejectsAmbiguousTypedCallDeterministically(t *testing.T) {
	const packageName = "dynamicops"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	interfaceBoundary := BoundaryDefinition{
		ID:   "dynamic.interface-mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/boundary",
			Receiver: "Mutator",
			Name:     "Mutate",
		},
		Match: ObjectMatchInterfaceImplementors,
	}
	concreteBoundary := BoundaryDefinition{
		ID:   "dynamic.value-mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/boundary",
			Receiver: "ValueMutator",
			Name:     "Mutate",
		},
		Match: ObjectMatchExact,
	}
	wantAmbiguity := "call matches multiple boundaries: dynamic.interface-mutate, dynamic.value-mutate"

	var baseline string
	boundaryOrders := [][]BoundaryDefinition{
		{interfaceBoundary, concreteBoundary},
		{concreteBoundary, interfaceBoundary},
	}
	for orderIndex, boundaries := range boundaryOrders {
		for repetition := 0; repetition < 3; repetition++ {
			_, err := discoverProfile(context.Background(), config, profile, boundaries)
			if err == nil {
				t.Fatalf("discoverProfile(order=%d, repetition=%d) error = nil, want ambiguous-boundary diagnostic", orderIndex, repetition)
			}
			if !strings.Contains(err.Error(), "AmbiguousConcreteMethodCall") {
				t.Errorf("discoverProfile(order=%d, repetition=%d) error = %q, want physical caller name", orderIndex, repetition, err)
			}
			if !strings.Contains(err.Error(), wantAmbiguity) {
				t.Errorf("discoverProfile(order=%d, repetition=%d) error = %q, want %q", orderIndex, repetition, err, wantAmbiguity)
			}
			if baseline == "" {
				baseline = err.Error()
				continue
			}
			if err.Error() != baseline {
				t.Fatalf("discoverProfile(order=%d, repetition=%d) diagnostics changed\n got:\n%s\nwant:\n%s", orderIndex, repetition, err, baseline)
			}
		}
	}
}
