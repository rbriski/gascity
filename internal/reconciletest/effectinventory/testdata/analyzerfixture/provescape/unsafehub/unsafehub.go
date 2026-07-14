// Package unsafehub exercises exact typed field identity reached through an
// unsafe-derived receiver.
package unsafehub

import "unsafe"

// Target is the value carried by fixture channels.
type Target string

// Hub owns the exact channel field used as the discovery boundary.
type Hub struct {
	Wake chan Target
}

// UnsafeDerivedField sends through the exact Wake field on a Hub reconstructed
// from unsafe.Pointer. Typed field identity does not make that provenance safe.
func UnsafeDerivedField(pointer unsafe.Pointer, target Target) {
	(*Hub)(pointer).Wake <- target
}
