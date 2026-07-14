// Package cycle exercises an incoming-call cycle with no concrete channel
// origin outside the cycle.
package cycle

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

// RecursiveSelector exposes the otherwise caller-less function value without
// adding a concrete incoming call edge.
var RecursiveSelector = selectRecursive

func selectRecursive(channel <-chan Target, recurse bool) {
	select {
	case <-channel:
	default:
	}
	if recurse {
		selectRecursive(channel, false)
	}
}
