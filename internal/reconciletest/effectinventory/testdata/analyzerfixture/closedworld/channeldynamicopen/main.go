// Package main models a dynamic channel function read from open storage.
package main

import "time"

type Clock struct {
	After func(time.Duration) <-chan time.Time
}

func (c Clock) afterFunc() func(time.Duration) <-chan time.Time {
	return c.After
}

func openDynamicWait(c Clock) {
	select {
	case <-c.afterFunc()(time.Second):
	default:
	}
}

func main() {
	openDynamicWait(Clock{After: time.After})
}
