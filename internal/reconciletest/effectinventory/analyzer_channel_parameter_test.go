package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileBindsChannelParameterToExactFieldCaller(t *testing.T) {
	const packageName = "channelparam/field"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "channelparam.field.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: "Hub",
			Name:     "Wake",
		},
		Match: ObjectMatchChannel,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "field.go", "selectWake", nil, OperationSelectReceive, 1),
	})
}

func TestDiscoverProfileRejectsChannelParameterWithoutAuthoredCallerDeterministically(t *testing.T) {
	const packageName = "channelparam/external"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "channelparam.external.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package: fixtureModulePath + "/" + packageName,
			Name:    "Approved",
		},
		Match: ObjectMatchChannel,
	}
	locator := fixtureCall("", packageName, "external.go", "SelectExternal", nil, OperationSelectReceive, 1).Matcher.Enclosing.key()
	want := "effect discovery failed for profile \"linux/default\":\n- " + locator +
		": unresolved channel operation has open-world provenance compatible with an inventoried boundary"

	for repetition := 0; repetition < 3; repetition++ {
		observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
		if err == nil {
			t.Fatalf("discoverProfile(repetition=%d) error = nil, want unresolved-caller diagnostic", repetition)
		}
		if observed != nil {
			t.Fatalf("discoverProfile(repetition=%d) observed = %#v, want nil", repetition, observed)
		}
		if err.Error() != want {
			t.Fatalf("discoverProfile(repetition=%d) diagnostic mismatch\n got:\n%s\nwant:\n%s", repetition, err, want)
		}
	}
}

func TestDiscoverProfileRejectsCallerCycleWithoutConcreteChannelDeterministically(t *testing.T) {
	const packageName = "channelparam/cycle"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:     "channelparam.cycle.wake",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Approved"},
		Match:  ObjectMatchChannel,
	}
	locator := fixtureCall("", packageName, "cycle.go", "selectRecursive", nil, OperationSelectReceive, 1).Matcher.Enclosing.key()
	want := "effect discovery failed for profile \"linux/default\":\n- " + locator +
		": unresolved channel operation has open-world provenance compatible with an inventoried boundary"

	for repetition := 0; repetition < 3; repetition++ {
		observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
		if err == nil {
			t.Fatalf("discoverProfile(repetition=%d) error = nil, want caller-cycle diagnostic", repetition)
		}
		if observed != nil {
			t.Fatalf("discoverProfile(repetition=%d) observed = %#v, want nil", repetition, observed)
		}
		if err.Error() != want {
			t.Fatalf("discoverProfile(repetition=%d) diagnostic mismatch\n got:\n%s\nwant:\n%s", repetition, err, want)
		}
	}
}

func TestDiscoverProfileTracksLocalChannelRegisteredThroughExactParameter(t *testing.T) {
	const packageName = "channelparam/register"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "channelparam.register.signal-notify",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package: "os/signal",
			Name:    "Notify",
		},
		Match: ObjectMatchChannel,
		Input: ValueSlot{Kind: SlotParameter, Index: 1},
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "register.go", "RegisteredLocal", nil, OperationSelectReceive, 1),
	})
}

func TestDiscoverProfileRejectsAmbiguousChannelRegistrationCallDeterministically(t *testing.T) {
	const packageName = "channelparam/ambiguous"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:     "channelparam.ambiguous.signal-notify",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: "os/signal", Name: "Notify"},
		Match:  ObjectMatchChannel,
		Input:  ValueSlot{Kind: SlotParameter, Index: 1},
	}
	locator := fixtureCall("", packageName, "ambiguous.go", "SelectAfterAmbiguousRegistration", nil, OperationCall, 1).Matcher.Enclosing.key()
	want := "effect discovery failed for profile \"linux/default\":\n- " + locator +
		": channel input boundary " + boundary.ID + " has ambiguous call provenance"

	for repetition := 0; repetition < 3; repetition++ {
		observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
		if err == nil {
			t.Fatalf("discoverProfile(repetition=%d) error = nil, want ambiguous-call diagnostic", repetition)
		}
		if observed != nil {
			t.Fatalf("discoverProfile(repetition=%d) observed = %#v, want nil", repetition, observed)
		}
		if err.Error() != want {
			t.Fatalf("discoverProfile(repetition=%d) diagnostic mismatch\n got:\n%s\nwant:\n%s", repetition, err, want)
		}
	}
}

func TestDiscoverProfileRejectsInvalidChannelInputSlots(t *testing.T) {
	const packageName = "channelparam/register"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	base := BoundaryDefinition{
		ID:     "channelparam.register.signal-notify",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: "os/signal", Name: "Notify"},
		Match:  ObjectMatchChannel,
	}
	tests := []struct {
		name   string
		mutate func(*BoundaryDefinition)
		want   string
	}{
		{
			name: "non-channel parameter",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Input = ValueSlot{Kind: SlotParameter, Index: 2}
			},
			want: "parameter 2 does not have channel type",
		},
		{
			name: "input and output",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Input = ValueSlot{Kind: SlotParameter, Index: 1}
				boundary.Output = ValueSlot{Kind: SlotResult, Index: 1}
			},
			want: "cannot name both channel input and output slots",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			boundary := base
			test.mutate(&boundary)
			_, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("discoverProfile() error = %v, want diagnostic containing %q", err, test.want)
			}
		})
	}
}
