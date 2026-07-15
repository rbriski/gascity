// Package main models a locally closed dynamic channel-returning function.
package main

import "time"

type clock struct {
	after func(time.Duration) <-chan time.Time
}

func newClock() *clock {
	return &clock{after: time.After}
}

func (c *clock) afterFunc() func(time.Duration) <-chan time.Time {
	if c.after != nil {
		return c.after
	}
	return time.After
}

func closedDynamicWait(c *clock) {
	select {
	case <-c.afterFunc()(time.Second):
	default:
	}
}

func main() {
	closedDynamicWait(newClock())
}
