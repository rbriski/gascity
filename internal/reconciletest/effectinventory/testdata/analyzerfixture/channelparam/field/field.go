// Package field exercises exact channel provenance through a statically
// resolved helper parameter.
package field

// Target is the value carried by fixture channels.
type Target string

// Hub owns the exact channel field used as the discovery boundary.
type Hub struct {
	Wake chan Target
}

func selectWake(channel <-chan Target) {
	select {
	case <-channel:
	default:
	}
}

func selectUnrelated(channel <-chan Target) {
	select {
	case <-channel:
	default:
	}
}

// SelectExact passes the inventoried field through the helper parameter.
func SelectExact(hub *Hub) {
	selectWake(hub.Wake)
}

// SelectLocal passes an unrelated, same-typed local channel through a
// different helper. Type compatibility alone must not classify its select.
func SelectLocal() {
	selectUnrelated(make(chan Target))
}
