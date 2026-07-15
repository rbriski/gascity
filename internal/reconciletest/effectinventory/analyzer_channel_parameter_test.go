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

func TestDiscoverClosedWorldProfileTracksChannelRegisteredThroughClosedGlobalFunction(t *testing.T) {
	const packageName = "closedworld/signalglobal"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := signalNotifyFixtureBoundary("channelparam.closed-global.signal-notify")

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(boundary.ID, packageName, "main.go", "run", nil, OperationSelectReceive, 1),
	})
}

func TestDiscoverClosedWorldProfileRejectsUnregisteredSameSignatureChannelRelease(t *testing.T) {
	const packageName = "closedworld/signalreleasewrong"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := signalNotifyFixtureBoundary("channelparam.same-signature-release.signal-notify")

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want unregistered-release escape diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, want := range []string{"run", "runDynamic", boundary.ID, "escape through an argument to an unanalyzed callee"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func TestDiscoverClosedWorldProfileReleasesOnlyTheDeclaredArgumentSlot(t *testing.T) {
	const packageName = "closedworld/signalreleaseindex"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := BoundaryDefinition{
		ID:     "channelparam.release-index.register",
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Register"},
		Match:  ObjectMatchChannel,
		Input:  ValueSlot{Kind: SlotParameter, Index: 1},
		Release: ChannelRelease{
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Stop"},
			Input:  ValueSlot{Kind: SlotParameter, Index: 1},
		},
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want non-release argument escape diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, want := range []string{"run", boundary.ID, "escape through an argument to an unanalyzed callee"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func TestDiscoverClosedWorldProfileRejectsChannelRegistrationThroughExportedGlobalFunction(t *testing.T) {
	const packageName = "closedworld/signalglobalopen"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	config.closedWorld = true
	boundary := signalNotifyFixtureBoundary("channelparam.open-global.signal-notify")

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want exported-global call-provenance diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, want := range []string{"run", "ambiguous call provenance"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func signalNotifyFixtureBoundary(id string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:     id,
		Kind:   KindWakeSource,
		Object: ObjectRef{Package: "os/signal", Name: "Notify"},
		Match:  ObjectMatchChannel,
		Input:  ValueSlot{Kind: SlotParameter, Index: 1},
		Release: ChannelRelease{
			Object: ObjectRef{Package: "os/signal", Name: "Stop"},
			Input:  ValueSlot{Kind: SlotParameter, Index: 1},
		},
	}
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

func TestDiscoverProfileRejectsInvalidChannelReleases(t *testing.T) {
	const packageName = "channelparam/register"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	base := BoundaryDefinition{
		ID:      "channelparam.register.signal-notify",
		Kind:    KindWakeSource,
		Object:  ObjectRef{Package: "os/signal", Name: "Notify"},
		Match:   ObjectMatchChannel,
		Input:   ValueSlot{Kind: SlotParameter, Index: 1},
		Release: ChannelRelease{Object: ObjectRef{Package: "os/signal", Name: "Stop"}, Input: ValueSlot{Kind: SlotParameter, Index: 1}},
	}
	tests := []struct {
		name   string
		mutate func(*BoundaryDefinition)
		want   string
	}{
		{
			name: "unloaded package",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Release.Object.Package = "example.com/not-loaded"
			},
			want: `channel release package "example.com/not-loaded" was not loaded`,
		},
		{
			name: "missing object",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Release.Object.Name = "Missing"
			},
			want: "does not exist",
		},
		{
			name: "non-function object",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Release.Object = ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "NotFunction"}
			},
			want: "is not a function or method",
		},
		{
			name: "out-of-range parameter",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Release.Input.Index = 2
			},
			want: "has no parameter slot 2",
		},
		{
			name: "non-channel parameter",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Release.Object.Name = "Notify"
				boundary.Release.Input.Index = 2
			},
			want: "parameter 2 does not have channel type",
		},
		{
			name: "incompatible channel",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Release.Object = ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "WrongChannel"}
			},
			want: "is incompatible with source channel type",
		},
		{
			name: "method target",
			mutate: func(boundary *BoundaryDefinition) {
				boundary.Release.Object = ObjectRef{Package: fixtureModulePath + "/" + packageName, Receiver: "Stopper", Name: "Stop"}
			},
			want: "channel release must name a package function",
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
