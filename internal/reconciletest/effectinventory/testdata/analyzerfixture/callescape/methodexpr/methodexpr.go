// Package methodexpr exercises an open-world callback whose signature can
// hold the method expression for an exact inventoried method.
package methodexpr

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// Impl owns the exact inventoried mutation method.
type Impl struct{}

// Mutate is the exact method boundary under test.
func (Impl) Mutate(boundary.Target) {}

// InvokeInjectedMethodExpression invokes a callback supplied by an external
// caller. Impl.Mutate is assignable to callback as a method expression.
func InvokeInjectedMethodExpression(callback func(Impl, boundary.Target), impl Impl, target boundary.Target) {
	callback(impl, target)
}

// InvokeInjectedPointerAdaptedMethodExpression accepts the pointer-adapted
// method expression (*Impl).Mutate for the value-receiver boundary.
func InvokeInjectedPointerAdaptedMethodExpression(callback func(*Impl, boundary.Target), impl *Impl, target boundary.Target) {
	callback(impl, target)
}
