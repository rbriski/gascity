package effectinventory

import (
	"fmt"
	"sort"
	"strings"
)

func cloneGate(gate GateRef) GateRef {
	clone := gate
	clone.Conditions = append([]GateCondition(nil), gate.Conditions...)
	for index := range clone.Conditions {
		clone.Conditions[index].Parameter.Function.ClosurePath = append(
			[]int(nil),
			gate.Conditions[index].Parameter.Function.ClosurePath...,
		)
	}
	sort.Slice(clone.Conditions, func(i, j int) bool {
		return canonicalGateCondition(clone.Conditions[i]) < canonicalGateCondition(clone.Conditions[j])
	})
	return clone
}

func canonicalGateRef(gate GateRef) string {
	conditions := make([]string, len(gate.Conditions))
	for index, condition := range gate.Conditions {
		conditions[index] = canonicalGateCondition(condition)
	}
	sort.Strings(conditions)
	return canonicalFields(
		"gate-v2",
		string(gate.Kind),
		canonicalObjectRef(gate.Predicate),
		gate.Expected,
		canonicalStringList("gate-conditions-v1", conditions),
	)
}

func canonicalGateCondition(condition GateCondition) string {
	return canonicalFields(
		"gate-condition-v1",
		string(condition.Kind),
		canonicalObjectRef(condition.Predicate),
		canonicalGateParameterRef(condition.Parameter),
		canonicalObjectRef(condition.Capability),
		condition.Expected,
	)
}

func canonicalGateParameterRef(parameter GateParameterRef) string {
	return canonicalFields(
		"gate-parameter-v1",
		canonicalFunctionRef(parameter.Function),
		canonicalValueSlot(parameter.Slot),
	)
}

func validateGate(gate GateRef, scope string, problems *[]string) {
	switch gate.Kind {
	case GateUnconditionalLegacy:
		if !gate.Predicate.zero() {
			addProblem(problems, scope, "unconditional gate cannot name a predicate")
		}
		if gate.Expected != "" {
			addProblem(problems, scope, "unconditional gate cannot name an expected value")
		}
		if len(gate.Conditions) != 0 {
			addProblem(problems, scope, "unconditional gate cannot name conditions")
		}
	case GatePredicate:
		validateObject(gate.Predicate, scope+" gate predicate", problems)
		if strings.TrimSpace(gate.Expected) == "" {
			addProblem(problems, scope, "gate expected value is required")
		}
		if len(gate.Conditions) != 0 {
			addProblem(problems, scope, "simple predicate gate cannot name conditions")
		}
	case GateAll, GateAny:
		if !gate.Predicate.zero() {
			addProblem(problems, scope, "compound gate cannot name a top-level predicate")
		}
		if gate.Expected != "" {
			addProblem(problems, scope, "compound gate cannot name a top-level expected value")
		}
		if gate.Kind == GateAll && len(gate.Conditions) == 0 {
			addProblem(problems, scope, "all gate requires at least one condition")
		}
		if gate.Kind == GateAny && len(gate.Conditions) < 2 {
			addProblem(problems, scope, "any gate requires at least two conditions")
		}
		validateGateConditions(gate.Conditions, scope, problems)
	default:
		addProblem(problems, scope, "unknown current gate %q", gate.Kind)
	}
}

func validateGateConditions(conditions []GateCondition, scope string, problems *[]string) {
	conditions = append([]GateCondition(nil), conditions...)
	sort.Slice(conditions, func(i, j int) bool {
		return canonicalGateCondition(conditions[i]) < canonicalGateCondition(conditions[j])
	})
	seen := make(map[string]bool, len(conditions))
	for _, condition := range conditions {
		key := canonicalGateCondition(condition)
		conditionScope := fmt.Sprintf("%s gate condition %q", scope, deriveContentID("condition-v1-", key))
		if seen[key] {
			addProblem(problems, scope, "duplicate gate condition %q", deriveContentID("condition-v1-", key))
		}
		seen[key] = true
		validateGateCondition(condition, conditionScope, problems)
	}
}

func validateGateCondition(condition GateCondition, scope string, problems *[]string) {
	switch condition.Kind {
	case GateConditionPredicate:
		validateObject(condition.Predicate, scope+" predicate", problems)
		if !condition.Parameter.zero() {
			addProblem(problems, scope, "predicate condition cannot name a parameter")
		}
		if !condition.Capability.zero() {
			addProblem(problems, scope, "predicate condition cannot name a capability")
		}
		validateGateExpected(condition.Expected, scope, problems)
	case GateConditionParameter:
		if !condition.Predicate.zero() {
			addProblem(problems, scope, "parameter condition cannot name a predicate")
		}
		validateGateParameter(condition.Parameter, scope, problems)
		if !condition.Capability.zero() {
			addProblem(problems, scope, "parameter condition cannot name a capability")
		}
		validateGateExpected(condition.Expected, scope, problems)
	case GateConditionCapability:
		if !condition.Predicate.zero() {
			addProblem(problems, scope, "capability condition cannot name a predicate")
		}
		validateGateParameter(condition.Parameter, scope, problems)
		validateObject(condition.Capability, scope+" gate capability", problems)
		if condition.Capability.Receiver != "" {
			addProblem(problems, scope, "gate capability must name a receiverless type")
		}
		if !oneOf(condition.Expected, GateCapabilityAvailable, GateCapabilityUnavailable) {
			addProblem(
				problems,
				scope,
				"capability expectation %q must be %q or %q",
				condition.Expected,
				GateCapabilityAvailable,
				GateCapabilityUnavailable,
			)
		}
	default:
		addProblem(problems, scope, "unknown gate condition %q", condition.Kind)
	}
}

func validateGateExpected(expected, scope string, problems *[]string) {
	if strings.TrimSpace(expected) == "" {
		addProblem(problems, scope, "gate condition expected value is required")
	}
}

func validateGateParameter(parameter GateParameterRef, scope string, problems *[]string) {
	if parameter.Function.zero() {
		addProblem(problems, scope, "gate parameter function is required")
	} else {
		validateFunction(parameter.Function, scope+" gate parameter function", problems)
	}
	validateExactSlot(parameter.Slot, SlotParameter, scope+" gate parameter slot", problems)
}

func (function FunctionRef) zero() bool {
	return function.Object.zero() && function.File == "" && len(function.ClosurePath) == 0
}

func (parameter GateParameterRef) zero() bool {
	return parameter.Function.zero() && parameter.Slot.zero()
}
