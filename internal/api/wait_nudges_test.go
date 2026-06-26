package api

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestWithdrawQueuedWaitNudgesUsesNudgesStore guards the nudges-relocation
// invariant on the API surface: every call to withdrawQueuedWaitNudges must
// pass the nudges-class store (s.state.NudgesBeadStore()), never the work store
// (CityBeadStore / a bare `store`). The gc:nudge shadow beads live in the
// relocated nudges store when [beads.classes.nudges] is relocated; passing the
// work store there silently orphans them — markTerminalBeadByID does
// store.Get(beadID), the relocated bead is absent, ErrNotFound is swallowed
// (internal/nudgequeue/waits.go), so the queue file is drained but the open
// gc:nudge shadow is never terminalized. The CLI path was migrated to
// openNudgesClassStore; this test keeps the API path from regressing.
func TestWithdrawQueuedWaitNudgesUsesNudgesStore(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("Glob(*.go): %v", err)
	}
	callRe := regexp.MustCompile(`withdrawQueuedWaitNudges\(([^,]+),`)
	var offenders []string
	for _, file := range files {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		data, err := os.ReadFile(file)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", file, err)
		}
		for i, line := range strings.Split(string(data), "\n") {
			if strings.Contains(line, "func withdrawQueuedWaitNudges") {
				continue // the definition, not a call site
			}
			m := callRe.FindStringSubmatch(line)
			if m == nil {
				continue
			}
			if strings.TrimSpace(m[1]) != "s.state.NudgesBeadStore()" {
				offenders = append(offenders, fmt.Sprintf("%s:%d: %s", file, i+1, strings.TrimSpace(line)))
			}
		}
	}
	if len(offenders) > 0 {
		t.Fatalf("withdrawQueuedWaitNudges must be called with s.state.NudgesBeadStore() (not the work store); offenders:\n%s",
			strings.Join(offenders, "\n"))
	}
}

// TestFakeStateNudgesBeadStoreFallsBackToCityStore documents the default-backend
// equivalence: with no relocated nudges store configured, NudgesBeadStore returns
// the work store, so the API path is byte-identical at the default backend.
func TestFakeStateNudgesBeadStoreFallsBackToCityStore(t *testing.T) {
	f := newFakeState(t)
	if got := f.NudgesBeadStore(); got != f.CityBeadStore() {
		t.Fatalf("default backend: NudgesBeadStore() must equal CityBeadStore(); got distinct stores")
	}
	relocated := beads.NewMemStore()
	f.nudgesBeadStore = relocated
	if got := f.NudgesBeadStore(); got != relocated {
		t.Fatalf("relocated backend: NudgesBeadStore() must return the configured nudges store")
	}
}

// TestFakeStateSessionsBeadStoreFallsBackToCityStore documents the default-backend
// equivalence for the sessions seam: with no relocated sessions store configured,
// SessionsBeadStore returns the work store, so the API path is byte-identical at
// the default backend.
func TestFakeStateSessionsBeadStoreFallsBackToCityStore(t *testing.T) {
	f := newFakeState(t)
	if got := f.SessionsBeadStore(); got != f.CityBeadStore() {
		t.Fatalf("default backend: SessionsBeadStore() must equal CityBeadStore(); got distinct stores")
	}
	relocated := beads.NewMemStore()
	f.sessionsBeadStore = relocated
	if got := f.SessionsBeadStore(); got != relocated {
		t.Fatalf("relocated backend: SessionsBeadStore() must return the configured sessions store")
	}
}
