package engine

// This file is the executor's crash-injection seam (blueprint §5, slice P4.4).
// It is a TEST-ONLY hook that is INERT in production: crashHook is nil, so
// crashAt is a single nil comparison per boundary with no allocation and no
// behavior change. The crash-injection harness (crash_test.go) installs a hook
// via SetCrashHookForTest to abandon a run mid-cycle at a chosen boundary — the
// hook returns a sentinel error the run propagates out — then resumes over the
// surviving journal to prove convergence and the effect at-most-once contract.
//
// In-process injection is deliberate (over the blueprint's SIGKILL/PID-ns
// sketch): returning an error at the exact boundary is deterministic, needs no
// subprocess, and runs clean under `go test -race`. The surviving journal is
// byte-for-byte what an uncooperative kill at that boundary would leave — every
// append commits its event and its Tier-A projection before a boundary fires,
// so the store is consistent at each injection point (the intra-append H1
// window — a crash between an event's commit and its projection — is exercised
// separately by the P4.3 resume-reconciles-projection test).

// crashBoundary names one point in the executor's decide -> persist -> act ->
// persist cycle at which a test may inject a crash. The labels are compiled into
// production but never triggered there (crashHook stays nil).
type crashBoundary string

const (
	// crashAfterRunStarted fires after run.started commits, before the first unit.
	crashAfterRunStarted crashBoundary = "after-run-started"
	// crashBeforeActivate fires after the decide phase picks a unit, before its
	// node.activated append — the (a) boundary. Nothing for the node is on disk yet.
	crashBeforeActivate crashBoundary = "before-activate"
	// crashBeforeAct fires after the pre-act append (node.activated for exec,
	// effect.scheduled for do) but before the side effect runs — the (b) boundary.
	// The effect has NOT run; the host is NOT called.
	crashBeforeAct crashBoundary = "before-act"
	// crashAfterDispatch fires on the real-bead do path (REDESIGN §9.1) after the
	// DispatchWork side effect created the (durable, findable-by-metadata) work bead
	// but BEFORE the write-once owned.admitted{work_bead} dispatch fact commits. On
	// re-Advance the lookup-before-create seam re-finds the same bead id and lands
	// the missing dispatch fact — exactly one bead, exactly one owned.admitted.
	crashAfterDispatch crashBoundary = "after-dispatch"
	// crashAfterAct fires after the side effect runs (exec shell / agent RunDo)
	// but before its settlement append — the (c) boundary. The effect DID run
	// exactly once; its outcome has not been recorded.
	crashAfterAct crashBoundary = "after-act"
	// crashAfterSettle fires after a unit's outcome.settled commits, before the
	// next unit — the (d) boundary. The unit is fully settled on disk.
	crashAfterSettle crashBoundary = "after-settle"
	// crashBeforeSnapshot fires when a snapshot is due but before it is written
	// (no snapshots row, no snapshot.anchored event yet).
	crashBeforeSnapshot crashBoundary = "before-snapshot"
	// crashAfterSnapshot fires after a snapshot.anchored event commits and its
	// state is durable — the resume anchor exists.
	crashAfterSnapshot crashBoundary = "after-snapshot"
	// crashBeforeRunClosed fires after the last unit and the seal snapshot, before
	// run.closed — the run has done all its work but is not sealed.
	crashBeforeRunClosed crashBoundary = "before-run-closed"
)

// crashHook is the TEST-ONLY injection seam. It is nil in production; the sole
// writer is SetCrashHookForTest in the test binary. It receives the boundary, the
// run's stream id, and the current activation (the stream id for run-level and
// snapshot boundaries) so a harness can target a precise (boundary, activation)
// cell and record which stream to resume.
var crashHook func(b crashBoundary, streamID, activation string) error

// crashAt consults the test-only crash seam at boundary b for the given
// activation. In production crashHook is nil and this returns nil immediately —
// the only production cost of the seam is this nil comparison.
func (d *driver) crashAt(b crashBoundary, activation string) error {
	if crashHook == nil {
		return nil
	}
	return crashHook(b, d.streamID, activation)
}
