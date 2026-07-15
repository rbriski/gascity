// Package main models a closed global function routed to signal.Notify.
package main

import (
	"os"
	"os/signal"
)

var notify = signal.Notify

func run() {
	channel := make(chan os.Signal, 1)
	notify(channel, os.Interrupt)
	select {
	case <-channel:
	default:
	}
}

func main() {
	run()
}
