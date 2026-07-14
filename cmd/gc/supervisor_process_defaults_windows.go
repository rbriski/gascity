//go:build windows

package main

import (
	"fmt"
	"syscall"

	"github.com/gastownhall/gascity/internal/processgroup"
)

var (
	supervisorGetpgid = func(pid int) (int, error) {
		return 0, fmt.Errorf("%w: cannot resolve process group for pid %d", processgroup.ErrProcessGroupsUnsupported, pid)
	}
	supervisorGetpgrp = func() int { return 0 }
	supervisorKill    = func(target int, _ syscall.Signal) error {
		return fmt.Errorf("%w: cannot signal process-group target %d", processgroup.ErrProcessGroupsUnsupported, target)
	}
)
