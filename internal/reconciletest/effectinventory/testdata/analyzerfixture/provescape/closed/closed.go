// Package closed exercises channel provenance that remains provably closed
// through lexical capture and static recursive passthrough.
package closed

// Target is the value carried by fixture channels.
type Target string

// Approved is the exact channel object used as the discovery boundary.
var Approved = make(chan Target, 1)

// LexicalClosure captures a local copy of Approved in a lexical closure.
func LexicalClosure(target Target) {
	channel := Approved
	send := func() {
		channel <- target
	}
	send()
}

func recursivePassthrough(depth int) chan Target {
	if depth == 0 {
		return Approved
	}
	return recursivePassthrough(depth - 1)
}

// StaticRecursivePassthrough sends through a static recursive helper whose
// only non-recursive result is Approved.
func StaticRecursivePassthrough(target Target) {
	recursivePassthrough(2) <- target
}
