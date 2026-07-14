// Package convertible exercises channel compatibility across distinct named
// channel types and generic type parameters.
package convertible

// Target is the value carried by fixture channels.
type Target string

// ApprovedChannel is the named type of the exact inventoried boundary.
type ApprovedChannel chan Target

// InjectedChannel is distinct from, but convertible to, ApprovedChannel.
type InjectedChannel chan Target

// Approved is the exact named-channel object used as the discovery boundary.
var Approved ApprovedChannel = make(chan Target, 1)

// GenericConvertibleSend sends through a caller-selected channel type whose
// constraint admits the named ApprovedChannel type.
func GenericConvertibleSend[C ~chan Target](channel C, target Target) {
	channel <- target
}

// NamedConvertibleSend sends through a distinct named channel supplied by an
// unanalyzed caller. Convertibility keeps its provenance relevant to Approved.
func NamedConvertibleSend(channel InjectedChannel, target Target) {
	channel <- target
}
