package main

import (
	"errors"
	"sync"
)

var errNudgeKeyChildExited = errors.New("nudge keyed reconciler child exited")

// nudgeKeyChildLiveness linearizes keyed-child exit against city readiness
// publication. If publication wins, the exit is delivered to the ready
// CityRuntime; if exit wins, publication is refused.
type nudgeKeyChildLiveness struct {
	mu      sync.Mutex
	exited  bool
	failure chan error
	done    chan struct{}
}

func newNudgeKeyChildLiveness() *nudgeKeyChildLiveness {
	return &nudgeKeyChildLiveness{
		failure: make(chan error, 1),
		done:    make(chan struct{}),
	}
}

func (l *nudgeKeyChildLiveness) finish(err error) {
	if err == nil {
		err = errNudgeKeyChildExited
	}
	l.mu.Lock()
	l.exited = true
	l.mu.Unlock()
	l.failure <- err
	close(l.failure)
	close(l.done)
}

func (l *nudgeKeyChildLiveness) publishReady(stillReady func() bool, publish func()) bool {
	if l == nil || stillReady == nil || publish == nil {
		return false
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.exited || !stillReady() {
		return false
	}
	publish()
	return true
}
