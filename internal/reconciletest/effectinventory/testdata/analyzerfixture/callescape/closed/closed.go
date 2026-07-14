// Package closed exercises callable values whose provenance remains fully
// local and closed.
package closed

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// Impl owns an exact concrete mutation method.
type Impl struct{}

// Mutate is the exact method boundary under test.
func (Impl) Mutate(boundary.Target) {}

// BoundConcreteMethod captures a bound method from a concrete receiver
// that cannot be replaced by an open-world caller.
func BoundConcreteMethod(impl Impl, target boundary.Target) {
	mutate := impl.Mutate
	mutate(target)
}

// LocalFunctionSlot invokes a local slot that is neither published nor
// passed by address.
func LocalFunctionSlot(target boundary.Target) {
	effect := boundary.Emit
	effect(target)
}
