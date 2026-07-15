// Package main provides a closed-executable analyzer fixture.
package main

var (
	approved   = make(chan struct{})
	registered = make(chan struct{})
	callback   = safeCallback
)

func CallbackEffect() {}

func RegisterInput(chan struct{}) {}

func safeCallback() {}

func invokeCallback(candidate func()) {
	candidate()
}

func receiveParameter(candidate <-chan struct{}) {
	select {
	case <-candidate:
	default:
	}
}

func receiveRegistered() {
	select {
	case <-registered:
	default:
	}
}

func reachableSources() {
	callback = CallbackEffect
	invokeCallback(CallbackEffect)
	receiveParameter(approved)
	RegisterInput(registered)
}

func main() {
	reachableSources()
	callback()
	invokeCallback(safeCallback)
	receiveParameter(make(chan struct{}))
	receiveRegistered()
}
