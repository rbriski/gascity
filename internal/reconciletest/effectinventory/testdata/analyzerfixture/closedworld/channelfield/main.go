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

type zeroGate struct {
	entered chan struct{}
}

func newZeroGate() *zeroGate {
	return &zeroGate{}
}

func main() {
	approved := newApprovedGate()
	local := newLocalGate()
	zero := newZeroGate()
	select {
	case <-approved.entered:
	default:
	}
	select {
	case <-local.entered:
	default:
	}
	select {
	case <-zero.entered:
	default:
	}
}
