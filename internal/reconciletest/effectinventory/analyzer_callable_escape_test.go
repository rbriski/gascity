package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileRejectsBoundMethodFromOpenWorldNarrowerInterface(t *testing.T) {
	const packageName = "callescape/narrower"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "callescape.complete-mutator.mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/boundary",
			Receiver: "CompleteMutator",
			Name:     "Mutate",
		},
		Match: ObjectMatchInterfaceImplementors,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertCallableEscapeRejected(t, observed, err, "BoundOpenWorldNarrowerMethod")
	for _, owner := range []string{"BoundOpenWorldNarrowerPhi", "BoundOpenWorldNarrowerField", "BoundOpenWorldNarrowerClosure"} {
		if !strings.Contains(err.Error(), owner) {
			t.Errorf("discoverProfile() error = %q, want %q", err, owner)
		}
	}
}

func TestDiscoverProfileRejectsInjectedCompatibleMethodExpression(t *testing.T) {
	const packageName = "callescape/methodexpr"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "callescape.method-expression.mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: "Impl",
			Name:     "Mutate",
		},
		Match: ObjectMatchExact,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertCallableEscapeRejected(t, observed, err, "InvokeInjectedMethodExpression")
	if !strings.Contains(err.Error(), "InvokeInjectedPointerAdaptedMethodExpression") {
		t.Errorf("discoverProfile() error = %q, want pointer-adapted method expression owner", err)
	}
}

func TestDiscoverProfileRejectsInjectedNarrowInterfaceMethodExpression(t *testing.T) {
	const packageName = "callescape/interfaceexpr"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "callescape.interface-expression.mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/boundary",
			Receiver: "CompleteMutator",
			Name:     "Mutate",
		},
		Match: ObjectMatchInterfaceImplementors,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertCallableEscapeRejected(t, observed, err, "InvokeNarrowMethodExpression")
}

func TestDiscoverProfileRejectsInjectedPromotedMethodExpression(t *testing.T) {
	const packageName = "callescape/promotedexpr"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "callescape.promoted-expression.mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: "Inner",
			Name:     "Mutate",
		},
		Match: ObjectMatchExact,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertCallableEscapeRejected(t, observed, err, "InvokePromotedMethodExpression")
}

func TestDiscoverProfileRejectsBoundaryValuesEscapedToUnanalyzedCallees(t *testing.T) {
	const packageName = "callescape/externalarg"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundaries := []BoundaryDefinition{
		{
			ID:     "callescape.external.callback",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Effect"},
			Match:  ObjectMatchExact,
		},
		{
			ID:     "callescape.external.channel",
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Approved"},
			Match:  ObjectMatchChannel,
		},
		{
			ID:   "callescape.external.bound-method",
			Kind: KindProviderMutation,
			Object: ObjectRef{
				Package:  fixtureModulePath + "/" + packageName,
				Receiver: "Walker",
				Name:     "Visit",
			},
			Match: ObjectMatchExact,
		},
		{
			ID:   "callescape.external.method-expression",
			Kind: KindProviderMutation,
			Object: ObjectRef{
				Package:  fixtureModulePath + "/" + packageName,
				Receiver: "Entry",
				Name:     "Compare",
			},
			Match: ObjectMatchExact,
		},
		{
			ID:     "callescape.external.bound-channel-receiver",
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "ReceiverApproved"},
			Match:  ObjectMatchChannel,
		},
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), boundaries)
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want escaped-boundary diagnostics")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, fragment := range []string{
		"Route", "RouteConverted", "RoutePhi", "RouteField", "RouteInjected", "RouteAlloc", "RouteFreeVar",
		"RouteIndex", "RouteLookup", "RouteOpenResult",
		"RouteBound", "SortEntries", "Notify", "NotifyInjected", "NotifyPhi",
		"NotifyField", "PointerChannel", "LocalPointerChannel", "BoundChannelReceiver",
		"OpenInterfaceDrop", "OpenFunctionDrop", "BodylessDrop",
		"OpenBoundSink", "OpenBoundBoxedSink", "OpenBoundOnlyBoxedSink",
		"OpenBoundOnlyFreeVarSink", "OpenBoundOnlyFieldSink", "OpenThunkSink", "MixedInterfaceDrop", "BoxedCallback",
		"BoxedChannel", "BoxedChannelResult", "VariadicCallback", "VariadicChannel", "AsyncValues", "unanalyzed",
		"EllipsisBoxedCallback", "EllipsisBoxedChannel", "EllipsisCallback",
		"EllipsisChannel", "EllipsisOpenCallback", "EllipsisOpenChannel",
		"BoxedSliceCallback", "BoxedSliceChannel",
	} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("discoverProfile() error = %q, want %q", err, fragment)
		}
	}
	for _, fragment := range []string{
		"RouteUnrelatedBound", "SortOtherEntries", "AuthoredDrops",
		"ClosedDynamicDrop", "ClosedInterfaceDrop", "AuthoredGenericDrop",
		"AuthoredBoundDrop", "AuthoredBoundWithInterfaceArg", "ClosedExternalValues",
		"NilVariadicValues", "EllipsisOpenBoxed", "BoxedOpenSlice",
	} {
		if strings.Contains(err.Error(), fragment) {
			t.Errorf("discoverProfile() error = %q, do not want closed negative control %q", err, fragment)
		}
	}

	reversed := append([]BoundaryDefinition(nil), boundaries...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	secondObserved, secondErr := discoverProfile(context.Background(), config, fixtureLinuxProfile(), reversed)
	if secondErr == nil || secondErr.Error() != err.Error() {
		t.Fatalf("discoverProfile() reversed-boundary error = %v, want byte-identical %v", secondErr, err)
	}
	if secondObserved != nil {
		t.Fatalf("discoverProfile() reversed-boundary observed = %#v, want nil", secondObserved)
	}
}

func diagnosticLine(message, owner string) string {
	for _, line := range strings.Split(message, "\n") {
		if strings.Contains(line, owner) {
			return line
		}
	}
	return ""
}

func TestDiscoverProfileRejectsAddressEscapedLocalFunctionSlot(t *testing.T) {
	const packageName = "callescape/localslot"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "callescape.local-slot.emit",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package: fixtureModulePath + "/boundary",
			Name:    "Emit",
		},
		Match: ObjectMatchExact,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	assertCallableEscapeRejected(t, observed, err, "AddressEscapedLocalFunctionSlot")
}

func TestDiscoverProfileAcceptsClosedCallableValues(t *testing.T) {
	const packageName = "callescape/closed"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	methodBoundary := BoundaryDefinition{
		ID:   "callescape.closed.mutate",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: "Impl",
			Name:     "Mutate",
		},
		Match: ObjectMatchExact,
	}
	emitBoundary := BoundaryDefinition{
		ID:   "callescape.closed.emit",
		Kind: KindProviderMutation,
		Object: ObjectRef{
			Package: fixtureModulePath + "/boundary",
			Name:    "Emit",
		},
		Match: ObjectMatchExact,
	}

	observed, err := discoverProfile(
		context.Background(),
		config,
		fixtureLinuxProfile(),
		[]BoundaryDefinition{methodBoundary, emitBoundary},
	)
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall(methodBoundary.ID, packageName, "closed.go", "BoundConcreteMethod", nil, OperationCall, 1),
		fixtureCall(emitBoundary.ID, packageName, "closed.go", "LocalFunctionSlot", nil, OperationCall, 1),
	})
}

func assertCallableEscapeRejected(t *testing.T, observed []ObservedSite, err error, owner string) {
	t.Helper()
	if err == nil {
		t.Fatalf("discoverProfile() error = nil, want fail-closed callable-escape diagnostic for %s", owner)
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil after callable escape", observed)
	}
	if !strings.Contains(err.Error(), owner) {
		t.Fatalf("discoverProfile() error = %q, want owning function %q", err, owner)
	}
}
