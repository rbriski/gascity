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

// NarrowMutator deliberately owns only the target method's signature. It is
// not the exact Impl method boundary selected by the inventory.
type NarrowMutator interface {
	Mutate(boundary.Target)
}

// NarrowImpl is a closed, unrelated implementation of NarrowMutator.
type NarrowImpl struct{}

// Mutate has the same signature as Impl.Mutate without sharing its object.
func (NarrowImpl) Mutate(boundary.Target) {}

// SignatureOnlyInterfaceCall proves that an unresolved interface dispatch is
// not an exact boundary match merely because its method signature collides.
func SignatureOnlyInterfaceCall(mutator NarrowMutator, target boundary.Target) {
	mutator.Mutate(target)
}

func unrelatedEffect(boundary.Target) {}

func callClosedTarget(effect func(boundary.Target), target boundary.Target) {
	effect(target)
}

// VTAParameterCall gives VTA one complete authored target set through
// an unexported helper parameter.
func VTAParameterCall(target boundary.Target) {
	callClosedTarget(unrelatedEffect, target)
}

var closedGlobalTarget = unrelatedEffect

// VTAGlobalCall invokes an unexported global whose complete authored
// stores select only the unrelated target.
func VTAGlobalCall(target boundary.Target) {
	closedGlobalTarget(target)
}

func closedTargetFactory() func(boundary.Target) {
	return unrelatedEffect
}

// VTAFactoryCall invokes the exact authored result of a closed factory.
func VTAFactoryCall(target boundary.Target) {
	closedTargetFactory()(target)
}

// VTAReadOnlyCaptureCall proves that a local callable remains closed when an
// authored closure captures it without providing any mutation path.
func VTAReadOnlyCaptureCall(target boundary.Target) {
	effect := unrelatedEffect
	call := func() {
		effect(target)
	}
	call()
}
