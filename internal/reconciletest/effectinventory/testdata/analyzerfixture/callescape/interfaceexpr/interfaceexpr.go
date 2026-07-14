// Package interfaceexpr exercises an injected method expression whose
// receiver interface is narrower than the inventoried interface.
package interfaceexpr

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// NarrowMutator shares the target method but omits Complete.
type NarrowMutator interface {
	Mutate(boundary.Target)
}

// InvokeNarrowMethodExpression accepts NarrowMutator.Mutate as an external
// callback. Its dynamic receiver may also implement CompleteMutator.
func InvokeNarrowMethodExpression(callback func(NarrowMutator, boundary.Target), receiver NarrowMutator, target boundary.Target) {
	callback(receiver, target)
}
