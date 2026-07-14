// Package multiple contains two explicit init declarations in one file. They
// cannot safely share the same package, file, function-name locator.
package multiple

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

func init() {
	boundary.Emit("first-init")
}

func init() {
	boundary.Emit("second-init")
}
