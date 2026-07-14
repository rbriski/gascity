//go:build !windows

package main

import "syscall"

var (
	supervisorGetpgid = syscall.Getpgid
	supervisorGetpgrp = syscall.Getpgrp
	supervisorKill    = syscall.Kill
)
