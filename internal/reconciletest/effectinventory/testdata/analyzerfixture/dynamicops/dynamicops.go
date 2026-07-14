// Package dynamicops exercises closed local function values and deliberately
// overlapping typed effect boundaries.
package dynamicops

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// ClosedFunctionValueOperations invokes one exact function boundary through
// separately initialized local values for each supported call operation.
func ClosedFunctionValueOperations(target boundary.Target) {
	callEffect := boundary.Emit
	callEffect(target)

	goEffect := boundary.Emit
	go goEffect(target)

	deferEffect := boundary.Emit
	defer deferEffect(target)
}

// AmbiguousConcreteMethodCall is one physical call that satisfies both the
// exact concrete-method and interface-implementor boundary definitions used by
// the analyzer test.
func AmbiguousConcreteMethodCall(mutator boundary.ValueMutator, target boundary.Target) {
	mutator.Mutate(target)
}
