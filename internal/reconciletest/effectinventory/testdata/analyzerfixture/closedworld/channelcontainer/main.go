// Package main models locally-created channels carried through erased sync.Map
// values and native map lookups before send, receive, and close operations.
package main

import "sync"

var Approved = make(chan struct{})

type semaphore struct {
	permit chan struct{}
}

var semaphores sync.Map

func useSyncMapCarrier() {
	actual, _ := semaphores.LoadOrStore("session", &semaphore{permit: make(chan struct{}, 1)})
	sem := actual.(*semaphore)
	sem.permit <- struct{}{}
	<-sem.permit
}

type flight struct {
	done chan struct{}
}

func useNativeMapCarrier() {
	flights := make(map[string]*flight)
	current := &flight{done: make(chan struct{})}
	flights["run"] = current
	loaded := flights["run"]
	close(loaded.done)
	<-loaded.done
}

type waitGate struct {
	ready <-chan struct{}
}

type waitStart struct {
	ready chan struct{}
}

type fake struct {
	gates  map[string]waitGate
	starts map[string]waitStart
}

func newFake() *fake {
	return &fake{
		gates:  make(map[string]waitGate),
		starts: make(map[string]waitStart),
	}
}

func (f *fake) gate(name string) chan struct{} {
	ready := make(chan struct{})
	f.gates[name] = waitGate{ready: ready}
	return ready
}

func (f *fake) started(name string) <-chan struct{} {
	ready := make(chan struct{})
	f.starts[name] = waitStart{ready: ready}
	return ready
}

func (f *fake) wait(name string) {
	if started, ok := f.starts[name]; ok {
		close(started.ready)
		delete(f.starts, name)
	}
	if gate, ok := f.gates[name]; ok {
		<-gate.ready
	}
}

func useFakeCarriers() {
	f := newFake()
	gate := f.gate("session")
	started := f.started("session")
	close(gate)
	f.wait("session")
	<-started
}

type approvedCarrier struct {
	ready chan struct{}
}

func receiveApproved() {
	carrier := &approvedCarrier{ready: Approved}
	select {
	case <-carrier.ready:
	default:
	}
}

func main() {
	useSyncMapCarrier()
	useNativeMapCarrier()
	useFakeCarriers()
	receiveApproved()
}
