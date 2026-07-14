// Package ambiguous exercises a closed dynamic call that may select either
// the inventoried registration boundary or an unrelated compatible function.
package ambiguous

import (
	"os"
	"os/signal"
)

func ignore(chan<- os.Signal, ...os.Signal) {}

// SelectAfterAmbiguousRegistration must not treat a local channel as
// registered when the closed call target set contains a non-boundary target.
func SelectAfterAmbiguousRegistration(useNotify bool) {
	register := ignore
	if useNotify {
		register = signal.Notify
	}
	channel := make(chan os.Signal, 1)
	register(channel, os.Interrupt)
	select {
	case <-channel:
	default:
	}
}
