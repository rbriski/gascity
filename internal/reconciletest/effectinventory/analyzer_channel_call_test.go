package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverClosedWorldProfileTracesClosedDynamicChannelResult(t *testing.T) {
	const packageName = "closedworld/channeldynamic"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := timeAfterFixtureBoundary()

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "main.go", "closedDynamicWait", nil, OperationSelectReceive, 1),
	})
}

func TestDiscoverClosedWorldProfileRejectsOpenDynamicChannelResult(t *testing.T) {
	const packageName = "closedworld/channeldynamicopen"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := timeAfterFixtureBoundary()

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want open dynamic channel-result diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, want := range []string{"openDynamicWait", "open-world provenance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func TestDiscoverClosedWorldProfileRejectsMixedClosedDynamicChannelTargets(t *testing.T) {
	const packageName = "closedworld/channeldynamicmixed"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := timeAfterFixtureBoundary()

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want mixed dynamic channel-target diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, want := range []string{"mixedDynamicWait", "open-world provenance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func timeAfterFixtureBoundary() BoundaryDefinition {
	return BoundaryDefinition{
		ID:     "closed-world.time-after",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: "time", Name: "After"},
		Match:  ObjectMatchChannel,
		Output: ValueSlot{Kind: SlotResult, Index: 1},
	}
}
