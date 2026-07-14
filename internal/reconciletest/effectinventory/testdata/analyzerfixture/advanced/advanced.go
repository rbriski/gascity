// Package advanced provides lexical-identity and ordering fixtures for the
// effect-inventory analyzer.
package advanced

import "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary"

// DirectEmitTwice contains two physical sites for the same boundary and
// operation kind in one lexical function.
func DirectEmitTwice(target boundary.Target) {
	boundary.Emit(target)
	boundary.Emit(target)
}

// NestedClosure keeps its physical site in a distinct lexical function.
func NestedClosure(target boundary.Target) {
	emit := func() {
		boundary.Emit(target)
	}
	emit()
}

// SourceWrapper owns one physical Emit site.
func SourceWrapper(target boundary.Target) {
	boundary.Emit(target)
}

// CallsSourceWrapper exercises an ordinary source wrapper edge. Its call is
// not itself a physical Emit site.
func CallsSourceWrapper(target boundary.Target) {
	SourceWrapper(target)
}

// GenericEmit owns one lexical Emit site regardless of its instantiations.
func GenericEmit[T any](target boundary.Target, _ T) {
	boundary.Emit(target)
}

// InstantiateGeneric forces two concrete SSA instantiations of GenericEmit.
func InstantiateGeneric(target boundary.Target) {
	GenericEmit(target, 1)
	GenericEmit(target, "two")
}
