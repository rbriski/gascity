// Package result exercises exact function-result channel provenance.
package result

// Target is the value carried by the fixture channel.
type Target string

// Wake returns the exact package-function result channel inventoried by the
// analyzer fixture.
func Wake() chan Target {
	return make(chan Target)
}

// Receive receives from result slot one of Wake through a local alias.
func Receive() Target {
	wake := Wake()
	return <-wake
}
