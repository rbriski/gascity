// Package reflectcall exercises reflective invocation of exact effect
// boundaries through CallSlice and a bound Call method value.
package reflectcall

import "reflect"

// Target is the value passed to fixture effects.
type Target string

// Effect is an exact non-variadic effect boundary.
func Effect(Target) {}

// VariadicEffect is an exact variadic effect boundary.
func VariadicEffect(Target, ...Target) {}

// SliceInvoke invokes VariadicEffect through reflect.Value.CallSlice.
func SliceInvoke(target Target) {
	reflect.ValueOf(VariadicEffect).CallSlice([]reflect.Value{
		reflect.ValueOf(target),
		reflect.ValueOf([]Target(nil)),
	})
}

// ReflectBoundCall invokes Effect through a bound reflect.Value.Call method
// value, which must not evade reflective-execution detection.
func ReflectBoundCall(target Target) {
	call := reflect.ValueOf(Effect).Call
	call([]reflect.Value{reflect.ValueOf(target)})
}
