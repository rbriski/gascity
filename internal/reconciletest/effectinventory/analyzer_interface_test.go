package effectinventory

import (
	"context"
	"testing"
)

func TestDiscoverProfileMatchesCompleteInterfaceAtSelectedReceiver(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/complete",
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	boundaries := []BoundaryDefinition{{
		ID:   "complete-mutator.mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/boundary",
			Receiver: "CompleteMutator",
			Name:     "Mutate",
		},
		Match: ObjectMatchInterfaceImplementors,
	}}

	observed, err := discoverProfile(context.Background(), config, profile, boundaries)
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	completeInterfaceSites := observed[:0]
	for _, site := range observed {
		if site.BoundaryID == "complete-mutator.mutate" {
			completeInterfaceSites = append(completeInterfaceSites, site)
		}
	}

	assertObservedSites(t, completeInterfaceSites, []observedKey{
		fixtureCall("complete-mutator.mutate", "complete", "complete.go", "PromotedRoute", nil, OperationCall, 1),
		fixtureCall("complete-mutator.mutate", "complete", "complete.go", "PromotedMethodValueRoute", nil, OperationCall, 1),
	})
}
