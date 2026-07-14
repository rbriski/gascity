// Package boundary defines typed side-effect and wake boundaries for the
// effect-inventory analyzer fixture.
package boundary

// Target is the value carried into fixture effects and wake channels.
type Target string

// Mutator is the fixture's interface-dispatched side-effect boundary.
type Mutator interface {
	Mutate(Target)
}

// MutatorAlias exercises aliases of the boundary interface.
type MutatorAlias = Mutator

// ValueMutator implements Mutator with a value receiver.
type ValueMutator struct{}

// Mutate implements Mutator.
func (ValueMutator) Mutate(Target) {}

// ValueMutatorAlias exercises aliases of a concrete boundary implementor.
type ValueMutatorAlias = ValueMutator

// PointerMutator implements Mutator only through its pointer method set.
type PointerMutator struct{}

// Mutate implements Mutator.
func (*PointerMutator) Mutate(Target) {}

// EmbeddedMutator implements Mutator through a promoted method.
type EmbeddedMutator struct {
	ValueMutator
}

// CompleteMutator exercises interface matching against the interface's full
// method set, not merely the selected method's signature.
type CompleteMutator interface {
	Mutate(Target)
	Complete()
}

// PartialCompleteMutator owns the target method but does not implement
// CompleteMutator because it lacks Complete.
type PartialCompleteMutator struct{}

// Mutate matches CompleteMutator.Mutate but is not independently an
// implementation of CompleteMutator.
func (PartialCompleteMutator) Mutate(Target) {}

// PromotedCompleteMutator implements CompleteMutator by promoting Mutate and
// declaring the interface's second method itself.
type PromotedCompleteMutator struct {
	PartialCompleteMutator
}

// Complete supplies the second method required by CompleteMutator.
func (PromotedCompleteMutator) Complete() {}

// SameSignatureMutator is an unrelated owner of a method with the target
// signature; it deliberately lacks Complete.
type SameSignatureMutator struct{}

// Mutate has the target signature without implementing CompleteMutator.
func (SameSignatureMutator) Mutate(Target) {}

// Unrelated has a method whose name, but not type identity, resembles the
// Mutator boundary.
type Unrelated struct{}

// Mutate is deliberately not a Mutator method because its parameter differs.
func (Unrelated) Mutate(int) {}

// Emit is the fixture's exact free-function side-effect boundary.
func Emit(Target) {}

// EffectFunc is the typed function form of Emit.
type EffectFunc func(Target)

// EffectHolder stores a side-effect function in a field.
type EffectHolder struct {
	Effect EffectFunc
}

// WakeHub owns the exact channel field used by wake-source fixtures.
type WakeHub struct {
	Wake chan Target
}
