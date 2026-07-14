// Package externalarg exercises inventoried values handed to dependencies
// whose function bodies are outside the authored SSA universe.
package externalarg

import (
	"io/fs"
	"os"
	"os/signal"
	"path/filepath"
	"slices"

	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/callescape/externaldep"
)

// Effect is the exact callback boundary.
func Effect(_ string, _ fs.FileInfo, err error) error { return err }

// Route gives Effect to filepath.Walk, which invokes it outside authored SSA.
func Route(root string) error {
	return filepath.Walk(root, Effect)
}

// RouteConverted gives an explicitly converted exact boundary to an
// unanalyzed dependency.
func RouteConverted(root string) error {
	effect := filepath.WalkFunc(Effect)
	return filepath.Walk(root, effect)
}

// RoutePhi gives either the exact boundary or a closed local callback to an
// unanalyzed dependency.
func RoutePhi(root string, useBoundary bool) error {
	effect := filepath.WalkFunc(func(_ string, _ fs.FileInfo, err error) error { return err })
	if useBoundary {
		effect = Effect
	}
	return filepath.Walk(root, effect)
}

type callbackHolder struct {
	effect filepath.WalkFunc
}

// RouteField loads the exact boundary from a local field before handing it to
// an unanalyzed dependency.
func RouteField(root string) error {
	holder := callbackHolder{effect: Effect}
	return filepath.Walk(root, holder.effect)
}

// RouteInjected hands an open-world compatible callback to an unanalyzed
// dependency. An external caller can supply Effect.
func RouteInjected(root string, effect filepath.WalkFunc) error {
	return filepath.Walk(root, effect)
}

// RouteAlloc loads the exact boundary through an address-taken local slot.
func RouteAlloc(root string) error {
	effect := filepath.WalkFunc(Effect)
	pointer := &effect
	return filepath.Walk(root, *pointer)
}

// RouteFreeVar loads the exact boundary through a lexical free-variable
// binding inside an authored closure.
func RouteFreeVar(root string) error {
	effect := filepath.WalkFunc(Effect)
	route := func() error {
		return filepath.Walk(root, effect)
	}
	return route()
}

// RouteIndex reads the exact boundary from a local slice before handing it to
// an unauthored dependency.
func RouteIndex(root string) error {
	effects := []filepath.WalkFunc{Effect}
	return filepath.Walk(root, effects[0])
}

// RouteLookup reads the exact boundary from a local map before handing it to
// an unauthored dependency.
func RouteLookup(root string) error {
	effects := map[string]filepath.WalkFunc{"effect": Effect}
	return filepath.Walk(root, effects["effect"])
}

// RouteOpenResult hands a callable returned by an open-world multi-result
// factory to an unauthored dependency. The result could be Effect.
func RouteOpenResult(root string, factory func() (filepath.WalkFunc, error)) error {
	effect, err := factory()
	if err != nil {
		return err
	}
	return filepath.Walk(root, effect)
}

// Walker owns an exact method boundary compatible with filepath.WalkFunc when
// bound to a receiver.
type Walker struct{}

// Visit is the exact bound-method callback boundary.
func (Walker) Visit(_ string, _ fs.FileInfo, err error) error { return err }

// RouteBound gives an exact bound method to an unanalyzed dependency.
func RouteBound(root string, walker Walker) error {
	return filepath.Walk(root, walker.Visit)
}

// OtherWalker deliberately has the same method name and signature as Walker
// without owning the exact boundary object.
type OtherWalker struct{}

// Visit is an unrelated negative control.
func (OtherWalker) Visit(_ string, _ fs.FileInfo, err error) error { return err }

// RouteUnrelatedBound proves same-name/same-signature bound methods do not
// alias an exact boundary.
func RouteUnrelatedBound(root string, walker OtherWalker) error {
	return filepath.Walk(root, walker.Visit)
}

// Entry owns an exact method boundary whose method expression is compatible
// with slices.SortFunc.
type Entry int

// Compare is the exact method-expression callback boundary.
func (entry Entry) Compare(other Entry) int { return int(entry - other) }

// SortEntries gives an exact method expression to an unanalyzed generic
// dependency.
func SortEntries(entries []Entry) {
	slices.SortFunc(entries, Entry.Compare)
}

// OtherEntry deliberately has the same method-expression shape as Entry
// without owning the exact boundary object.
type OtherEntry int

// Compare is an unrelated negative control.
func (entry OtherEntry) Compare(other OtherEntry) int { return int(entry - other) }

// SortOtherEntries proves same-name/same-signature method expressions do not
// alias an exact boundary.
func SortOtherEntries(entries []OtherEntry) {
	slices.SortFunc(entries, OtherEntry.Compare)
}

// Approved is the exact channel boundary.
var Approved = make(chan os.Signal, 1)

// ReceiverApproved is an exact channel boundary used as an external bound
// method receiver.
var ReceiverApproved = make(externaldep.SignalChannel, 1)

// Notify gives Approved to os/signal, which sends outside authored SSA.
func Notify() {
	signal.Notify(Approved, os.Interrupt)
}

// NotifyInjected gives an open-world compatible channel to an unanalyzed
// dependency. An external caller can supply Approved.
func NotifyInjected(channel chan os.Signal) {
	signal.Notify(channel, os.Interrupt)
}

// NotifyPhi gives either the exact boundary or an injected channel to an
// unanalyzed dependency.
func NotifyPhi(injected chan os.Signal, useBoundary bool) {
	channel := injected
	if useBoundary {
		channel = Approved
	}
	signal.Notify(channel, os.Interrupt)
}

type channelHolder struct {
	channel chan os.Signal
}

// NotifyField loads the exact boundary from a local field before handing it
// to an unanalyzed dependency.
func NotifyField() {
	holder := channelHolder{channel: Approved}
	signal.Notify(holder.channel, os.Interrupt)
}

// PointerChannel hands the address of the exact channel boundary to an
// unauthored dependency.
func PointerChannel() {
	externaldep.AcceptChannelPointer(&Approved)
}

// LocalPointerChannel copies the exact boundary into an address-taken local
// slot before handing its pointer to an unauthored dependency.
func LocalPointerChannel() {
	channel := Approved
	externaldep.AcceptChannelPointer(&channel)
}

// BoundChannelReceiver invokes an unauthored method bound to the exact channel
// boundary. The capability lives in the bound receiver rather than Args.
func BoundChannelReceiver() {
	consume := ReceiverApproved.Consume
	consume()
}

func dropCallback(filepath.WalkFunc) {}

func dropChannel(chan os.Signal) {}

type callbackDropper interface {
	Drop(filepath.WalkFunc)
}

type localCallbackDropper struct{}

func (localCallbackDropper) Drop(filepath.WalkFunc) {}

type localOnlyDropper interface {
	DropOnly(filepath.WalkFunc)
}

type localOnlyDropperImpl struct{}

func (localOnlyDropperImpl) DropOnly(filepath.WalkFunc) {}

//go:noescape
func bodylessDrop(filepath.WalkFunc)

// AuthoredDrops proves that independently scanned authored callees do not
// trigger the external-escape rule merely because their arguments are
// boundary values.
func AuthoredDrops() {
	dropCallback(Effect)
	dropChannel(Approved)
}

// ClosedDynamicDrop proves that a closed function value whose authored body
// is scanned does not trigger the external-escape rule.
func ClosedDynamicDrop() {
	drop := func(filepath.WalkFunc) {}
	drop(Effect)
}

// ClosedInterfaceDrop proves that a closed interface dispatch whose only
// target has an authored body remains inside the scanned universe.
func ClosedInterfaceDrop() {
	var dropper callbackDropper = localCallbackDropper{}
	dropper.Drop(Effect)
}

// OpenInterfaceDrop can dispatch to an implementation outside the authored
// SSA universe and must therefore reject its boundary argument.
func OpenInterfaceDrop(dropper callbackDropper) {
	dropper.Drop(Effect)
}

// OpenBoundSink invokes a bound interface method whose receiver remains
// open-world.
func OpenBoundSink(dropper callbackDropper) {
	drop := dropper.Drop
	drop(Effect)
}

// OpenBoundBoxedSink routes the same open bound method through an interface
// box before invoking it with the exact boundary.
func OpenBoundBoxedSink(dropper callbackDropper) {
	boxed := any(dropper.Drop)
	drop := boxed.(func(filepath.WalkFunc))
	drop(Effect)
}

// OpenBoundOnlyBoxedSink isolates an open bound method whose currently known
// VTA target set contains only an authored implementation.
func OpenBoundOnlyBoxedSink(dropper localOnlyDropper, chooseLocal bool) {
	if chooseLocal {
		dropper = localOnlyDropperImpl{}
	}
	boxed := any(dropper.DropOnly)
	drop := boxed.(func(filepath.WalkFunc))
	drop(Effect)
}

// OpenBoundOnlyFreeVarSink routes the same open bound method through a lexical
// free variable before invoking it.
func OpenBoundOnlyFreeVarSink(dropper localOnlyDropper) {
	drop := dropper.DropOnly
	run := func() {
		drop(Effect)
	}
	run()
}

type dropHolder struct {
	drop func(filepath.WalkFunc)
}

// OpenBoundOnlyFieldSink routes the same open bound method through a local
// field before invoking it.
func OpenBoundOnlyFieldSink(dropper localOnlyDropper) {
	holder := dropHolder{drop: dropper.DropOnly}
	holder.drop(Effect)
}

// OpenThunkSink invokes an interface method expression whose receiver remains
// open-world even though the thunk itself is a static SSA callee.
func OpenThunkSink(dropper callbackDropper) {
	callbackDropper.Drop(dropper, Effect)
}

// MixedInterfaceDrop has a closed receiver value but a target set containing
// one authored and one unauthored implementation.
func MixedInterfaceDrop(useExternal bool) {
	var dropper callbackDropper = localCallbackDropper{}
	if useExternal {
		dropper = externaldep.Dropper{}
	}
	dropper.Drop(Effect)
}

// OpenFunctionDrop can invoke an externally supplied function and must
// therefore reject its boundary argument.
func OpenFunctionDrop(drop func(filepath.WalkFunc)) {
	drop(Effect)
}

// BodylessDrop calls a source-declared function with no authored SSA body.
// Such assembly-style declarations are not independently scanned callees.
func BodylessDrop() {
	bodylessDrop(Effect)
}

func authoredGenericDrop[T any](T, filepath.WalkFunc) {}

// AuthoredGenericDrop proves instantiated authored functions normalize to a
// source origin with a real body.
func AuthoredGenericDrop() {
	authoredGenericDrop(0, Effect)
}

// AuthoredBoundDrop proves a bound-method wrapper with a closed receiver and
// an authored terminal body remains inside the scanned universe.
func AuthoredBoundDrop() {
	drop := localCallbackDropper{}.Drop
	drop(Effect)
}

type twoArgLocalDropper struct{}

func (twoArgLocalDropper) Drop(any, filepath.WalkFunc) {}

// AuthoredBoundWithInterfaceArg proves the first source argument of a bound
// method wrapper is not mistaken for its already-bound receiver.
func AuthoredBoundWithInterfaceArg(value any) {
	drop := twoArgLocalDropper{}.Drop
	drop(value, Effect)
}

// BoxedCallback hands an exact callback boundary to an unauthored dependency
// after boxing it in an interface value.
func BoxedCallback() {
	externaldep.Accept(Effect)
}

// BoxedChannel hands an exact channel boundary to an unauthored dependency
// after boxing it in an interface value.
func BoxedChannel() {
	externaldep.Accept(Approved)
}

func boxedChannelResult() any { return Approved }

// BoxedChannelResult hands an exact channel through an authored erased result.
func BoxedChannelResult() {
	externaldep.Accept(boxedChannelResult())
}

// VariadicCallback hands an exact callback boundary to an unauthored
// dependency through the compiler's variadic argument pack.
func VariadicCallback() {
	externaldep.AcceptVariadic(Effect)
}

// VariadicChannel hands an exact channel boundary to an unauthored dependency
// through the compiler's variadic argument pack.
func VariadicChannel() {
	externaldep.AcceptVariadic(Approved)
}

// EllipsisBoxedCallback hands an exact boxed callback through a prebuilt
// variadic slice.
func EllipsisBoxedCallback() {
	values := []any{Effect}
	externaldep.AcceptVariadic(values...)
}

// EllipsisBoxedChannel hands an exact boxed channel through a prebuilt
// variadic slice.
func EllipsisBoxedChannel() {
	values := []any{Approved}
	externaldep.AcceptVariadic(values...)
}

// EllipsisCallback hands an exact callback through a typed prebuilt slice.
func EllipsisCallback() {
	values := []filepath.WalkFunc{Effect}
	externaldep.AcceptCallbacks(values...)
}

// EllipsisChannel hands an exact channel through a typed prebuilt slice.
func EllipsisChannel() {
	values := []chan os.Signal{Approved}
	externaldep.AcceptChannels(values...)
}

// EllipsisOpenCallback hands an open-world typed callback slice to an
// unauthored variadic callee.
func EllipsisOpenCallback(values []filepath.WalkFunc) {
	externaldep.AcceptCallbacks(values...)
}

// EllipsisOpenChannel hands an open-world typed channel slice to an
// unauthored variadic callee.
func EllipsisOpenChannel(values []chan os.Signal) {
	externaldep.AcceptChannels(values...)
}

// EllipsisOpenBoxed proves a fully erased element type is not source evidence
// of an inventoried callback or channel boundary.
func EllipsisOpenBoxed(values []any) {
	externaldep.AcceptVariadic(values...)
}

// BoxedSliceCallback hands a prebuilt slice containing an exact callback to
// an unauthored non-variadic any parameter.
func BoxedSliceCallback() {
	values := []any{Effect}
	externaldep.Accept(values)
}

// BoxedSliceChannel hands a prebuilt slice containing an exact channel to an
// unauthored non-variadic any parameter.
func BoxedSliceChannel() {
	values := []any{Approved}
	externaldep.Accept(values)
}

// BoxedOpenSlice proves boxing a fully erased slice does not invent boundary
// values absent from its source ancestry.
func BoxedOpenSlice(values []any) {
	externaldep.Accept(values)
}

// NilVariadicValues proves that a typed nil variadic slice is closed-empty.
func NilVariadicValues() {
	externaldep.AcceptVariadic([]any(nil)...)
}

// AsyncValues proves go and defer call instructions use the same handoff
// analysis as ordinary calls.
func AsyncValues() {
	go externaldep.Accept(Effect)
	defer signal.Stop(Approved)
}

// ClosedExternalValues proves that locally created, non-boundary values may
// be passed to unanalyzed dependencies.
func ClosedExternalValues(root string) error {
	channel := make(chan os.Signal, 1)
	signal.Notify(channel, os.Interrupt)
	return filepath.Walk(root, func(_ string, _ fs.FileInfo, err error) error { return err })
}
