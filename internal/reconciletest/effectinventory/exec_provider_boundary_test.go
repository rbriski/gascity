package effectinventory

import (
	"context"
	"testing"
)

const execProviderFixturePackage = fixtureModulePath + "/execprovider"

func TestCanonicalBoundariesKeepOuterExecSeamsButNotLegacyExecProvider(t *testing.T) {
	byObject := make(map[string]BoundaryDefinition)
	for _, boundary := range CanonicalBoundaries() {
		byObject[boundary.Object.key()] = boundary
	}

	for _, want := range []ObjectRef{
		{Package: runtimePackage, Receiver: "Provider", Name: "Start"},
		{Package: runtimePackage, Receiver: "Place", Name: "Exec"},
		{Package: runtimePackage, Receiver: "Carrier", Name: "Nudge"},
		{Package: runtimePackage, Receiver: "Attachment", Name: "Nudge"},
	} {
		boundary, ok := byObject[want.key()]
		if !ok {
			t.Errorf("outer mutation seam %s is missing", want.key())
			continue
		}
		if boundary.Kind != KindProviderMutation || boundary.Match != ObjectMatchInterfaceImplementors {
			t.Errorf("outer mutation seam %s = kind %q match %q, want provider mutation/interface implementors", want.key(), boundary.Kind, boundary.Match)
		}
	}

	legacy := ObjectRef{Package: runtimePackage, Receiver: "ExecProvider", Name: "Exec"}
	if boundary, ok := byObject[legacy.key()]; ok {
		t.Fatalf("read/write-conflated legacy seam %s remains registered as %q", legacy.key(), boundary.ID)
	}
}

func TestOuterExecSeamDiscoveryExcludesReadOnlyProcessAlive(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/execprovider",
	})
	boundary := BoundaryDefinition{
		ID:   "place.exec",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  execProviderFixturePackage,
			Receiver: "Place",
			Name:     "Exec",
		},
		Match: ObjectMatchInterfaceImplementors,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	want := []observedKey{
		fixtureCall("place.exec", "execprovider", "execprovider.go", "MutateThroughPlace", nil, OperationCall, 1),
	}
	assertObservedSites(t, observed, want)
	for _, site := range observed {
		if site.Matcher.Enclosing.Object.Name == "ProcessAlive" {
			t.Fatalf("read-only ProcessAlive legacy Exec call was classified as a mutation: %+v", site)
		}
	}
}
