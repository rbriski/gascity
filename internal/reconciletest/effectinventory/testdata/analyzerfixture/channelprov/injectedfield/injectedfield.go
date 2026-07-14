// Package injectedfield exercises open-world channel provenance copied into
// a field on a locally allocated struct.
package injectedfield

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

type holder struct {
	channel chan Target
}

// InjectedLocalFieldSend stores a same-typed injected channel in a local
// field. Local allocation does not turn its value into closed-world state.
func InjectedLocalFieldSend(injected chan Target, target Target) {
	local := &holder{channel: injected}
	local.channel <- target
}
