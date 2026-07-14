// Package closeparam exercises close on a channel supplied by an open-world
// caller whose type is compatible with an inventoried channel boundary.
package closeparam

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target)

// CloseInjected closes a compatible channel supplied outside the analyzed
// package. The channel could be Approved, so discovery must fail closed.
func CloseInjected(channel chan Target) {
	close(channel)
}
