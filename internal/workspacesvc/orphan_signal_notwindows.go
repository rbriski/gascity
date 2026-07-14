//go:build !windows

package workspacesvc

import (
	"log"
	"syscall"
)

// signalProcessOrGroup signals the process group led by pid, falling back
// to the process itself when pid is not a group leader. Unsafe targets are
// refused outright.
func signalProcessOrGroup(pid int, sig syscall.Signal) {
	if unsafeSignalTarget(pid) {
		log.Printf("workspacesvc: refusing to signal unsafe orphan-reap target pid %d", pid)
		return
	}
	if err := syscall.Kill(-pid, sig); err == nil {
		return
	}
	_ = syscall.Kill(pid, sig)
}

// unsafeSignalTarget reports whether pid must never be signaled: init,
// nonpositive pids (kill(2) broadcast and current-group semantics), and the
// sweeper's own process group.
func unsafeSignalTarget(pid int) bool {
	return pid <= 1 || pid == syscall.Getpgrp()
}
