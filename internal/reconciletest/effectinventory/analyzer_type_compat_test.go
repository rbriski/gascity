package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileRejectsOpenWorldGenericChannelOperations(t *testing.T) {
	const packageName = "typecompat/generic"
	config := typeCompatibilityFixtureConfig(t, packageName)
	boundary := typeCompatibilityChannelBoundary(packageName, "Approved")

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatalf("discoverProfile() = %#v, nil; want generic open-world channel diagnostics", observed)
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil when generic channel provenance is open-world", observed)
	}
	for _, function := range []string{"Send", "Receive", "Close"} {
		if !strings.Contains(err.Error(), function) {
			t.Errorf("discoverProfile() error = %q, want diagnostic for %s", err, function)
		}
	}
	if !strings.Contains(err.Error(), "open-world") && !strings.Contains(err.Error(), "unresolved") {
		t.Errorf("discoverProfile() error = %q, want open-world or unresolved provenance diagnostic", err)
	}
}

func TestDiscoverProfileRejectsOpenWorldConvertibleNamedChannels(t *testing.T) {
	const packageName = "typecompat/convertible"
	config := typeCompatibilityFixtureConfig(t, packageName)
	boundary := typeCompatibilityChannelBoundary(packageName, "Approved")

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatalf("discoverProfile() = %#v, nil; want convertible named-channel diagnostics", observed)
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil when convertible channel provenance is open-world", observed)
	}
	for _, function := range []string{"GenericConvertibleSend", "NamedConvertibleSend"} {
		if !strings.Contains(err.Error(), function) {
			t.Errorf("discoverProfile() error = %q, want diagnostic for %s", err, function)
		}
	}
}

func TestDiscoverProfileRejectsAliasReceiverAndNamesDeclaringReceiver(t *testing.T) {
	const packageName = "typecompat/receiveralias"
	config := typeCompatibilityFixtureConfig(t, packageName)
	boundary := typeCompatibilityReceiverBoundary(packageName, "Alias")

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatalf("discoverProfile() = %#v, nil; want receiver-alias boundary error", observed)
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil for a receiver-alias boundary", observed)
	}
	for _, fragment := range []string{"Alias", "alias", "Declaring", "declaring receiver"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("discoverProfile() error = %q, want fragment %q", err, fragment)
		}
	}
}

func TestDiscoverProfileAcceptsCanonicalDeclaringReceiver(t *testing.T) {
	const packageName = "typecompat/receiveralias"
	config := typeCompatibilityFixtureConfig(t, packageName)
	boundary := typeCompatibilityReceiverBoundary(packageName, "Declaring")

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "receiveralias.go", "CanonicalRoute", nil, OperationChannelSend, 1),
	})
}

func typeCompatibilityFixtureConfig(t *testing.T, packageName string) analysisConfig {
	t.Helper()
	return fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
}

func typeCompatibilityChannelBoundary(packageName, objectName string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:     "typecompat." + packageName + ".channel",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: objectName},
		Match:  ObjectMatchChannel,
	}
}

func typeCompatibilityReceiverBoundary(packageName, receiver string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:   "typecompat.receiveralias.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: receiver,
			Name:     "Wake",
		},
		Match:  ObjectMatchChannel,
		Output: ValueSlot{Kind: SlotResult, Index: 1},
	}
}
