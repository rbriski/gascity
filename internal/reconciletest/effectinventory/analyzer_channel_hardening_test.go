package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileTracksExactChannelFieldAliasesAndRangeReceives(t *testing.T) {
	const packageName = "channels/field"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	boundary := BoundaryDefinition{
		ID:   "channels.field.wake",
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
		fixtureCall(boundary.ID, packageName, "field.go", "AliasSend", nil, OperationChannelSend, 1),
		fixtureCall(boundary.ID, packageName, "field.go", "RangeReceive", nil, OperationChannelReceive, 1),
	}
	assertObservedSites(t, observed, want)
}

func TestDiscoverProfileTracksExactFunctionResultChannel(t *testing.T) {
	const packageName = "channels/result"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	boundary := BoundaryDefinition{
		ID:   "channels.result.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package: fixtureModulePath + "/" + packageName,
			Name:    "Wake",
		},
		Match:  ObjectMatchChannel,
		Output: ValueSlot{Kind: SlotResult, Index: 1},
	}

	observed, err := discoverProfile(context.Background(), config, profile, []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}

	want := []observedKey{
		fixtureCall(boundary.ID, packageName, "result.go", "Receive", nil, OperationChannelReceive, 1),
	}
	assertObservedSites(t, observed, want)
}

func TestDiscoverProfileRejectsOpenWorldChannelProvenance(t *testing.T) {
	const packageName = "channels/external"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	boundary := BoundaryDefinition{
		ID:   "channels.external.approved",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package: fixtureModulePath + "/" + packageName,
			Name:    "Approved",
		},
		Match:  ObjectMatchChannel,
		Output: ValueSlot{Kind: SlotResult, Index: 1},
	}

	_, err := discoverProfile(context.Background(), config, profile, []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() returned nil, want open-world channel provenance error")
	}
	for _, function := range []string{"InjectedCallback", "InjectedChannel"} {
		if !strings.Contains(err.Error(), function) {
			t.Errorf("discoverProfile() error = %q, want diagnostic for %s", err, function)
		}
	}
}

func TestDiscoverProfileRejectsCloseOfExactChannelAsUnsupported(t *testing.T) {
	const packageName = "channels/closeop"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	boundary := BoundaryDefinition{
		ID:   "channels.close.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: "Hub",
			Name:     "Wake",
		},
		Match: ObjectMatchChannel,
	}

	_, err := discoverProfile(context.Background(), config, profile, []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() returned nil, want unsupported close diagnostic")
	}
	for _, want := range []string{"CloseWake", "close", "unsupported"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}
