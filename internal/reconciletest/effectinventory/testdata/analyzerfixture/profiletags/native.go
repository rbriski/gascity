//go:build gascity_native_beads

// Package profiletags exercises the canonical default/native profile tags.
package profiletags

// Affect is the exact profile-selected boundary.
func Affect() {}

// ProfileRoute invokes the native implementation.
func ProfileRoute() { Affect() }
