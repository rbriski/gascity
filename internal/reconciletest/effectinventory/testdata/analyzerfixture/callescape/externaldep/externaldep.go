// Package externaldep is deliberately loaded only as a dependency of the
// authored fixture. Its SSA bodies exist, but they are outside sourceFuncs.
package externaldep

import (
	"os"
	"path/filepath"
	"unsafe"
)

// Accept receives one boxed value.
func Accept(any) {}

// AcceptInt receives a non-channel scalar without boxing it.
func AcceptInt(int) {}

// AcceptStringChannel receives a string channel without boxing it.
func AcceptStringChannel(chan string) {}

// AcceptUnsafePointer receives an unsafe capability carrier.
func AcceptUnsafePointer(unsafe.Pointer) {}

// AcceptVariadic receives compiler-packed boxed values.
func AcceptVariadic(...any) {}

// AcceptCallbacks receives an existing callback slice through ellipsis.
func AcceptCallbacks(...filepath.WalkFunc) {}

// AcceptChannels receives an existing channel slice through ellipsis.
func AcceptChannels(...chan os.Signal) {}

// AcceptChannelPointer receives a pointer to a channel capability.
func AcceptChannelPointer(*chan os.Signal) {}

// Dropper is an unauthored implementation used in a mixed dispatch set.
type Dropper struct{}

// Drop receives a callback without invoking it.
func (Dropper) Drop(filepath.WalkFunc) {}

// SignalChannel is a named channel with an external receiver method.
type SignalChannel chan os.Signal

// Consume receives through its channel receiver outside the authored source
// universe.
func (channel SignalChannel) Consume() { <-channel }
