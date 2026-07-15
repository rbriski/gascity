// Package main models a release call where the same channel value occupies
// both the declared release slot and an unrelated unanalyzed slot.
package main

//go:noescape
func Register(chan int)

//go:noescape
func Stop(chan int, chan int)

func run() {
	channel := make(chan int)
	Register(channel)
	Stop(channel, channel)
	select {
	case <-channel:
	default:
	}
}

func main() {
	run()
}
