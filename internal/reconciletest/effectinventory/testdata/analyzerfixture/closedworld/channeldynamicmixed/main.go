// Package main models a closed target set with mixed channel provenance.
package main

import "time"

type Source struct {
	Channel <-chan time.Time
}

func mixedDynamicWait(source Source, alternate bool) {
	after := time.After
	if alternate {
		after = func(time.Duration) <-chan time.Time {
			return source.Channel
		}
	}
	select {
	case <-after(time.Second):
	default:
	}
}

func main() {
	mixedDynamicWait(Source{Channel: make(chan time.Time)}, false)
}
