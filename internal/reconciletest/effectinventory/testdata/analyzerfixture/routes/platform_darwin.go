package routes

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// PlatformRoute is the Darwin-specific interface-dispatched effect site.
func PlatformRoute(mutator boundary.Mutator) {
	mutator.Mutate("darwin")
}
