// Package targetgate supplies typed target and admission-gate evidence fixtures.
package targetgate

// Item is the boundary value whose ID is selected by valid projections.
type Item struct {
	ID string
}

// Label is a method, not a field projection.
func (Item) Label() string { return "item" }

// Other has a same-named field on a different declared type.
type Other struct {
	ID string
}

// Box proves projections across an instantiated generic field object.
type Box[T any] struct {
	ID T
}

// EmbeddedItem exposes Item.ID only through promotion.
type EmbeddedItem struct {
	Item
}

// Left and Right make Ambiguous.ID unresolvable.
type Left struct {
	ID string
}

// Right contributes the second ambiguous ID field.
type Right struct {
	ID string
}

// Ambiguous has no unique ID selector.
type Ambiguous struct {
	Left
	Right
}

// Payload is the value carried by Wake.
type Payload struct {
	ID string
}

// Mutator is the interface-dispatched effect boundary.
type Mutator interface {
	Apply(*Item) (*Item, error)
}

// OptionalMutator is a valid optional capability interface.
type OptionalMutator interface {
	OptionalMutation()
}

// ConflictingCapability cannot coexist with Mutator because Apply conflicts.
type ConflictingCapability interface {
	Apply(*Item) string
}

// GenericCapability cannot be named as a runtime assertion without type args.
type GenericCapability[T any] interface {
	Generic(T)
}

// OptionalMutatorAlias must not masquerade as the exact declared capability.
type OptionalMutatorAlias = OptionalMutator

// ConcreteCapability is not an interface capability.
type ConcreteCapability struct{}

// Sink supplies an exact method boundary and a concrete gate parameter.
type Sink struct {
	Name string
}

// Apply is an exact receiver/parameter/result boundary.
func (*Sink) Apply(item *Item) (*Item, error) { return item, nil }

// ApplyEmbedded returns a value whose ID is only promoted from Item.
func (*Sink) ApplyEmbedded(item *EmbeddedItem) (*EmbeddedItem, error) { return item, nil }

// ApplyFree is a receiverless boundary used by missing-receiver fixtures.
func ApplyFree(item *Item) (*Item, error) { return item, nil }

// ApplyBox is a boundary over one concrete instantiation of Box.
func ApplyBox(box *Box[string]) (*Box[string], error) { return box, nil }

// Wake is a direct channel boundary.
var Wake = make(chan Payload)

// StringWake has an incompatible channel element type.
var StringWake = make(chan string)

// RegisterWake returns one invocation-local wake source.
func RegisterWake() <-chan struct{} { return make(chan struct{}) }

// ResultWakeRoute owns the exact wake registration boundary crossing.
func ResultWakeRoute() {
	select {
	case <-RegisterWake():
	default:
	}
}

// ConfigID and ConstantID exercise exact non-function source objects.
var ConfigID string

// ConstantID is exact constant provenance.
const ConstantID = "constant-id"

// Lookup provides one exact result slot.
func Lookup() (*Item, error) { return &Item{}, nil }

// NoResults is a function with no result slots.
func NoResults() {}

// GoodPredicate is a receiverless boolean predicate.
func GoodPredicate() bool { return true }

// GenericPredicate has no executable instantiation in an ObjectRef.
func GenericPredicate[T any]() bool { return true }

// EnabledConstant is an untyped boolean predicate value.
const EnabledConstant = true

// BadStringPredicate has a non-boolean result.
func BadStringPredicate() string { return "true" }

// BadPairPredicate has a non-predicate result shape.
func BadPairPredicate() (bool, error) { return true, nil }

// PredicateNeedsArgument cannot be invoked as a closed predicate.
func PredicateNeedsArgument(bool) bool { return true }

// Gate supplies a method predicate and a field predicate.
type Gate struct {
	Enabled bool
}

// Ready is a closed method predicate.
func (*Gate) Ready() bool { return true }

// Origin starts the connected route containing Middle.
func Origin(mutator Mutator, item *Item) {
	Middle(mutator, item)
}

// Middle owns the physical interface boundary crossing.
func Middle(mutator Mutator, item *Item) {
	if GoodPredicate() {
		_, _ = mutator.Apply(item)
	}
}

// Unrelated has a compatible parameter but is outside Origin's route chain.
func Unrelated(mutator Mutator) {
	_ = mutator
}

// ConcreteOrigin starts the exact Sink route.
func ConcreteOrigin(sink *Sink, item *Item) {
	ConcreteMiddle(sink, item)
}

// ConcreteMiddle owns the exact Sink.Apply boundary crossing.
func ConcreteMiddle(sink *Sink, item *Item) {
	_, _ = sink.Apply(item)
}

// EmbeddedMiddle owns the exact Sink.ApplyEmbedded boundary crossing.
func EmbeddedMiddle(sink *Sink, item *EmbeddedItem) {
	_, _ = sink.ApplyEmbedded(item)
}

// FreeMiddle owns the receiverless ApplyFree boundary crossing.
func FreeMiddle(item *Item) {
	_, _ = ApplyFree(item)
}

// BoxMiddle owns the instantiated generic ApplyBox boundary crossing.
func BoxMiddle(box *Box[string]) {
	_, _ = ApplyBox(box)
}

// ChannelRoute owns the Wake boundary crossing.
func ChannelRoute(payload Payload) {
	Wake <- payload
}
