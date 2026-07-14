package effectinventory

import (
	"context"
	"testing"
)

func TestDiscoverProfileFindsOnlyExactChannelObjectSites(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/routes",
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}

	observed, err := discoverProfile(context.Background(), config, profile, []BoundaryDefinition{fixtureWakeBoundary()})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}

	want := []observedKey{
		fixtureCall("wake-hub.wake", "routes", "routes.go", "ChannelSendRoute", nil, OperationChannelSend, 1),
		fixtureCall("wake-hub.wake", "routes", "routes.go", "ChannelReceiveRoute", nil, OperationChannelReceive, 1),
		fixtureCall("wake-hub.wake", "routes", "routes.go", "ChannelSelectRoute", nil, OperationSelectSend, 1),
		fixtureCall("wake-hub.wake", "routes", "routes.go", "ChannelSelectRoute", nil, OperationSelectReceive, 1),
	}
	assertObservedSites(t, observed, want)

	for _, site := range observed {
		if site.Profile != BuildLinuxDefault {
			t.Errorf("observed site profile = %q, want %q: %+v", site.Profile, BuildLinuxDefault, site)
		}
	}
}

func fixtureWakeBoundary() BoundaryDefinition {
	return BoundaryDefinition{
		ID:   "wake-hub.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/boundary",
			Receiver: "WakeHub",
			Name:     "Wake",
		},
		Match: ObjectMatchChannel,
	}
}
