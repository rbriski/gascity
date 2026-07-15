// Package main models a same-signature bodyless sink that must not be
// mistaken for the exact signal.Stop release operation.
package main

import (
	"os"
	"os/signal"
)

//go:noescape
func unrelatedStop(chan<- os.Signal)

var dynamicStop = signal.Stop

func run() {
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt)
	defer unrelatedStop(channel)
	select {
	case <-channel:
	default:
	}
}

func runDynamic() {
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt)
	defer dynamicStop(channel)
	select {
	case <-channel:
	default:
	}
}

func main() {
	run()
	runDynamic()
}
