// Package field exercises exact channel-field provenance.
package field

// Target is the value carried by fixture channels.
type Target string

// Hub owns one inventoried channel and one same-typed negative control.
type Hub struct {
	Wake      chan Target
	Unrelated chan Target
}

// AliasSend sends through a local alias of the exact Wake field.
func AliasSend(hub *Hub, target Target) {
	wake := hub.Wake
	wake <- target
}

// UnrelatedSend sends through a locally populated same-typed field with a
// different object identity and must not be attributed to Wake.
func UnrelatedSend(target Target) {
	hub := &Hub{Unrelated: make(chan Target, 1)}
	unrelated := hub.Unrelated
	unrelated <- target
}

// RangeReceive receives repeatedly through a local alias of the exact Wake
// field. The implicit receive in a range statement remains an inventoried
// receive operation.
func RangeReceive(hub *Hub) {
	wake := hub.Wake
	for target := range wake {
		_ = target
	}
}
