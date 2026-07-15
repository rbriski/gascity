package effectinventory

import (
	"context"
	"testing"
)

func TestCanonicalSignalNotifyContextBoundaryIsDiscovered(t *testing.T) {
	const packageName = "closedworld/signalcontext"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true

	var boundary BoundaryDefinition
	for _, candidate := range CanonicalBoundaries() {
		if candidate.Object == (ObjectRef{Package: "os/signal", Name: "NotifyContext"}) {
			boundary = candidate
			break
		}
	}
	if boundary.ID == "" {
		t.Fatal("CanonicalBoundaries() is missing os/signal.NotifyContext")
	}
	if boundary.Kind != KindWakeSource || boundary.Match != ObjectMatchExact || !boundary.Input.zero() || !boundary.Output.zero() {
		t.Fatalf("signal.NotifyContext boundary = %+v, want exact wake source without channel slots", boundary)
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "main.go", "waitForTermination", nil, OperationCall, 1),
	})
}
