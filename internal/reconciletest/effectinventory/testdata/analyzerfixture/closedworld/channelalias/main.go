// Package main proves that private channel-field addresses cannot be aliased silently.
package main

var Approved = make(chan struct{})

type gate struct {
	entered chan struct{}
}

func newGate() *gate {
	return &gate{entered: Approved}
}

func replace(channel *chan struct{}) {
	*channel = make(chan struct{})
}

func main() {
	current := newGate()
	replace(&current.entered)
	select {
	case <-current.entered:
	default:
	}
}
