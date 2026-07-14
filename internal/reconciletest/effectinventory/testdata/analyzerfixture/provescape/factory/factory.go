// Package factory exercises an open-world channel factory for which VTA also
// sees a known in-program target.
package factory

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

// Factory returns a channel selected by its caller.
type Factory func() chan Target

// KnownFactory is an in-program VTA candidate that returns Approved.
func KnownFactory() chan Target {
	return Approved
}

// DynamicFactory sends through an injected factory. KnownFactory being a VTA
// candidate does not prove callers cannot inject another compatible factory.
func DynamicFactory(factory Factory, target Target) {
	factory() <- target
}

// SeedKnownCandidate makes KnownFactory visible as a dynamic call target.
func SeedKnownCandidate(target Target) {
	DynamicFactory(KnownFactory, target)
}
