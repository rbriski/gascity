// Package register exercises a local channel registered through an exact
// channel-producing function parameter.
package register

import (
	"os"
	"os/signal"
)

// NotFunction has callable type but is not an exact function declaration.
var NotFunction = signal.Stop

// WrongChannel has a channel parameter incompatible with signal.Notify.
func WrongChannel(chan string) {}

// Stopper exercises rejection of release methods while the contract only
// admits exact package functions.
type Stopper struct{}

// Stop accepts the same channel type but is intentionally a method.
func (Stopper) Stop(chan<- os.Signal) {}

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
