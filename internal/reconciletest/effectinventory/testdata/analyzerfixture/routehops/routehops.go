// Package routehops supplies authored call chains for route-hop evidence tests.
package routehops

// Worker is an interface-dispatched route target.
type Worker interface {
	Run()
}

// WorkerA is one closed-world Worker implementation.
type WorkerA struct{}

// Run implements Worker.
func (WorkerA) Run() {}

// WorkerB is a second closed-world Worker implementation.
type WorkerB struct{}

// Run implements Worker.
func (WorkerB) Run() {}

// Leaf is the shared static route endpoint.
func Leaf() {}

// OtherLeaf is an unrelated, valid FunctionRef used by wrong-callee tests.
func OtherLeaf() {}

// DuplicateOwner has two distinct exact edges to Leaf.
func DuplicateOwner() {
	Leaf()
	Leaf()
}

// ClosureOwner reaches Leaf through its first lexical closure.
func ClosureOwner() {
	run := func() {
		Leaf()
	}
	run()
}

// InterfaceOwner invokes the selected Worker implementation dynamically.
func InterfaceOwner(worker Worker) {
	worker.Run()
}

// SeedInterfaceTargets makes both Worker implementations visible to VTA.
func SeedInterfaceTargets() {
	InterfaceOwner(WorkerA{})
	InterfaceOwner(WorkerB{})
}

// GoOwner starts Leaf in a goroutine.
func GoOwner() {
	go Leaf()
}

// DeferOwner defers Leaf.
func DeferOwner() {
	defer Leaf()
}

// ChainOwner reaches Leaf through ChainMiddle.
func ChainOwner() {
	ChainMiddle()
}

// ChainMiddle is the middle of the valid two-hop chain.
func ChainMiddle() {
	Leaf()
}

// OtherOwner has a valid edge to Leaf but is not reached by ChainOwner.
func OtherOwner() {
	Leaf()
}

// PlatformOwner reaches the source-selected PlatformLeaf implementation.
func PlatformOwner() {
	PlatformLeaf()
}
