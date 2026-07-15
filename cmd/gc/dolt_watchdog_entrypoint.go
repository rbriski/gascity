package main

import "io"

// privateManagedDoltWatchdogEntrypoint dispatches the private argv sentinels
// used when gc re-executes itself as a managed Dolt watchdog. It must run
// before Cobra for both the production binary and cmd/gc's test binary.
func privateManagedDoltWatchdogEntrypoint(args []string, stdout, stderr io.Writer) (bool, int) {
	if len(args) == 0 {
		return false, 0
	}
	switch args[0] {
	case managedDoltScopeWatchdogArg:
		return true, runManagedDoltScopeWatchdog(args[1:], stdout, stderr)
	case managedDoltTestWatchdogArg:
		return true, runManagedDoltTestWatchdog(args[1:], stdout, stderr)
	default:
		return false, 0
	}
}
