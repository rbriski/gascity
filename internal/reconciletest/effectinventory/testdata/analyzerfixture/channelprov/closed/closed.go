// Package closed exercises closed-world channel provenance through static
// helpers and locally allocated structs.
package closed

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

type holder struct {
	channel chan Target
}

func passthrough(channel chan Target) chan Target {
	return channel
}

// StaticPassthroughSend sends through a static identity helper. The returned
// value retains the exact Approved provenance.
func StaticPassthroughSend(target Target) {
	passthrough(Approved) <- target
}

// LocalStructCopySend copies Approved into a field on a local allocation
// before sending through it.
func LocalStructCopySend(target Target) {
	local := &holder{channel: Approved}
	local.channel <- target
}

// UnrelatedLocalFieldSend uses a same-typed, locally created channel.
// It is closed-world but unrelated to Approved and must remain excluded.
func UnrelatedLocalFieldSend(target Target) {
	local := &holder{channel: make(chan Target, 1)}
	local.channel <- target
}
