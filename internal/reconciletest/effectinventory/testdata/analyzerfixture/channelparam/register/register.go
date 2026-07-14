// Package register exercises a local channel registered through an exact
// channel-producing function parameter.
package register

import (
	"os"
	"os/signal"
)

// RegisteredLocal registers a local channel with signal.Notify before using
// the same channel in a select receive.
func RegisteredLocal() {
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt)
	select {
	case <-channel:
	default:
	}
}

// UnregisteredLocal selects an unrelated channel with the same type. It must
// not inherit the signal.Notify boundary from type compatibility.
func UnregisteredLocal() {
	channel := make(chan os.Signal, 1)
	select {
	case <-channel:
	default:
	}
}
