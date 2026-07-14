// Package promotedexpr exercises an injected promoted method expression.
package promotedexpr

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// Inner declares the exact method boundary.
type Inner struct{}

// Mutate is the exact method boundary.
func (Inner) Mutate(boundary.Target) {}

// Outer promotes Inner.Mutate.
type Outer struct {
	Inner
}

// InvokePromotedMethodExpression accepts Outer.Mutate as an external callback,
// which resolves to the exact method declared by Inner.
func InvokePromotedMethodExpression(callback func(Outer, boundary.Target), receiver Outer, target boundary.Target) {
	callback(receiver, target)
}
