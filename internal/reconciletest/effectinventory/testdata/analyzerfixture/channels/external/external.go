// Package external contains deliberately open-world channel provenance.
package external

// Target is the value carried by fixture channels.
type Target string

// Approved returns the exact result channel used as the discovery boundary.
func Approved() chan Target {
	return make(chan Target)
}

// Source is an externally supplied channel factory.
type Source func() chan Target

// InjectedChannel sends to a channel supplied by an unanalyzed caller. Its
// provenance may reach Approved and therefore cannot be treated as closed.
func InjectedChannel(channel chan Target, target Target) {
	channel <- target
}

// InjectedCallback sends to a channel returned by an unanalyzed callback.
// Even when VTA sees a known source elsewhere, the exported callback remains
// open-world.
func InjectedCallback(source Source, target Target) {
	source() <- target
}
