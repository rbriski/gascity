// Package complete isolates complete-interface dispatch fixtures from the
// single-method interface routes.
package complete

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// PromotedRoute calls the complete-interface boundary through an outer type
// that promotes the target method from a partial type.
func PromotedRoute(mutator boundary.PromotedCompleteMutator, target boundary.Target) {
	mutator.Mutate(target)
}

// PromotedMethodValueRoute binds the promoted method through the complete
// outer type before invoking it.
func PromotedMethodValueRoute(mutator boundary.PromotedCompleteMutator, target boundary.Target) {
	invoke := mutator.Mutate
	invoke(target)
}

// PartialRoute calls the declaration through a receiver that does not
// implement the complete interface.
func PartialRoute(mutator boundary.PartialCompleteMutator, target boundary.Target) {
	mutator.Mutate(target)
}

// SameSignatureRoute is an unrelated same-signature negative control.
func SameSignatureRoute(mutator boundary.SameSignatureMutator, target boundary.Target) {
	mutator.Mutate(target)
}
