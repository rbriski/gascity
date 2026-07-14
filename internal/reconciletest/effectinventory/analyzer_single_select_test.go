package effectinventory

import (
	"context"
	"testing"
)

func TestDiscoverProfilePreservesBlockingSingleCaseSelectOperations(t *testing.T) {
	const packageName = "selectsingle"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	boundary := BoundaryDefinition{
		ID:   "selectsingle.hub.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: "Hub",
			Name:     "Wake",
		},
		Match: ObjectMatchChannel,
	}

	observed, err := discoverProfile(context.Background(), config, profile, []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}

	want := []observedKey{
		fixtureCall(boundary.ID, packageName, "selectsingle.go", "BlockingSelectSend", nil, OperationSelectSend, 1),
		fixtureCall(boundary.ID, packageName, "selectsingle.go", "BlockingSelectReceive", nil, OperationSelectReceive, 1),
	}
	assertObservedSites(t, observed, want)

	for _, site := range observed {
		if site.Profile != BuildLinuxDefault {
			t.Errorf("observed site profile = %q, want %q: %+v", site.Profile, BuildLinuxDefault, site)
		}
		if site.Matcher.Operation == OperationChannelSend || site.Matcher.Operation == OperationChannelReceive {
			t.Errorf("blocking select was classified as direct channel operation: %+v", site)
		}
	}
}
