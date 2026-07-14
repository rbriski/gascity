// Package structstore exercises a locally initialized channel-bearing struct
// whose address is published into memory owned by an open-world caller.
package structstore

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

// Hub holds a channel whose provenance becomes externally mutable when the
// whole Hub escapes.
type Hub struct {
	Wake chan Target
}

// WholeStructExternalStore publishes the whole local Hub through an injected
// pointer before using its channel field.
func WholeStructExternalStore(external **Hub, target Target) {
	local := &Hub{Wake: Approved}
	*external = local
	local.Wake <- target
}
