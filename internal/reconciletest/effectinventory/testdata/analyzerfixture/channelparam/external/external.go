// Package external exercises a channel parameter with no authored caller.
package external

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

// SelectExternal can be called with any compatible channel outside the
// analyzed source universe, so its select must remain a hard error.
func SelectExternal(channel <-chan Target) {
	select {
	case <-channel:
	default:
	}
}
