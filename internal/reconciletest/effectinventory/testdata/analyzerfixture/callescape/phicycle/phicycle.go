// Package phicycle exercises a slice whose SSA provenance contains a cycle.
package phicycle

import (
	"os"

	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/callescape/externaldep"
)

type boxedValues []any

func (boxedValues) boxed() {}

type box interface {
	boxed()
}

// Effect is the exact callback boundary.
func Effect() {}

// PossibleEffect is compatible with the open cyclic slice but not stored in it.
func PossibleEffect() {}

// Approved is the exact channel boundary.
var Approved = make(chan os.Signal, 1)

// Ready is a second exact channel boundary with an incompatible carrier type.
var Ready = make(chan struct{}, 1)

// EscapedValues hands the stored boundary values to an unauthored dependency
// through a loop Phi whose back edge converts between named and unnamed slices.
func EscapedValues(again bool) {
	values := boxedValues{Effect, Approved, Ready}
	for again {
		values = boxedValues([]any(values))
		again = false
	}
	externaldep.AcceptVariadic([]any(values)...)
}

// EscapedBoxedValues puts the cyclic slice behind an interface Phi whose back
// edge precedes its preheader edge in SSA predecessor order.
func EscapedBoxedValues(again bool) {
	values := box(boxedValues{Effect, Approved, Ready})
	goto preheader

loop:
	values = any(values).(box)
entry:
	if again {
		again = false
		goto loop
	}
	externaldep.Accept(values)
	return

preheader:
	goto entry
}
