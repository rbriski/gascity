// Package multivehicle models a raw leaf intentionally shared by two approved
// typed vehicle roots.
package multivehicle

import (
	"os"
	"syscall"
)

var _ *os.Process

// RootA is one approved typed vehicle.
func RootA(pid int, kill func(int, syscall.Signal) error) {
	helper(pid, kill)
}

// RootB is a second approved typed vehicle.
func RootB(pid int, kill func(int, syscall.Signal) error) {
	helper(pid, kill)
}

func helper(pid int, kill func(int, syscall.Signal) error) {
	_ = kill(pid, syscall.SIGKILL)
}
