// Package escape contains deliberately unresolved effect routes. Analyzer
// tests opt into this package to verify that dynamic and reflective dispatch
// fail closed without contaminating the positive fixture packages.
package escape

import (
	"reflect"

	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"
)

// DynamicFunctionParameter invokes an effect supplied only at runtime.
func DynamicFunctionParameter(effect boundary.EffectFunc, target boundary.Target) {
	effect(target)
}

// DynamicFunctionField invokes an effect loaded from an unknown holder.
func DynamicFunctionField(holder boundary.EffectHolder, target boundary.Target) {
	holder.Effect(target)
}

// SeedKnownDynamicTargets proves that VTA's known targets do not close an
// exported callback parameter or function field to values supplied by callers
// outside the analyzed packages.
func SeedKnownDynamicTargets(target boundary.Target) boundary.EffectHolder {
	DynamicFunctionParameter(boundary.Emit, target)
	return boundary.EffectHolder{Effect: boundary.Emit}
}

// ReflectiveMethod hides an interface boundary invocation behind reflection.
func ReflectiveMethod(mutator boundary.Mutator, target boundary.Target) {
	reflect.ValueOf(mutator.Mutate).Call([]reflect.Value{reflect.ValueOf(target)})
}
