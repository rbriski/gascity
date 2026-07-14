// Package closeop exercises an unsupported operation on an exact channel.
package closeop

// Target is the value carried by the fixture channel.
type Target string

// Hub owns the exact inventoried wake channel.
type Hub struct {
	Wake chan Target
}

// CloseWake closes the exact inventoried channel. Close is intentionally not
// an OperationKind, so discovery must reject it explicitly instead of silently
// dropping the operation.
func CloseWake(hub *Hub) {
	close(hub.Wake)
}
