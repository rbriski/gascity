// Package productionscope is an analyzer root whose authored effects live in
// production dependencies rather than in the root package itself.
package productionscope

import (
	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"
	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/profiletags"
	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/routes"
)

// Run reaches profile-selected and platform-selected dependency effects.
func Run(mutator boundary.Mutator) {
	profiletags.ProfileRoute()
	routes.PlatformRoute(mutator)
}
