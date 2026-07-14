package effectinventory

import (
	"reflect"
	"testing"
)

func TestValidateRegistryAcceptsSimpleAndCompoundGates(t *testing.T) {
	tests := []struct {
		name string
		gate GateRef
	}{
		{
			name: "simple predicate compatibility form",
			gate: GateRef{
				Kind:      GatePredicate,
				Predicate: objectRef("github.com/gastownhall/gascity/cmd/gc", "CityRuntime", "routeRecoveryEnabled"),
				Expected:  "true",
			},
		},
		{
			name: "single parameter capability",
			gate: GateRef{
				Kind: GateAll,
				Conditions: []GateCondition{{
					Kind:       GateConditionCapability,
					Parameter:  graphStoreParameter(),
					Capability: objectRef(beadsPackage, "", "GraphApplyStore"),
					Expected:   GateCapabilityAvailable,
				}},
			},
		},
		{
			name: "compound field parameter and capability",
			gate: compoundGraphGate(),
		},
		{
			name: "alternative capabilities",
			gate: GateRef{
				Kind: GateAny,
				Conditions: []GateCondition{
					{
						Kind:       GateConditionCapability,
						Parameter:  graphStoreParameter(),
						Capability: objectRef(beadsPackage, "", "GraphApplyStore"),
						Expected:   GateCapabilityAvailable,
					},
					{
						Kind:       GateConditionCapability,
						Parameter:  graphStoreParameter(),
						Capability: objectRef(beadsPackage, "", "StorageGraphApplyStore"),
						Expected:   GateCapabilityAvailable,
					},
				},
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			firstRoute(&registry).CurrentGate = tt.gate
			if err := validateRegistry(registry, validationDate()); err != nil {
				t.Fatalf("ValidateRegistry() rejected gate: %v", err)
			}
		})
	}
}

func TestValidateRegistryRejectsImpossibleGateForms(t *testing.T) {
	tests := []struct {
		name string
		gate GateRef
		want string
	}{
		{
			name: "unconditional with conditions",
			gate: GateRef{Kind: GateUnconditionalLegacy, Conditions: []GateCondition{{
				Kind: GateConditionPredicate, Predicate: objectRef("example", "Config", "Enabled"), Expected: "true",
			}}},
			want: "unconditional gate cannot name conditions",
		},
		{
			name: "simple predicate with conditions",
			gate: GateRef{
				Kind:       GatePredicate,
				Predicate:  objectRef("example", "Config", "Enabled"),
				Expected:   "true",
				Conditions: []GateCondition{{Kind: GateConditionPredicate, Predicate: objectRef("example", "Config", "Ready"), Expected: "true"}},
			},
			want: "simple predicate gate cannot name conditions",
		},
		{
			name: "all without conditions",
			gate: GateRef{Kind: GateAll},
			want: "all gate requires at least one condition",
		},
		{
			name: "any with one condition",
			gate: GateRef{Kind: GateAny, Conditions: []GateCondition{{
				Kind: GateConditionPredicate, Predicate: objectRef("example", "Config", "Enabled"), Expected: "true",
			}}},
			want: "any gate requires at least two conditions",
		},
		{
			name: "compound with top-level predicate",
			gate: GateRef{
				Kind:       GateAll,
				Predicate:  objectRef("example", "Config", "Enabled"),
				Conditions: []GateCondition{{Kind: GateConditionPredicate, Predicate: objectRef("example", "Config", "Ready"), Expected: "true"}},
			},
			want: "compound gate cannot name a top-level predicate",
		},
		{
			name: "duplicate condition",
			gate: func() GateRef {
				condition := GateCondition{Kind: GateConditionPredicate, Predicate: objectRef("example", "Config", "Enabled"), Expected: "true"}
				return GateRef{Kind: GateAll, Conditions: []GateCondition{condition, condition}}
			}(),
			want: "duplicate gate condition",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			firstRoute(&registry).CurrentGate = tt.gate
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestValidateRegistryRejectsMalformedGateConditions(t *testing.T) {
	tests := []struct {
		name      string
		condition GateCondition
		want      string
	}{
		{
			name:      "unknown",
			condition: GateCondition{Kind: GateConditionKind("magic"), Expected: "true"},
			want:      `unknown gate condition "magic"`,
		},
		{
			name: "predicate with parameter",
			condition: GateCondition{
				Kind: GateConditionPredicate, Predicate: objectRef("example", "Config", "Enabled"), Parameter: graphStoreParameter(), Expected: "true",
			},
			want: "predicate condition cannot name a parameter",
		},
		{
			name:      "parameter without owner",
			condition: GateCondition{Kind: GateConditionParameter, Parameter: GateParameterRef{Slot: ValueSlot{Kind: SlotParameter, Index: 1}}, Expected: "non-default"},
			want:      "gate parameter function is required",
		},
		{
			name:      "parameter with result slot",
			condition: GateCondition{Kind: GateConditionParameter, Parameter: GateParameterRef{Function: graphStoreParameter().Function, Slot: ValueSlot{Kind: SlotResult, Index: 1}}, Expected: "non-default"},
			want:      `gate parameter slot: slot must be "parameter"`,
		},
		{
			name:      "parameter with capability",
			condition: GateCondition{Kind: GateConditionParameter, Parameter: graphStoreParameter(), Capability: objectRef(beadsPackage, "", "GraphApplyStore"), Expected: "non-default"},
			want:      "parameter condition cannot name a capability",
		},
		{
			name:      "capability without interface",
			condition: GateCondition{Kind: GateConditionCapability, Parameter: graphStoreParameter(), Expected: GateCapabilityAvailable},
			want:      "gate capability package is required",
		},
		{
			name:      "capability names method",
			condition: GateCondition{Kind: GateConditionCapability, Parameter: graphStoreParameter(), Capability: objectRef(beadsPackage, "GraphApplyStore", "ApplyGraphPlan"), Expected: GateCapabilityAvailable},
			want:      "gate capability must name a receiverless type",
		},
		{
			name:      "capability has free-form expectation",
			condition: GateCondition{Kind: GateConditionCapability, Parameter: graphStoreParameter(), Capability: objectRef(beadsPackage, "", "GraphApplyStore"), Expected: "probably"},
			want:      `capability expectation "probably" must be "available" or "unavailable"`,
		},
		{
			name:      "missing expectation",
			condition: GateCondition{Kind: GateConditionParameter, Parameter: graphStoreParameter()},
			want:      "gate condition expected value is required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			firstRoute(&registry).CurrentGate = GateRef{Kind: GateAll, Conditions: []GateCondition{tt.condition}}
			assertErrorContains(t, validateRegistry(registry, validationDate()), tt.want)
		})
	}
}

func TestCompileRegistryCanonicalizesGateConditionsAndDoesNotAlias(t *testing.T) {
	forward := validRegistry()
	firstRoute(&forward).CurrentGate = compoundGraphGate()
	reversed := validRegistry()
	firstRoute(&reversed).CurrentGate = compoundGraphGate()
	reverse(firstRoute(&reversed).CurrentGate.Conditions)

	forwardCompiled, err := CompileRegistry(forward, discoveryForRegistry(forward), validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry(forward) failed: %v", err)
	}
	reversedCompiled, err := CompileRegistry(reversed, discoveryForRegistry(reversed), validationDate())
	if err != nil {
		t.Fatalf("CompileRegistry(reversed) failed: %v", err)
	}
	if !reflect.DeepEqual(forwardCompiled, reversedCompiled) {
		t.Fatalf("compiled registry changed with gate condition order\nforward: %#v\nreverse: %#v", forwardCompiled, reversedCompiled)
	}

	firstRoute(&forward).CurrentGate.Conditions[0].Expected = "changed"
	got := forwardCompiled.Registrations[0].Cases[0].Routes[0].Definition.CurrentGate.Conditions
	for _, condition := range got {
		if condition.Expected == "changed" {
			t.Fatal("compiled registry retained mutable gate condition input")
		}
	}
}

func TestGateDiagnosticsIgnoreConditionInputOrder(t *testing.T) {
	forward := validRegistry()
	forwardGate := compoundGraphGate()
	forwardGate.Conditions[0].Expected = ""
	forwardGate.Conditions[2].Expected = "maybe"
	firstRoute(&forward).CurrentGate = forwardGate

	reversed := validRegistry()
	reversedGate := compoundGraphGate()
	reversedGate.Conditions[0].Expected = ""
	reversedGate.Conditions[2].Expected = "maybe"
	reverse(reversedGate.Conditions)
	firstRoute(&reversed).CurrentGate = reversedGate

	forwardErr := validateRegistry(forward, validationDate())
	reverseErr := validateRegistry(reversed, validationDate())
	if forwardErr == nil || reverseErr == nil {
		t.Fatalf("invalid gate errors = (%v, %v), want both non-nil", forwardErr, reverseErr)
	}
	if forwardErr.Error() != reverseErr.Error() {
		t.Fatalf("gate diagnostics changed with condition order\nforward:\n%s\nreverse:\n%s", forwardErr, reverseErr)
	}
}

func TestCompileRegistryRouteIDIncludesCompoundGateSemantics(t *testing.T) {
	base := validRegistry()
	firstRoute(&base).CurrentGate = compoundGraphGate()
	baseID := compiledFixtureRouteID(t, base)

	tests := []struct {
		name   string
		mutate func(*GateRef)
	}{
		{"connective", func(gate *GateRef) { gate.Kind = GateAny }},
		{"predicate expectation", func(gate *GateRef) { gate.Conditions[0].Expected = "false" }},
		{"parameter identity", func(gate *GateRef) { gate.Conditions[1].Parameter.Slot.Index = 3 }},
		{"capability identity", func(gate *GateRef) { gate.Conditions[2].Capability.Name = "StorageGraphApplyStore" }},
		{"capability expectation", func(gate *GateRef) { gate.Conditions[2].Expected = GateCapabilityUnavailable }},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			registry := validRegistry()
			firstRoute(&registry).CurrentGate = compoundGraphGate()
			tt.mutate(&firstRoute(&registry).CurrentGate)
			if got := compiledFixtureRouteID(t, registry); got == baseID {
				t.Fatalf("route ID stayed %q after %s changed", got, tt.name)
			}
		})
	}
}

func TestCanonicalGateCoversEveryAuthoredConditionField(t *testing.T) {
	base := compoundGraphGate().Conditions[2]
	wantDifferent := []struct {
		name   string
		mutate func(*GateCondition)
	}{
		{"Kind", func(condition *GateCondition) { condition.Kind = GateConditionParameter }},
		{"Predicate", func(condition *GateCondition) { condition.Predicate = objectRef("example", "Config", "Enabled") }},
		{"Parameter", func(condition *GateCondition) { condition.Parameter.Slot.Index++ }},
		{"Capability", func(condition *GateCondition) { condition.Capability.Name = "StorageGraphApplyStore" }},
		{"Expected", func(condition *GateCondition) { condition.Expected = GateCapabilityUnavailable }},
	}

	baseline := canonicalGateCondition(base)
	for _, tt := range wantDifferent {
		t.Run(tt.name, func(t *testing.T) {
			candidate := base
			tt.mutate(&candidate)
			if got := canonicalGateCondition(candidate); got == baseline {
				t.Fatalf("canonicalGateCondition() did not cover GateCondition.%s", tt.name)
			}
		})
	}
}

func TestCanonicalGateCoversEveryAuthoredGateField(t *testing.T) {
	base := GateRef{
		Kind:      GatePredicate,
		Predicate: objectRef("example", "Config", "Enabled"),
		Expected:  "true",
	}
	wantDifferent := []struct {
		name   string
		mutate func(*GateRef)
	}{
		{"Kind", func(gate *GateRef) { gate.Kind = GateAll }},
		{"Predicate", func(gate *GateRef) { gate.Predicate.Name = "Ready" }},
		{"Expected", func(gate *GateRef) { gate.Expected = "false" }},
		{"Conditions", func(gate *GateRef) {
			gate.Conditions = []GateCondition{{Kind: GateConditionParameter, Parameter: graphStoreParameter(), Expected: "non-nil"}}
		}},
	}

	baseline := canonicalGateRef(base)
	for _, tt := range wantDifferent {
		t.Run(tt.name, func(t *testing.T) {
			candidate := cloneGate(base)
			tt.mutate(&candidate)
			if got := canonicalGateRef(candidate); got == baseline {
				t.Fatalf("canonicalGateRef() did not cover GateRef.%s", tt.name)
			}
		})
	}
}

func graphStoreParameter() GateParameterRef {
	return GateParameterRef{
		Function: functionRef(
			"github.com/gastownhall/gascity/cmd/gc",
			"cmd/gc/bead_policy_store.go",
			"applyGraphWithPolicy",
		),
		Slot: ValueSlot{Kind: SlotParameter, Index: 2},
	}
}

func compoundGraphGate() GateRef {
	return GateRef{
		Kind: GateAll,
		Conditions: []GateCondition{
			{
				Kind:      GateConditionPredicate,
				Predicate: objectRef("github.com/gastownhall/gascity/cmd/gc", "beadPolicyGraphStore", "enabled"),
				Expected:  "true",
			},
			{
				Kind:      GateConditionParameter,
				Parameter: graphStoreParameter(),
				Expected:  "non-nil",
			},
			{
				Kind:       GateConditionCapability,
				Parameter:  graphStoreParameter(),
				Capability: objectRef(beadsPackage, "", "GraphApplyStore"),
				Expected:   GateCapabilityAvailable,
			},
		},
	}
}
