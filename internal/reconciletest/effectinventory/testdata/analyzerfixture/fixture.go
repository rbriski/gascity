// Package analyzerfixture defines logical owners whose wrapper chains reach
// the typed effect sites in the fixture's routes package.
package analyzerfixture

import (
	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"
	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/routes"
)

// ControllerOwner reaches SharedRoute through OuterRoute.
func ControllerOwner(mutator boundary.Mutator, target boundary.Target) {
	routes.OuterRoute(mutator, target)
}

// ForegroundOwner reaches the same SharedRoute site without OuterRoute.
func ForegroundOwner(mutator boundary.Mutator, target boundary.Target) {
	routes.SharedRoute(mutator, target)
}

// PlatformOwner reaches the source-selected PlatformRoute site.
func PlatformOwner(mutator boundary.Mutator) {
	routes.PlatformRoute(mutator)
}
