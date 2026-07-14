// Package channelprefilter supplies a call-valued escaped argument whose
// provenance requires the call graph only when channel tracing is warranted.
package channelprefilter

import (
	"unsafe"

	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/callescape/externaldep"
)

func producedValue() int { return 1 }

// EscapeResult hands a non-channel call result to an unauthored dependency.
func EscapeResult() {
	externaldep.AcceptInt(producedValue())
}

// Approved is the exact channel boundary.
var Approved = make(chan string, 1)

func producedChannel() chan string { return Approved }

// EscapeChannelResult hands an exact channel result to an unauthored dependency.
func EscapeChannelResult() {
	externaldep.AcceptStringChannel(producedChannel())
}

// EscapeInjectedChannel hands a compatible open-world channel to the same
// unauthored dependency.
func EscapeInjectedChannel(channel chan string) {
	externaldep.AcceptStringChannel(channel)
}

// EscapeUnsafeChannel hands an unsafe alias of the exact channel to an
// unauthored dependency.
func EscapeUnsafeChannel() {
	externaldep.AcceptUnsafePointer(unsafe.Pointer(&Approved))
}
