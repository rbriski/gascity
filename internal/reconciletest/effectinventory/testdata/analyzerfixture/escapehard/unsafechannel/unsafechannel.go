// Package unsafechannel exercises an exact channel boundary routed through an
// unsafe pointer-to-channel load hidden behind a static helper.
package unsafechannel

import "unsafe"

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target)

func unsafeAlias() chan Target {
	pointer := unsafe.Pointer(&Approved)
	pointer = unsafe.Add(pointer, 0)
	return *(*chan Target)(pointer)
}

// UnsafeDerivedSend sends to Approved after recovering it through an
// unsafe.Pointer/unsafe.Add-derived pointer-to-channel load.
func UnsafeDerivedSend(target Target) {
	unsafeAlias() <- target
}
