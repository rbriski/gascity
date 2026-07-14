// Package explicit contains an exact effect in an explicit init declaration.
package explicit

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

func init() {
	boundary.Emit("explicit-init")
}
