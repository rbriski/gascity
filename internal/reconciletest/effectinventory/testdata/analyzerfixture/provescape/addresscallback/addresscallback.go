// Package addresscallback exercises channel-bearing addresses passed to
// callbacks supplied by an open-world caller.
package addresscallback

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

type hub struct {
	wake chan Target
}

// LocalChannelAddress lets an unknown callback replace a local channel before
// the channel is used. Its earlier Approved value cannot close its provenance.
func LocalChannelAddress(callback func(*chan Target), target Target) {
	channel := Approved
	callback(&channel)
	channel <- target
}

// StructFieldAddress lets an unknown callback replace a channel through the
// address of a field on an otherwise local struct.
func StructFieldAddress(callback func(*chan Target), target Target) {
	local := &hub{wake: Approved}
	callback(&local.wake)
	local.wake <- target
}
