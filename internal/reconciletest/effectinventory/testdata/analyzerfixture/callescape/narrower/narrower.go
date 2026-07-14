// Package narrower exercises a bound method captured from an open-world
// interface that is narrower than the inventoried interface.
package narrower

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// NarrowMutator omits CompleteMutator's Complete method, but an external
// caller may still supply a concrete value that implements CompleteMutator.
type NarrowMutator interface {
	Mutate(boundary.Target)
}

// BoundOpenWorldNarrowerMethod captures a method before invoking it. The
// dynamic receiver may implement the inventoried complete interface even
// though NarrowMutator itself does not.
func BoundOpenWorldNarrowerMethod(mutator NarrowMutator, target boundary.Target) {
	mutate := mutator.Mutate
	mutate(target)
}

// BoundOpenWorldNarrowerPhi merges a bound open-world interface method with
// an unrelated callback before invocation.
func BoundOpenWorldNarrowerPhi(mutator NarrowMutator, alternate func(boundary.Target), choose bool, target boundary.Target) {
	mutate := alternate
	if choose {
		mutate = mutator.Mutate
	}
	mutate(target)
}

type holder struct {
	mutate func(boundary.Target)
}

// BoundOpenWorldNarrowerField stores the bound method in a local field before
// invocation, which must retain the receiver's open-world provenance.
func BoundOpenWorldNarrowerField(mutator NarrowMutator, target boundary.Target) {
	local := &holder{mutate: mutator.Mutate}
	local.mutate(target)
}

// BoundOpenWorldNarrowerClosure captures the bound method in another lexical
// closure before invoking it.
func BoundOpenWorldNarrowerClosure(mutator NarrowMutator, target boundary.Target) {
	mutate := mutator.Mutate
	run := func() {
		mutate(target)
	}
	run()
}
