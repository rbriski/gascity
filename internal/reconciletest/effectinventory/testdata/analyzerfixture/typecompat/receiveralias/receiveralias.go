// Package receiveralias distinguishes a method's declared receiver from a
// source-level alias for that receiver.
package receiveralias

// Target is the value carried by fixture channels.
type Target string

// Declaring owns the Wake method used as the exact channel boundary.
type Declaring struct{}

// Alias is a source-level alias for Declaring. It must not be accepted as the
// receiver identity of an exact boundary.
type Alias = Declaring

// Wake returns the exact result channel used as the discovery boundary.
func (Declaring) Wake() chan Target {
	return make(chan Target, 1)
}

// CanonicalRoute sends through Wake using its canonical declaring receiver.
func CanonicalRoute(receiver Declaring, target Target) {
	receiver.Wake() <- target
}
