// Package main models an externally replaceable signal registration function.
package main

import (
	"os"
	"os/signal"
)

var Notify = signal.Notify

func run() {
	channel := make(chan os.Signal, 1)
	Notify(channel, os.Interrupt)
	select {
	case <-channel:
	default:
	}
}

func main() {
	run()
}
