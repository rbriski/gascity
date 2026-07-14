package routes

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// PlatformRoute is the Windows-specific interface-dispatched effect site.
func PlatformRoute(mutator boundary.Mutator) {
	mutator.Mutate("windows")
}
