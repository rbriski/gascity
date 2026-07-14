// Package generic exercises open-world generic channel operations against an
// exact channel boundary with the same core type.
package generic

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

// Send sends through a caller-supplied channel whose constraint admits
// Approved. Exported generic parameters remain open-world.
func Send[C ~chan Target](channel C, target Target) {
	channel <- target
}

// Receive receives through a caller-supplied channel whose constraint
// admits Approved. Exported generic parameters remain open-world.
func Receive[C ~chan Target](channel C) Target {
	return <-channel
}

// Close closes a caller-supplied channel whose constraint admits
// Approved. Exported generic parameters remain open-world.
func Close[C ~chan Target](channel C) {
	close(channel)
}
