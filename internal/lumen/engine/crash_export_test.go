package engine

// This file exposes the test-only crash seam to the external crash-injection
// harness (crash_test.go, package engine_test). Because it is a _test.go file it
// compiles ONLY into the test binary — it adds NOTHING to the production API. The
// crashHook variable it writes is nil in production; this setter is its sole
// writer, so production behavior is provably unchanged (a nil-guarded no-op).

// Exported boundary labels for the harness's (boundary, activation) table. They
// mirror the unexported crashBoundary constants without exporting the type.
const (
	CrashAfterRunStarted = string(crashAfterRunStarted)
	CrashBeforeActivate  = string(crashBeforeActivate)
	CrashAfterActivate   = string(crashAfterActivate)
	CrashAfterDispatch   = string(crashAfterDispatch)
	CrashBeforeAct       = string(crashBeforeAct)
	CrashAfterAct        = string(crashAfterAct)
	CrashAfterSettle     = string(crashAfterSettle)
	CrashBeforeSnapshot  = string(crashBeforeSnapshot)
	CrashAfterSnapshot   = string(crashAfterSnapshot)
	CrashBeforeRunClosed = string(crashBeforeRunClosed)
)

// SetCrashHookForTest installs a crash injector and returns a restore func the
// caller must defer. fn receives (boundary, streamID, activation) and returns a
// non-nil error to abandon the run at that point; nil clears the hook. It exists
// only in the test binary.
func SetCrashHookForTest(fn func(boundary, streamID, activation string) error) func() {
	prev := crashHook
	if fn == nil {
		crashHook = nil
	} else {
		crashHook = func(b crashBoundary, streamID, activation string) error {
			return fn(string(b), streamID, activation)
		}
	}
	return func() { crashHook = prev }
}

// CrashHookInstalled reports whether a crash hook is currently installed. The
// harness asserts it is false by default (production is inert) and after teardown.
func CrashHookInstalled() bool { return crashHook != nil }
