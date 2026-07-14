//go:build !windows

package main

import (
	"syscall"
	"time"
)

func terminateManagedDoltTestProcessGroup(pid int) (bool, error) {
	pgid, err := syscall.Getpgid(pid)
	if err != nil || pgid != pid || pgid <= 1 {
		return false, nil
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		return false, nil
	}
	deadline := time.Now().Add(managedDoltTestProcessGroupKillWait)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return true, nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	time.Sleep(250 * time.Millisecond)
	return true, nil
}
