// Package globalhelper contains an effectful helper reached during package
// variable initialization.
package globalhelper

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// Initialized forces the package-initializer route through Initialize.
var Initialized = Initialize()

// Initialize owns the physical effect reached from package initialization.
func Initialize() boundary.Target {
	boundary.Emit("global-helper")
	return "initialized"
}
