package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestMainExitCodeDispatchesPrivateManagedDoltWatchdogsBeforeCobra(t *testing.T) {
	tests := []struct {
		name     string
		sentinel string
	}{
		{name: "scope watchdog", sentinel: managedDoltScopeWatchdogArg},
		{name: "test watchdog", sentinel: managedDoltTestWatchdogArg},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			code := mainExitCode([]string{test.sentinel}, &stdout, &stderr)

			if code != 2 {
				t.Fatalf("mainExitCode(%q) = %d, want watchdog usage exit 2; stderr=%q", test.sentinel, code, stderr.String())
			}
			if stdout.Len() != 0 {
				t.Fatalf("mainExitCode(%q) stdout = %q, want empty", test.sentinel, stdout.String())
			}
			if !strings.Contains(stderr.String(), "usage: "+test.sentinel) {
				t.Fatalf("mainExitCode(%q) stderr = %q, want watchdog usage", test.sentinel, stderr.String())
			}
		})
	}
}

func TestPrivateManagedDoltWatchdogEntrypointDoesNotConsumeOrdinaryArgs(t *testing.T) {
	tests := [][]string{
		nil,
		{"version"},
		{managedDoltScopeWatchdogArg + "-other"},
		{managedDoltTestWatchdogArg + "-other"},
	}
	for _, args := range tests {
		var stdout, stderr bytes.Buffer
		handled, code := privateManagedDoltWatchdogEntrypoint(args, &stdout, &stderr)

		if handled || code != 0 {
			t.Fatalf("privateManagedDoltWatchdogEntrypoint(%q) = (%t, %d), want (false, 0)", args, handled, code)
		}
		if stdout.Len() != 0 || stderr.Len() != 0 {
			t.Fatalf("privateManagedDoltWatchdogEntrypoint(%q) wrote stdout=%q stderr=%q", args, stdout.String(), stderr.String())
		}
	}
}
