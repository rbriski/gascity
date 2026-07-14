// Package routes provides positive, statically resolvable effect routes for
// the effect-inventory analyzer fixture.
package routes

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// SharedRoute is the physical interface-dispatch site reached by multiple
// logical-owner wrappers in the root fixture package.
func SharedRoute(mutator boundary.Mutator, target boundary.Target) {
	mutator.Mutate(target)
}

// OuterRoute adds a wrapper hop before SharedRoute.
func OuterRoute(mutator boundary.Mutator, target boundary.Target) {
	SharedRoute(mutator, target)
}

// InterfaceAliasRoute calls the boundary through its interface alias.
func InterfaceAliasRoute(mutator boundary.MutatorAlias, target boundary.Target) {
	mutator.Mutate(target)
}

// ValueImplementorRoute calls a concrete value implementor directly.
func ValueImplementorRoute(mutator boundary.ValueMutator, target boundary.Target) {
	mutator.Mutate(target)
}

// PointerImplementorRoute calls a concrete pointer implementor directly.
func PointerImplementorRoute(mutator *boundary.PointerMutator, target boundary.Target) {
	mutator.Mutate(target)
}

// ConcreteAliasRoute calls a concrete implementor through its type alias.
func ConcreteAliasRoute(mutator boundary.ValueMutatorAlias, target boundary.Target) {
	mutator.Mutate(target)
}

// PromotedMethodRoute calls the boundary through a promoted embedded method.
func PromotedMethodRoute(mutator boundary.EmbeddedMutator, target boundary.Target) {
	mutator.Mutate(target)
}

// UnrelatedSameNameRoute is a name-only false-positive fixture.
func UnrelatedSameNameRoute(unrelated boundary.Unrelated) {
	unrelated.Mutate(1)
}

// ClosureRoute places an interface-dispatched effect inside a closure.
func ClosureRoute(mutator boundary.Mutator, target boundary.Target) {
	invoke := func() {
		mutator.Mutate(target)
	}
	invoke()
}

// MethodValueRoute invokes an interface boundary through a method value.
func MethodValueRoute(mutator boundary.Mutator, target boundary.Target) {
	invoke := mutator.Mutate
	invoke(target)
}

// FunctionVariableRoute invokes the exact function boundary through a local
// function variable.
func FunctionVariableRoute(target boundary.Target) {
	invoke := boundary.Emit
	invoke(target)
}

// FunctionFieldRoute invokes the exact function boundary through a
// statically initialized function field.
func FunctionFieldRoute(target boundary.Target) {
	holder := boundary.EffectHolder{Effect: boundary.Emit}
	holder.Effect(target)
}

// GoroutineRoute invokes the exact function boundary in a new goroutine.
func GoroutineRoute(target boundary.Target) {
	go boundary.Emit(target)
}

// DeferredRoute invokes the exact function boundary when the route returns.
func DeferredRoute(target boundary.Target) {
	defer boundary.Emit(target)
}

// ChannelSendRoute sends directly to the exact wake channel field.
func ChannelSendRoute(hub *boundary.WakeHub, target boundary.Target) {
	hub.Wake <- target
}

// ChannelReceiveRoute receives directly from the exact wake channel field.
func ChannelReceiveRoute(hub *boundary.WakeHub) boundary.Target {
	return <-hub.Wake
}

// ChannelSelectRoute exercises send and receive cases for the exact wake
// channel field alongside an unrelated cancellation channel.
func ChannelSelectRoute(hub *boundary.WakeHub, target boundary.Target, canceled <-chan struct{}) boundary.Target {
	select {
	case hub.Wake <- target:
		return target
	case received := <-hub.Wake:
		return received
	case <-canceled:
		return ""
	}
}
