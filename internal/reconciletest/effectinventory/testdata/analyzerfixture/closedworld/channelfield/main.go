// Package main provides private channel fields initialized by authored constructors.
package main

var Approved = make(chan struct{})

type approvedGate struct {
	entered chan struct{}
}

func newApprovedGate() *approvedGate {
	return &approvedGate{entered: Approved}
}

type localGate struct {
	entered chan struct{}
}

func newLocalGate() *localGate {
	return &localGate{entered: make(chan struct{})}
}

func newPrimedLocalGate() *localGate {
	gate := &localGate{entered: make(chan struct{}, 1)}
	gate.entered <- struct{}{}
	return gate
}

func (g *localGate) receiveWithDeferredRelease() {
	defer func() {
		select {
		case <-g.entered:
		default:
		}
	}()
	select {
	case <-g.entered:
	default:
	}
}

type zeroGate struct {
	entered chan struct{}
}

func newZeroGate() *zeroGate {
	return &zeroGate{}
}

func main() {
	approved := newApprovedGate()
	local := newLocalGate()
	primed := newPrimedLocalGate()
	zero := newZeroGate()
	select {
	case <-approved.entered:
	default:
	}
	select {
	case <-local.entered:
	default:
	}
	local.receiveWithDeferredRelease()
	primed.receiveWithDeferredRelease()
	select {
	case <-zero.entered:
	default:
	}
}
