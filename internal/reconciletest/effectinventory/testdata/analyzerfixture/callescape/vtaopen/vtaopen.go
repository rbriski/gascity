// Package vtaopen exercises VTA target sets that are not closed proofs.
package vtaopen

import (
	"os"

	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"
)

func authoredTarget(boundary.Target) {}

// OpenParameterCall remains open even after an authored caller seeds a known
// VTA target because external callers may provide a different function.
func OpenParameterCall(effect func(boundary.Target), target boundary.Target) {
	effect(target)
}

// SeedOpenParameter gives VTA a nonempty authored target set without closing
// the exported parameter source.
func SeedOpenParameter(target boundary.Target) {
	OpenParameterCall(authoredTarget, target)
}

// ExportedTarget is externally writable, so its known authored initializer is
// not a closed target-set proof.
var ExportedTarget = authoredTarget

// ExportedGlobalCall invokes an exported function slot that another package
// may replace with an inventoried boundary.
func ExportedGlobalCall(target boundary.Target) {
	ExportedTarget(target)
}

var escapedTarget = authoredTarget

// EscapedGlobalTarget exposes the address of an otherwise-unexported function
// slot, preventing its current VTA target set from being treated as closed.
func EscapedGlobalTarget() *func(boundary.Target) {
	return &escapedTarget
}

// EscapedGlobalCall invokes the address-escaped global slot.
func EscapedGlobalCall(target boundary.Target) {
	escapedTarget(target)
}

// EscapedSetterCaptureCall passes a setter for its captured callable to an
// open callback. The callback may synchronously install an inventoried target
// before the subsequent dynamic call.
func EscapedSetterCaptureCall(expose func(func(func(boundary.Target))), target boundary.Target) {
	effect := authoredTarget
	setter := func(next func(boundary.Target)) {
		effect = next
	}
	expose(setter)
	effect(target)
}

// SeedEscapedSetterCapture gives VTA a nonempty authored target set without
// closing the exported callback parameter.
func SeedEscapedSetterCapture(target boundary.Target) {
	EscapedSetterCaptureCall(func(setter func(func(boundary.Target))) {
		setter(authoredTarget)
	}, target)
}

func authoredStringTarget(string) error { return nil }

// MixedCoveredCall has both an authored target and the uncovered os.Chdir
// target. A nonempty VTA set is insufficient when any target is uncovered.
func MixedCoveredCall(target string, external bool) error {
	effect := authoredStringTarget
	if external {
		effect = os.Chdir
	}
	return effect(target)
}
