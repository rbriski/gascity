// Package bypass contains raw-process analyzer bypass fixtures.
package bypass

import (
	"os"
	"syscall"
)

// Direct bypasses every approved process vehicle.
func Direct(pid int, process *os.Process) {
	_ = syscall.Kill(pid, syscall.SIGKILL)
	_ = process.Signal(syscall.SIGTERM)
	_ = process.Kill()
}

// Injected invokes the raw syscall through a caller-supplied function value.
func Injected(pid int, kill func(int, syscall.Signal) error) {
	_ = kill(pid, syscall.SIGKILL)
}

// SupplyRaw makes the injected raw syscall provenance closed and explicit.
func SupplyRaw(pid int) {
	Injected(pid, syscall.Kill)
}
