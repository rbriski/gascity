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

// EmbeddedWorker reaches WorkerA.Run through method promotion.
type EmbeddedWorker struct {
	WorkerA
}

// PointerWorker implements Worker through its pointer method set.
type PointerWorker struct{}

// Run implements Worker.
func (*PointerWorker) Run() {}

// Leaf is the shared static route endpoint.
func Leaf() {}

// GenericLeaf exercises instantiated callees whose SSA identity has an origin.
func GenericLeaf[T any]() {}

// OtherLeaf is an unrelated, valid FunctionRef used by wrong-callee tests.
func OtherLeaf() {}

// DuplicateOwner has two distinct exact edges to Leaf.
func DuplicateOwner() {
	Leaf()
	Leaf()
}

// GenericOwner calls one GenericLeaf instantiation.
func GenericOwner() {
	GenericLeaf[int]()
}

// GenericDynamicOwner chooses between two instantiations of one generic origin.
func GenericDynamicOwner(useInt bool) {
	var run func()
	if useInt {
		run = GenericLeaf[int]
	} else {
		run = GenericLeaf[string]
	}
	run()
}

// MixedDispatchOwner has exact and VTA-resolved edges to the same callee.
func MixedDispatchOwner(dynamic bool) {
	var run func()
	if dynamic {
		run = Leaf
	} else {
		run = OtherLeaf
	}
	Leaf()
	run()
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

// GenericInterfaceOwner contains an interface dispatch in a generic body.
func GenericInterfaceOwner[T any](worker Worker) {
	worker.Run()
}

// PromotedOwner invokes WorkerA.Run through an embedded receiver.
func PromotedOwner(worker EmbeddedWorker) {
	worker.Run()
}

// BoundConcreteOwner invokes a bound concrete method value.
func BoundConcreteOwner(worker WorkerA) {
	run := worker.Run
	run()
}

// BoundInterfaceOwner invokes a bound interface method value.
func BoundInterfaceOwner(worker Worker) {
	run := worker.Run
	run()
}

// ConcreteExpressionOwner invokes a concrete method expression.
func ConcreteExpressionOwner(worker WorkerA) {
	run := WorkerA.Run
	run(worker)
}

// InterfaceExpressionOwner invokes an interface method expression.
func InterfaceExpressionOwner(worker Worker) {
	run := Worker.Run
	run(worker)
}

// PointerAdaptOwner invokes a pointer-receiver method on an addressable value.
func PointerAdaptOwner(worker PointerWorker) {
	worker.Run()
}

// BoundPointerOwner invokes a bound pointer-receiver method on a value.
func BoundPointerOwner(worker PointerWorker) {
	run := worker.Run
	run()
}

// SeedInterfaceTargets makes both Worker implementations visible to VTA.
func SeedInterfaceTargets() {
	InterfaceOwner(WorkerA{})
	InterfaceOwner(WorkerB{})
	BoundInterfaceOwner(WorkerA{})
	BoundInterfaceOwner(WorkerB{})
	InterfaceExpressionOwner(WorkerA{})
	InterfaceExpressionOwner(WorkerB{})
	GenericInterfaceOwner[int](WorkerA{})
	GenericInterfaceOwner[string](WorkerB{})
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
