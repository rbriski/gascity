// Package vehicle is the approved raw-process vehicle analyzer fixture.
package vehicle

import (
	"os"
	"syscall"
)

// Root is the typed vehicle entry point used by the analyzer fixture.
func Root(pid int, process *os.Process, kill func(int, syscall.Signal) error) {
	_ = syscall.Kill(pid, syscall.SIGTERM)
	_ = process.Signal(syscall.SIGTERM)
	_ = process.Kill()
	helper(pid, kill)
}

func helper(pid int, kill func(int, syscall.Signal) error) {
	_ = kill(pid, syscall.SIGKILL)
}

// Probe exercises operations that must not be treated as destructive effects.
func Probe(pid int, process *os.Process, kill func(int, syscall.Signal) error) {
	_ = syscall.Kill(pid, 0)
	_ = process.Signal(syscall.Signal(0))
	_ = kill(pid, 0)
	_ = process.Release()

	lookalike := namedLookalike{}
	_ = lookalike.Kill()
	_ = lookalike.Signal(syscall.SIGKILL)
}

type namedLookalike struct{}

func (namedLookalike) Kill() error { return nil }

func (namedLookalike) Signal(syscall.Signal) error { return nil }
