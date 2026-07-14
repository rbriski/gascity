package effectinventory

import (
	"context"
	"reflect"
	"sort"
	"strings"
	"sync"
	"testing"
)

const targetGateFixturePackage = fixtureModulePath + "/targetgate"

func TestValidateTargetGateEvidenceForProfileAcceptsExactTypedClaims(t *testing.T) {
	analysis := loadTargetGateFixture(t)
	registries := []Registry{
		targetGateRegistry(),
		targetGateChannelRegistry("Wake", ObjectRef{Package: targetGateFixturePackage, Receiver: "Payload", Name: "ID"}),
		targetGateConcreteRegistry(),
		targetGateGenericProjectionRegistry(),
	}
	channelObject := targetGateChannelRegistry("Wake", ObjectRef{})
	firstTargetGateRoute(&channelObject).Target.Identities[0].BoundarySlot = ValueSlot{Kind: SlotBoundaryObject}
	registries = append(registries, channelObject)
	for _, registry := range registries {
		if err := validateTargetGateEvidenceForProfile(analysis, registry); err != nil {
			t.Fatalf("validateTargetGateEvidenceForProfile() error: %v", err)
		}
	}
}

func TestValidateTargetGateEvidenceForProfileProvesBoundarySlots(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Registry)
		want   []string
	}{
		{
			name: "missing receiver",
			mutate: func(registry *Registry) {
				registry.Boundaries[0].Object = targetGateObject("", "ApplyFree")
				registry.Boundaries[0].Match = ObjectMatchExact
			},
			want: []string{"boundary receiver", "has no receiver slot"},
		},
		{
			name: "stale parameter",
			mutate: func(registry *Registry) {
				firstTargetGateRoute(registry).Target.Identities[0].BoundarySlot.Index = 3
			},
			want: []string{"boundary parameter slot 3", "has only 1 parameter"},
		},
		{
			name: "stale result",
			mutate: func(registry *Registry) {
				firstTargetGateRoute(registry).Target.Identities[1].BoundarySlot.Index = 3
			},
			want: []string{"boundary result slot 3", "has only 2 results"},
		},
		{
			name: "non-channel element",
			mutate: func(registry *Registry) {
				firstTargetGateRoute(registry).Target.Identities[0].BoundarySlot = ValueSlot{Kind: SlotChannelElement}
			},
			want: []string{"boundary channel-element", "is not a channel boundary"},
		},
		{
			name: "unknown slot kind",
			mutate: func(registry *Registry) {
				firstTargetGateRoute(registry).Target.Identities[0].BoundarySlot = ValueSlot{Kind: ValueSlotKind("mystery")}
			},
			want: []string{`boundary slot kind "mystery" is not typed evidence`},
		},
		{
			name: "indexed receiver",
			mutate: func(registry *Registry) {
				firstTargetGateRoute(registry).Target.Identities[2].BoundarySlot.Index = 1
			},
			want: []string{"boundary receiver slot cannot have an index"},
		},
	}

	analysis := loadTargetGateFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := targetGateRegistry()
			tt.mutate(&registry)
			assertTargetGateEvidenceError(t, analysis, registry, tt.want...)
		})
	}

	wrongChannel := targetGateChannelRegistry("StringWake", ObjectRef{Package: targetGateFixturePackage, Receiver: "Payload", Name: "ID"})
	assertTargetGateEvidenceError(t, analysis, wrongChannel, "projection", "does not belong to selected slot type", "string")
}

func TestValidateTargetGateEvidenceForProfileProvesProjectionOwnership(t *testing.T) {
	tests := []struct {
		name       string
		projection ObjectRef
		want       []string
	}{
		{
			name:       "missing field",
			projection: targetGateObject("Item", "Missing"),
			want:       []string{"projection", "not an unambiguous field or method"},
		},
		{
			name:       "same named field on wrong type",
			projection: targetGateObject("Other", "ID"),
			want:       []string{"projection", "does not belong to selected slot type", "Item"},
		},
		{
			name:       "method is not a field projection",
			projection: targetGateObject("Item", "Label"),
			want:       []string{"projection", "is not a field"},
		},
		{
			name:       "ambiguous selector",
			projection: targetGateObject("Ambiguous", "ID"),
			want:       []string{"projection", "not an unambiguous field or method"},
		},
	}

	analysis := loadTargetGateFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := targetGateRegistry()
			firstTargetGateRoute(&registry).Target.Identities[0].Projection = tt.projection
			assertTargetGateEvidenceError(t, analysis, registry, tt.want...)
		})
	}

	promoted := targetGateConcreteRegistry()
	promoted.Boundaries[0].Object = targetGateObject("Sink", "ApplyEmbedded")
	promoted.Registrations[0].Matcher.Enclosing = targetGateFunction("EmbeddedMiddle")
	route := firstTargetGateRoute(&promoted)
	route.LogicalOwner = targetGateFunction("EmbeddedMiddle")
	route.Hops = nil
	route.Target.Identities[0].Projection = targetGateObject("Item", "ID")
	if err := validateTargetGateEvidenceForProfile(analysis, promoted); err != nil {
		t.Fatalf("unambiguous promoted projection rejected: %v", err)
	}
}

func TestValidateTargetGateEvidenceForProfileProvesSourceObjectsAndSlots(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*TargetIdentityRef)
		want   []string
	}{
		{
			name: "missing source object",
			mutate: func(identity *TargetIdentityRef) {
				identity.SourceObject.Name = "MissingLookup"
			},
			want: []string{"source object", "does not exist"},
		},
		{
			name: "source is not a function",
			mutate: func(identity *TargetIdentityRef) {
				identity.SourceObject = targetGateObject("", "ConfigID")
			},
			want: []string{"source object", "is not a function or method"},
		},
		{
			name: "stale source result",
			mutate: func(identity *TargetIdentityRef) {
				identity.SourceObject = targetGateObject("", "NoResults")
			},
			want: []string{"source result slot 1", "has no results"},
		},
		{
			name: "wrong source slot kind",
			mutate: func(identity *TargetIdentityRef) {
				identity.SourceSlot = ValueSlot{Kind: SlotParameter, Index: 1}
			},
			want: []string{"source slot must be result"},
		},
		{
			name: "object-field source is a function",
			mutate: func(identity *TargetIdentityRef) {
				identity.Source = TargetSourceObjectField
				identity.SourceObject = targetGateObject("", "GoodPredicate")
				identity.SourceSlot = ValueSlot{}
			},
			want: []string{"source object", "is not a field"},
		},
		{
			name: "config source is a function",
			mutate: func(identity *TargetIdentityRef) {
				identity.Source = TargetSourceConfigValue
				identity.SourceObject = targetGateObject("", "GoodPredicate")
				identity.SourceSlot = ValueSlot{}
			},
			want: []string{"source object", "is not a variable, field, or constant"},
		},
		{
			name: "constant source is a variable",
			mutate: func(identity *TargetIdentityRef) {
				identity.Source = TargetSourceConstant
				identity.SourceObject = targetGateObject("", "ConfigID")
				identity.SourceSlot = ValueSlot{}
			},
			want: []string{"source object", "is not a constant"},
		},
		{
			name: "channel source is not a channel",
			mutate: func(identity *TargetIdentityRef) {
				identity.Source = TargetSourceChannelPayload
				identity.SourceObject = targetGateObject("", "ConfigID")
				identity.SourceSlot = ValueSlot{Kind: SlotChannelElement}
			},
			want: []string{"source object", "does not have channel type"},
		},
	}

	analysis := loadTargetGateFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := targetGateRegistry()
			identity := &firstTargetGateRoute(&registry).Target.Identities[0]
			tt.mutate(identity)
			assertTargetGateEvidenceError(t, analysis, registry, tt.want...)
		})
	}

	accepted := []TargetIdentityRef{
		{
			BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
			Source:       TargetSourceObjectField,
			SourceObject: targetGateObject("Item", "ID"),
		},
		{
			BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
			Source:       TargetSourceConfigValue,
			SourceObject: targetGateObject("", "ConfigID"),
		},
		{
			BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
			Source:       TargetSourceConstant,
			SourceObject: targetGateObject("", "ConstantID"),
		},
	}
	for _, identity := range accepted {
		registry := targetGateRegistry()
		firstTargetGateRoute(&registry).Target.Identities[0] = identity
		if err := validateTargetGateEvidenceForProfile(analysis, registry); err != nil {
			t.Errorf("valid source %q rejected: %v", identity.Source, err)
		}
	}

	channelSource := targetGateRegistry()
	identity := &firstTargetGateRoute(&channelSource).Target.Identities[0]
	identity.Source = TargetSourceChannelPayload
	identity.SourceObject = targetGateObject("", "Wake")
	identity.SourceSlot = ValueSlot{Kind: SlotChannelElement}
	if err := validateTargetGateEvidenceForProfile(analysis, channelSource); err != nil {
		t.Fatalf("valid channel source rejected: %v", err)
	}
}

func TestValidateTargetGateEvidenceForProfileProvesGateParametersAndCapabilities(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*GateCondition)
		want   []string
	}{
		{
			name: "missing parameter function",
			mutate: func(condition *GateCondition) {
				condition.Parameter.Function.Object.Name = "MissingMiddle"
			},
			want: []string{"gate parameter function", "missing from loaded profile"},
		},
		{
			name: "stale parameter function file",
			mutate: func(condition *GateCondition) {
				condition.Parameter.Function.File = "internal/reconciletest/effectinventory/testdata/analyzerfixture/targetgate/stale.go"
			},
			want: []string{"gate parameter function", "file/profile mismatch"},
		},
		{
			name: "stale parameter index",
			mutate: func(condition *GateCondition) {
				condition.Parameter.Slot.Index = 3
			},
			want: []string{"gate parameter slot 3", "has only 2 parameters"},
		},
		{
			name: "wrong route chain",
			mutate: func(condition *GateCondition) {
				condition.Parameter.Function = targetGateFunction("Unrelated")
			},
			want: []string{"gate parameter function", "is outside the connected route chain"},
		},
		{
			name: "missing capability",
			mutate: func(condition *GateCondition) {
				condition.Capability.Name = "MissingCapability"
			},
			want: []string{"gate capability", "does not exist"},
		},
		{
			name: "concrete capability",
			mutate: func(condition *GateCondition) {
				condition.Capability.Name = "ConcreteCapability"
			},
			want: []string{"gate capability", "is not an interface type"},
		},
		{
			name: "capability alias",
			mutate: func(condition *GateCondition) {
				condition.Capability.Name = "OptionalMutatorAlias"
			},
			want: []string{"gate capability", "is an alias", "exact declared interface"},
		},
		{
			name: "incompatible capability interface",
			mutate: func(condition *GateCondition) {
				condition.Capability.Name = "ConflictingCapability"
			},
			want: []string{"gate capability", "cannot be asserted from parameter type", "Mutator"},
		},
		{
			name: "uninstantiated generic capability",
			mutate: func(condition *GateCondition) {
				condition.Capability.Name = "GenericCapability"
			},
			want: []string{"gate capability", "is an uninstantiated generic type"},
		},
	}

	analysis := loadTargetGateFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := targetGateRegistry()
			condition := &firstTargetGateRoute(&registry).CurrentGate.Conditions[2]
			tt.mutate(condition)
			assertTargetGateEvidenceError(t, analysis, registry, tt.want...)
		})
	}

	concrete := targetGateRegistry()
	condition := &firstTargetGateRoute(&concrete).CurrentGate.Conditions[2]
	condition.Parameter.Function = targetGateFunction("ConcreteMiddle")
	condition.Parameter.Slot = ValueSlot{Kind: SlotParameter, Index: 1}
	firstTargetGateRoute(&concrete).Hops = append(firstTargetGateRoute(&concrete).Hops, RouteHop{
		Site:     OperationSite{Operation: OperationCall, Enclosing: targetGateFunction("Middle"), Ordinal: 1},
		Dispatch: HopDispatchExact,
		Callee:   targetGateFunction("ConcreteMiddle"),
	})
	concrete.Registrations[0].Matcher.Enclosing = targetGateFunction("ConcreteMiddle")
	assertTargetGateEvidenceError(t, analysis, concrete, "gate capability parameter", "must have interface type", "Sink")

	logicalOwner := targetGateRegistry()
	for index := range firstTargetGateRoute(&logicalOwner).CurrentGate.Conditions {
		condition := &firstTargetGateRoute(&logicalOwner).CurrentGate.Conditions[index]
		if condition.Kind == GateConditionParameter || condition.Kind == GateConditionCapability {
			condition.Parameter.Function = targetGateFunction("Origin")
		}
	}
	if err := validateTargetGateEvidenceForProfile(analysis, logicalOwner); err != nil {
		t.Fatalf("connected logical-owner parameter rejected: %v", err)
	}

	disconnected := targetGateRegistry()
	firstTargetGateRoute(&disconnected).Hops[0].Callee = targetGateFunction("Unrelated")
	assertTargetGateEvidenceError(t, analysis, disconnected, "gate parameter function", "authored route chain is not connected")
}

func TestValidateTargetGateEvidenceForProfileProvesPredicateShapes(t *testing.T) {
	tests := []struct {
		name      string
		predicate ObjectRef
		expected  string
		want      []string
	}{
		{
			name:      "missing predicate",
			predicate: targetGateObject("", "MissingPredicate"),
			expected:  "true",
			want:      []string{"gate predicate", "does not exist"},
		},
		{
			name:      "wrong result type",
			predicate: targetGateObject("", "BadStringPredicate"),
			expected:  "true",
			want:      []string{"gate predicate", "must produce one boolean value", "string"},
		},
		{
			name:      "multiple results",
			predicate: targetGateObject("", "BadPairPredicate"),
			expected:  "true",
			want:      []string{"gate predicate", "has 2 results", "exactly 1"},
		},
		{
			name:      "requires argument",
			predicate: targetGateObject("", "PredicateNeedsArgument"),
			expected:  "true",
			want:      []string{"gate predicate", "has 1 parameter", "must be closed"},
		},
		{
			name:      "incompatible expectation",
			predicate: targetGateObject("", "GoodPredicate"),
			expected:  "non-empty",
			want:      []string{`predicate expectation "non-empty"`, `must be "true" or "false"`},
		},
		{
			name:      "uninstantiated generic predicate",
			predicate: targetGateObject("", "GenericPredicate"),
			expected:  "true",
			want:      []string{"gate predicate", "is an uninstantiated generic function"},
		},
	}

	analysis := loadTargetGateFixture(t)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := targetGateRegistry()
			condition := &firstTargetGateRoute(&registry).CurrentGate.Conditions[0]
			condition.Predicate = tt.predicate
			condition.Expected = tt.expected
			assertTargetGateEvidenceError(t, analysis, registry, tt.want...)
		})
	}

	for _, predicate := range []ObjectRef{
		targetGateObject("", "EnabledConstant"),
		targetGateObject("Gate", "Enabled"),
		targetGateObject("Gate", "Ready"),
	} {
		registry := targetGateRegistry()
		firstTargetGateRoute(&registry).CurrentGate.Conditions[0].Predicate = predicate
		if err := validateTargetGateEvidenceForProfile(analysis, registry); err != nil {
			t.Errorf("valid predicate %s rejected: %v", predicate.key(), err)
		}
	}
}

func TestValidateTargetGateEvidenceForProfileUsesOnlyActiveUnambiguousCase(t *testing.T) {
	analysis := loadTargetGateFixture(t)
	registry := targetGateRegistry()
	valid := registry.Registrations[0].Cases[0].Routes[0]
	inactive := valid
	inactive.CurrentGate.Conditions = append([]GateCondition(nil), valid.CurrentGate.Conditions...)
	inactive.CurrentGate.Conditions[0].Predicate.Name = "MissingDarwinPredicate"
	registry.Registrations[0].Cases = append(registry.Registrations[0].Cases, ProfileCase{
		BuildProfiles: []BuildProfileID{BuildDarwinDefault},
		Routes:        []Route{inactive},
	})
	if err := validateTargetGateEvidenceForProfile(analysis, registry); err != nil {
		t.Fatalf("inactive route affected Linux evidence: %v", err)
	}

	registry.Registrations[0].Cases = append(registry.Registrations[0].Cases, ProfileCase{
		BuildProfiles: []BuildProfileID{BuildLinuxDefault},
		Routes:        []Route{valid},
	})
	assertTargetGateEvidenceError(t, analysis, registry, "profile has 2 active classification cases")

	duplicateMembership := targetGateRegistry()
	duplicateMembership.Registrations[0].Cases[0].BuildProfiles = append(
		duplicateMembership.Registrations[0].Cases[0].BuildProfiles,
		BuildLinuxDefault,
	)
	assertTargetGateEvidenceError(t, analysis, duplicateMembership, `profile "linux/default" appears 2 times`)
}

func TestValidateTargetGateEvidenceForProfileDiagnosticsAreDeterministic(t *testing.T) {
	analysis := loadTargetGateFixture(t)
	registry := targetGateRegistry()
	route := firstTargetGateRoute(&registry)
	route.Target.Identities[0].Projection = targetGateObject("Other", "ID")
	route.CurrentGate.Conditions[0].Predicate = targetGateObject("", "BadStringPredicate")
	route.CurrentGate.Conditions[2].Capability.Name = "ConcreteCapability"

	var baseline string
	for repetition := 0; repetition < 4; repetition++ {
		candidate := targetGateRegistry()
		*firstTargetGateRoute(&candidate) = cloneTargetGateRoute(*route)
		if repetition%2 == 1 {
			reverse(firstTargetGateRoute(&candidate).Target.Identities)
			reverse(firstTargetGateRoute(&candidate).CurrentGate.Conditions)
		}
		err := validateTargetGateEvidenceForProfile(analysis, candidate)
		if err == nil {
			t.Fatal("validateTargetGateEvidenceForProfile() error = nil")
		}
		if baseline == "" {
			baseline = err.Error()
			continue
		}
		if err.Error() != baseline {
			t.Fatalf("diagnostics changed\n got:\n%s\nwant:\n%s", err, baseline)
		}
	}
	lines := strings.Split(strings.TrimPrefix(baseline, "effect target/gate evidence failed for profile \"linux/default\":\n- "), "\n- ")
	sorted := append([]string(nil), lines...)
	sort.Strings(sorted)
	if !reflect.DeepEqual(lines, sorted) {
		t.Fatalf("diagnostics are not sorted:\n%s", baseline)
	}
}

func TestValidateTargetGateEvidenceForProfileFailsClosedAndIsConcurrentReadOnly(t *testing.T) {
	if err := validateTargetGateEvidenceForProfile(nil, targetGateRegistry()); err == nil || !strings.Contains(err.Error(), "loaded analysis is required") {
		t.Fatalf("nil analysis error = %v, want fail-closed diagnostic", err)
	}

	analysis := loadTargetGateFixture(t)
	registry := targetGateRegistry()
	const workers = 8
	errors := make(chan error, workers)
	var group sync.WaitGroup
	group.Add(workers)
	for range workers {
		go func() {
			defer group.Done()
			errors <- validateTargetGateEvidenceForProfile(analysis, registry)
		}()
	}
	group.Wait()
	close(errors)
	for err := range errors {
		if err != nil {
			t.Errorf("validateTargetGateEvidenceForProfile() error: %v", err)
		}
	}
}

func loadTargetGateFixture(t *testing.T) *loadedAnalysis {
	t.Helper()
	analysis, err := loadAnalysis(context.Background(), fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/targetgate",
	}), fixtureLinuxProfile())
	if err != nil {
		t.Fatalf("loadAnalysis() error: %v", err)
	}
	return analysis
}

func targetGateRegistry() Registry {
	middle := targetGateFunction("Middle")
	origin := targetGateFunction("Origin")
	return Registry{
		Boundaries: []BoundaryDefinition{{
			ID:     "targetgate.mutator.apply",
			Kind:   KindProviderMutation,
			Object: targetGateObject("Mutator", "Apply"),
			Match:  ObjectMatchInterfaceImplementors,
		}},
		Registrations: []SiteRegistration{{
			BoundaryID: "targetgate.mutator.apply",
			Matcher:    OperationSite{Operation: OperationCall, Enclosing: middle, Ordinal: 1},
			Cases: []ProfileCase{{
				BuildProfiles: []BuildProfileID{BuildLinuxDefault},
				Routes: []Route{{
					LogicalOwner: origin,
					Hops: []RouteHop{{
						Site:     OperationSite{Operation: OperationCall, Enclosing: origin, Ordinal: 1},
						Dispatch: HopDispatchExact,
						Callee:   middle,
					}},
					Target: TargetRef{Identities: []TargetIdentityRef{
						{
							Role:         TargetRoleInput,
							BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
							Projection:   targetGateObject("Item", "ID"),
							Source:       TargetSourceFunctionResult,
							SourceObject: targetGateObject("", "Lookup"),
							SourceSlot:   ValueSlot{Kind: SlotResult, Index: 1},
						},
						{
							Role:         TargetRoleGenerated,
							BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
							Projection:   targetGateObject("Item", "ID"),
							Source:       TargetSourceBoundaryValue,
						},
						{
							Role:         TargetRolePrimary,
							BoundarySlot: ValueSlot{Kind: SlotReceiver},
							Source:       TargetSourceBoundaryValue,
						},
					}},
					CurrentGate: GateRef{Kind: GateAll, Conditions: []GateCondition{
						{
							Kind:      GateConditionPredicate,
							Predicate: targetGateObject("", "GoodPredicate"),
							Expected:  "true",
						},
						{
							Kind: GateConditionParameter,
							Parameter: GateParameterRef{
								Function: middle,
								Slot:     ValueSlot{Kind: SlotParameter, Index: 1},
							},
							Expected: "non-nil",
						},
						{
							Kind: GateConditionCapability,
							Parameter: GateParameterRef{
								Function: middle,
								Slot:     ValueSlot{Kind: SlotParameter, Index: 1},
							},
							Capability: targetGateObject("", "OptionalMutator"),
							Expected:   GateCapabilityAvailable,
						},
					}},
				}},
			}},
		}},
	}
}

func targetGateConcreteRegistry() Registry {
	middle := targetGateFunction("ConcreteMiddle")
	origin := targetGateFunction("ConcreteOrigin")
	return Registry{
		Boundaries: []BoundaryDefinition{{
			ID:     "targetgate.sink.apply",
			Kind:   KindProviderMutation,
			Object: targetGateObject("Sink", "Apply"),
			Match:  ObjectMatchExact,
		}},
		Registrations: []SiteRegistration{{
			BoundaryID: "targetgate.sink.apply",
			Matcher:    OperationSite{Operation: OperationCall, Enclosing: middle, Ordinal: 1},
			Cases: []ProfileCase{{BuildProfiles: []BuildProfileID{BuildLinuxDefault}, Routes: []Route{{
				LogicalOwner: origin,
				Hops: []RouteHop{{
					Site:     OperationSite{Operation: OperationCall, Enclosing: origin, Ordinal: 1},
					Dispatch: HopDispatchExact,
					Callee:   middle,
				}},
				Target: TargetRef{Identities: []TargetIdentityRef{{
					BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
					Projection:   targetGateObject("Item", "ID"),
					Source:       TargetSourceBoundaryValue,
				}, {
					BoundarySlot: ValueSlot{Kind: SlotReceiver},
					Projection:   targetGateObject("Sink", "Name"),
					Source:       TargetSourceBoundaryValue,
				}}},
				CurrentGate: GateRef{Kind: GatePredicate, Predicate: targetGateObject("", "GoodPredicate"), Expected: "true"},
			}}}},
		}},
	}
}

func targetGateChannelRegistry(channel string, projection ObjectRef) Registry {
	owner := targetGateFunction("ChannelRoute")
	return Registry{
		Boundaries: []BoundaryDefinition{{
			ID:     "targetgate.channel",
			Kind:   KindWakeSource,
			Object: targetGateObject("", channel),
			Match:  ObjectMatchChannel,
		}},
		Registrations: []SiteRegistration{{
			BoundaryID: "targetgate.channel",
			Matcher:    OperationSite{Operation: OperationChannelSend, Enclosing: owner, Ordinal: 1},
			Cases: []ProfileCase{{BuildProfiles: []BuildProfileID{BuildLinuxDefault}, Routes: []Route{{
				LogicalOwner: owner,
				Target: TargetRef{Identities: []TargetIdentityRef{{
					BoundarySlot: ValueSlot{Kind: SlotChannelElement},
					Projection:   projection,
					Source:       TargetSourceBoundaryValue,
				}}},
				CurrentGate: GateRef{Kind: GateUnconditionalLegacy},
			}}}},
		}},
	}
}

func targetGateGenericProjectionRegistry() Registry {
	owner := targetGateFunction("BoxMiddle")
	return Registry{
		Boundaries: []BoundaryDefinition{{
			ID:     "targetgate.apply-box",
			Kind:   KindProviderMutation,
			Object: targetGateObject("", "ApplyBox"),
			Match:  ObjectMatchExact,
		}},
		Registrations: []SiteRegistration{{
			BoundaryID: "targetgate.apply-box",
			Matcher:    OperationSite{Operation: OperationCall, Enclosing: owner, Ordinal: 1},
			Cases: []ProfileCase{{BuildProfiles: []BuildProfileID{BuildLinuxDefault}, Routes: []Route{{
				LogicalOwner: owner,
				Target: TargetRef{Identities: []TargetIdentityRef{
					{
						BoundarySlot: ValueSlot{Kind: SlotParameter, Index: 1},
						Projection:   targetGateObject("Box", "ID"),
						Source:       TargetSourceBoundaryValue,
					},
					{
						BoundarySlot: ValueSlot{Kind: SlotResult, Index: 1},
						Projection:   targetGateObject("Box", "ID"),
						Source:       TargetSourceBoundaryValue,
					},
				}},
				CurrentGate: GateRef{Kind: GateUnconditionalLegacy},
			}}}},
		}},
	}
}

func targetGateObject(receiver, name string) ObjectRef {
	return ObjectRef{Package: targetGateFixturePackage, Receiver: receiver, Name: name}
}

func targetGateFunction(name string) FunctionRef {
	return FunctionRef{
		Object: targetGateObject("", name),
		File:   "internal/reconciletest/effectinventory/testdata/analyzerfixture/targetgate/targetgate.go",
	}
}

func firstTargetGateRoute(registry *Registry) *Route {
	return &registry.Registrations[0].Cases[0].Routes[0]
}

func cloneTargetGateRoute(route Route) Route {
	clone := route
	clone.Hops = append([]RouteHop(nil), route.Hops...)
	clone.Target = cloneTarget(route.Target)
	clone.CurrentGate = cloneGate(route.CurrentGate)
	return clone
}

func assertTargetGateEvidenceError(t *testing.T, analysis *loadedAnalysis, registry Registry, wants ...string) {
	t.Helper()
	err := validateTargetGateEvidenceForProfile(analysis, registry)
	if err == nil {
		t.Fatalf("validateTargetGateEvidenceForProfile() error = nil, want %q", wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("validateTargetGateEvidenceForProfile() error = %q, want %q", err, want)
		}
	}
}
