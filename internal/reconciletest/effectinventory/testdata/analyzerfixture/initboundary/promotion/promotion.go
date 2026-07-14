// Package promotion distinguishes a method's declaring receiver from an outer
// type that exposes the method only through embedding.
package promotion

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// Declaring owns the exact method boundary.
type Declaring struct{}

// Affect is the exact method boundary used by the fixture.
func (Declaring) Affect(boundary.Target) {}

// Outer exposes Affect only as a promoted method.
type Outer struct {
	Declaring
}

// DirectRoute invokes Affect through its declaring receiver.
func DirectRoute(receiver Declaring, target boundary.Target) {
	receiver.Affect(target)
}

// PromotedRoute invokes the same declared method through its promoted receiver.
func PromotedRoute(receiver Outer, target boundary.Target) {
	receiver.Affect(target)
}
