// Package callerbypass models a raw leaf shared by an approved vehicle and an
// unapproved same-package caller.
package callerbypass

import (
	"os"
	"syscall"
)

var _ *os.Process

// Root is the approved typed vehicle.
func Root(pid int, kill func(int, syscall.Signal) error) {
	helper(pid, kill)
}

// BypassVehicle reaches the same physical raw leaf without entering Root.
func BypassVehicle(pid int, kill func(int, syscall.Signal) error) {
	helper(pid, kill)
}

func helper(pid int, kill func(int, syscall.Signal) error) {
	_ = kill(pid, syscall.SIGKILL)
}
