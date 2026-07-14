//go:build windows

package workspacesvc

import (
	"log"
	"syscall"

	"github.com/gastownhall/gascity/internal/pidutil"
)

// signalProcessOrGroup signals only pid on Windows. Gas City does not claim
// process-tree cleanup without job-object containment.
func signalProcessOrGroup(pid int, sig syscall.Signal) {
	if unsafeSignalTarget(pid) {
		log.Printf("workspacesvc: refusing to signal unsafe orphan-reap target pid %d", pid)
		return
	}
	_ = pidutil.Signal(pid, sig)
}

func unsafeSignalTarget(pid int) bool {
	return pid <= 1
}
