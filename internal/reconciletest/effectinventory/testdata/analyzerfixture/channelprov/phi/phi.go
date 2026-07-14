// Package phi exercises mixed exact and open-world channel provenance at an
// SSA phi feeding a source-level select operation.
package phi

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

// SelectInjected selects between Approved and an injected same-typed
// channel, then uses the merged value in a select send. The exact branch must
// not make the open-world operation safe to inventory.
func SelectInjected(injected chan Target, useInjected bool, target Target) {
	selected := Approved
	if useInjected {
		selected = injected
	}
	select {
	case selected <- target:
	default:
	}
}
