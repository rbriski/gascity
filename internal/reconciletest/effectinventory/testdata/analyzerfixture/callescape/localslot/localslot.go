// Package localslot exercises a locally initialized function slot whose
// address escapes to an open-world callback before invocation.
package localslot

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// AddressEscapedLocalFunctionSlot starts with a closed target, then lets an
// external callback replace the slot with any compatible function.
func AddressEscapedLocalFunctionSlot(rewrite func(*func(boundary.Target)), target boundary.Target) {
	effect := boundary.Emit
	rewrite(&effect)
	effect(target)
}
