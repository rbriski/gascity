package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/clock"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// lumenDepDoc is a two-do DAG: A (pool) then B (after A). A stranded A must
// skip-cascade B.
func lumenDepDoc(t *testing.T) *ir.IR {
	t.Helper()
	doc := `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "chain",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [
           {"kind": "do", "id": "A", "name": "A", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "source": {"kind": "prompt"},
            "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "Do A.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}},
           {"kind": "do", "id": "B", "name": "B", "after": ["A"],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "source": {"kind": "prompt"},
            "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "Do B after A.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}}
         ]}
      ]
    }`
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode dep IR: %v", err)
	}
	return d
}

// lumenRetryDoDoc wraps a pool do "hello" in a `retry 3 { … }` — the recovery
// arm: an infrastructure strand (firewall-settled retryable) is re-attempted as a
// fresh activation, while an honest worker verdict returns immediately.
func lumenRetryDoDoc(t *testing.T) *ir.IR {
	t.Helper()
	const doc = `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "retrydo",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [
           {"kind": "retry", "id": "attempt", "name": "attempt", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "attempts": {"kind": "literal", "value": 3},
            "body": {"kind": "do", "id": "hello", "name": "hello", "after": [],
              "origin": {"uri": "t", "line": 1, "col": 0},
              "source": {"kind": "prompt"},
              "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
              "body": {"raw": "Say hello.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}}}
         ]}
      ]
    }`
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode retry-do IR: %v", err)
	}
	return d
}

// TestFirewallStrandBecomesFreshAttempt (T-F1) is the §2.4 recovery story in
// process: a stranded claimant on a retry-wrapped do's attempt :0 is firewall-
// settled as a RETRYABLE strand, the same-tick re-Advance mints a fresh attempt :1
// (fresh-tokened, claimable), and a scripted claim + pass close on :1 seals the run
// pass. The stranded worker's later close of :0 loses at the write-once token.
func TestFirewallStrandBecomesFreshAttempt(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	fake := &clock.Fake{Time: time.Now()}
	cr.ensureLumenRuntime().clk = fake

	streamID := lumenSeedRun(t, cityPath, lumenRetryDoDoc(t), nil, tbHookRoute)
	cr.lumenRunsTick(ctx) // materialize attempt hello:0

	claimGS := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, claimGS, streamID, "hello:0", "worker-a"); err != nil {
		_ = claimGS.Close()
		t.Fatalf("claim hello:0: %v", err)
	}
	_ = claimGS.Close()

	gs := cr.lumenGraphStore(ctx)
	empty := newSessionBeadSnapshot(nil) // the claimant is dead (no session bead)
	cr.lumenClaimOrphanFirewall(ctx, gs, empty)
	fake.Time = fake.Time.Add(lumenStrandedGrace(cr.patrolInterval()) + time.Second)
	cr.lumenClaimOrphanFirewall(ctx, gs, empty) // settles hello:0 retryable + re-Advance mints hello:1

	// hello:0 settled failed with the stranded marker; a fresh attempt hello:1 exists.
	if o, out := lumenOwnedSettledOutput(t, cityPath, streamID, "hello:0"); o != engine.OutcomeFailed || !strings.HasPrefix(out, "stranded:") {
		t.Fatalf("hello:0 settle = {%q, %q}, want {failed, stranded:...}", o, out)
	}
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventAttemptMinted); n != 2 {
		t.Fatalf("attempt.minted count = %d, want 2 (hello:0 then the re-attempt hello:1)", n)
	}
	if st := lumenNodeStatus(t, cityPath, streamID, "hello"); st != "open" {
		t.Fatalf("hello status after re-attempt = %q, want open (fresh attempt claimable)", st)
	}

	// A fresh worker (worker-b) claims the re-attempt hello:1.
	claimGS2 := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, claimGS2, streamID, "hello:1", "worker-b"); err != nil {
		_ = claimGS2.Close()
		t.Fatalf("claim hello:1: %v", err)
	}
	_ = claimGS2.Close()

	// The stranded worker-a (revived) closes hello while attempt :1 is LIVE (claimed by
	// worker-b): the bare-id close resolves to the live attempt :1, and the closer-
	// identity guard refuses worker-a loudly. NOTE: worker-a and worker-b have DISTINCT
	// names, so this exercises only the guard's NAME axis (sharp edge 8 / T-Z1 shape).
	// It does NOT cover the true singleton-pool false-kill, where a false-killed
	// instance and its RESPAWN share one stable session name — that same-name case is
	// caught by the instance-unique claimant-id axis and pinned by
	// TestSameNameZombieCloseCannotSettleFreshAttempt (H1) in the engine suite.
	t.Setenv("GC_SESSION_NAME", "worker-a")
	t.Setenv("GC_SESSION_ID", "")
	t.Setenv("GC_ALIAS", "")
	var stderr bytes.Buffer
	code, handled := interceptTierBClose(cityPath,
		[]string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"},
		io.Discard, &stderr)
	if !handled || code == 0 {
		t.Fatalf("stranded worker-a's close of live hello:1 = (code=%d handled=%v); want a loud non-claimant refusal", code, handled)
	}
	// Only hello:0's strand settle exists — the zombie appended nothing.
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventOwnedSettled); n != 1 {
		t.Fatalf("owned.settled count = %d, want 1 (the zombie's close appended nothing)", n)
	}

	// worker-b, the real claimant, closes hello:1 pass → the run seals pass.
	t.Setenv("GC_SESSION_NAME", "worker-b")
	settleGS := tbHookOpenStore(t, cityPath)
	if err := engine.SettleTierBWorkAs(ctx, settleGS, streamID, "hello:1", engine.OutcomePass, "done", "worker-b", "", false); err != nil {
		_ = settleGS.Close()
		t.Fatalf("settle hello:1: %v", err)
	}
	_ = settleGS.Close()
	cr.lumenRunsTick(ctx) // re-Advance → retry arm settles the loop pass → seal
	assertLumenRunSealedOutcome(t, cityPath, streamID, engine.OutcomePass)
}

// lumenWorkerSessionBead builds an open pool session bead owning the canonical
// singleton worker name (the same reused name across a false-kill/respawn, which is
// exactly why the firewall must key liveness on the instance-unique bead id, not the
// name), with the reconciler's stranded marker optionally stamped.
func lumenWorkerSessionBead(id string, stranded bool) beads.Bead {
	const sessionName = "worker-a"
	meta := map[string]string{
		"session_name":         sessionName,
		"template":             tbHookRoute,
		poolManagedMetadataKey: boolMetadata(true),
	}
	if stranded {
		meta[strandedEventEmittedKey] = "2026-07-08T00:00:00Z"
	}
	return beads.Bead{
		ID:       id,
		Title:    sessionName,
		Status:   "open",
		Type:     sessionBeadType,
		Labels:   []string{sessionBeadLabel, "template:" + tbHookRoute},
		Metadata: meta,
	}
}

// lumenNodeStatus reads a fold-owned node's projected status.
func lumenNodeStatus(t *testing.T, cityPath, streamID, nodeID string) string {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	var s string
	if err := gs.DB().QueryRowContext(context.Background(),
		`SELECT status FROM nodes WHERE id = ? AND stream_id = ? AND fold_owned = 1`, nodeID, streamID).Scan(&s); err != nil {
		t.Fatalf("read status of %q: %v", nodeID, err)
	}
	return s
}

// lumenOwnedSettledOutput returns (outcome, output) of the owned.settled for
// activation, reading the journal directly.
func lumenOwnedSettledOutput(t *testing.T, cityPath, streamID, activation string) (string, string) {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	events, err := gs.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	for _, e := range events {
		if e.Type != engine.EventOwnedSettled {
			continue
		}
		var p struct {
			Handle  string `json:"handle"`
			Outcome string `json:"outcome"`
			Output  string `json:"output"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode owned.settled: %v", err)
		}
		if p.Handle == activation {
			return p.Outcome, p.Output
		}
	}
	return "", ""
}

// assertLumenRunSealedOutcome asserts the run sealed with the given outcome.
func assertLumenRunSealedOutcome(t *testing.T, cityPath, streamID, outcome string) {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	events, err := gs.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	for _, e := range events {
		if e.Type != engine.EventRunClosed {
			continue
		}
		var p struct {
			Outcome string `json:"outcome"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			t.Fatalf("decode run.closed: %v", err)
		}
		if p.Outcome != outcome {
			t.Fatalf("run.closed outcome = %q, want %q", p.Outcome, outcome)
		}
		return
	}
	t.Fatalf("run %q not sealed (no run.closed)", streamID)
}

// TestFirewallSettlesDeadClaimantAfterGrace (T-E1) proves a claimed row whose
// assignee matches no session bead is settled failed only AFTER the grace window,
// and the re-Advance skip-cascades the dependent and seals the run failed.
func TestFirewallSettlesDeadClaimantAfterGrace(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	fake := &clock.Fake{Time: time.Now()}
	cr.ensureLumenRuntime().clk = fake

	streamID := lumenSeedRun(t, cityPath, lumenDepDoc(t), nil, tbHookRoute)
	cr.lumenRunsTick(ctx) // materialize A (park); B deferred

	// Claim A by a worker that has NO session bead (recycled/deleted claimant).
	claimGS := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, claimGS, streamID, "A:0", "ghost-worker"); err != nil {
		_ = claimGS.Close()
		t.Fatalf("claim A: %v", err)
	}
	_ = claimGS.Close()

	gs := cr.lumenGraphStore(ctx)
	empty := newSessionBeadSnapshot(nil) // matches nothing → ghost-worker is dead

	// t0: first observation records the grace clock but does NOT settle.
	cr.lumenClaimOrphanFirewall(ctx, gs, empty)
	if st := lumenNodeStatus(t, cityPath, streamID, "A"); st != engine.StatusClaimed {
		t.Fatalf("A status at t0 = %q, want in_progress (grace not elapsed)", st)
	}

	// t0 + grace + 1s: settle failed + re-Advance.
	fake.Time = fake.Time.Add(lumenStrandedGrace(cr.patrolInterval()) + time.Second)
	cr.lumenClaimOrphanFirewall(ctx, gs, empty)

	outcome, output := lumenOwnedSettledOutput(t, cityPath, streamID, "A:0")
	if outcome != engine.OutcomeFailed {
		t.Fatalf("A owned.settled outcome = %q, want failed", outcome)
	}
	if output != "stranded: ghost-worker" {
		t.Fatalf("A owned.settled output = %q, want \"stranded: ghost-worker\"", output)
	}
	if st := lumenNodeStatus(t, cityPath, streamID, "B"); st != "skipped" {
		t.Fatalf("B status = %q, want skipped (A failed → skip-cascade)", st)
	}
	assertLumenRunSealedOutcome(t, cityPath, streamID, engine.OutcomeFailed)
}

// TestFirewallStrandedMarkerTrigger (T-E2) proves the marker drives the verdict: a
// matched session WITH the reconciler's stranded marker fires after grace; the same
// session WITHOUT the marker never fires.
func TestFirewallStrandedMarkerTrigger(t *testing.T) {
	ctx := context.Background()

	run := func(t *testing.T, stranded bool) (settled bool) {
		cr, cityPath, _ := lumenTestRuntime(t)
		fake := &clock.Fake{Time: time.Now()}
		cr.ensureLumenRuntime().clk = fake

		streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
		cr.lumenRunsTick(ctx)
		claimGS := tbHookOpenStore(t, cityPath)
		if err := engine.ClaimTierBWork(ctx, claimGS, streamID, "hello:0", "worker-a"); err != nil {
			_ = claimGS.Close()
			t.Fatalf("claim: %v", err)
		}
		_ = claimGS.Close()

		gs := cr.lumenGraphStore(ctx)
		snap := newSessionBeadSnapshot([]beads.Bead{lumenWorkerSessionBead("sess-1", stranded)})

		cr.lumenClaimOrphanFirewall(ctx, gs, snap) // t0: start clock
		// Well past grace.
		fake.Time = fake.Time.Add(3 * lumenStrandedGrace(cr.patrolInterval()))
		cr.lumenClaimOrphanFirewall(ctx, gs, snap)

		return lumenNodeStatus(t, cityPath, streamID, "hello") != engine.StatusClaimed
	}

	if !run(t, true) {
		t.Fatal("a session WITH the stranded marker was not firewall-settled after grace")
	}
	if run(t, false) {
		t.Fatal("a live session WITHOUT the stranded marker was firewall-settled (false kill)")
	}
}

// TestFirewallZombieLateCloseLosesLoud (T-E3) proves a zombie worker's later close
// of a firewall-settled row is a loud divergent-reclose refusal, with exactly one
// owned.settled in the journal.
func TestFirewallZombieLateCloseLosesLoud(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	fake := &clock.Fake{Time: time.Now()}
	cr.ensureLumenRuntime().clk = fake

	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
	cr.lumenRunsTick(ctx)
	claimGS := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, claimGS, streamID, "hello:0", "zombie"); err != nil {
		_ = claimGS.Close()
		t.Fatalf("claim: %v", err)
	}
	_ = claimGS.Close()

	gs := cr.lumenGraphStore(ctx)
	empty := newSessionBeadSnapshot(nil)
	cr.lumenClaimOrphanFirewall(ctx, gs, empty)
	fake.Time = fake.Time.Add(lumenStrandedGrace(cr.patrolInterval()) + time.Second)
	cr.lumenClaimOrphanFirewall(ctx, gs, empty) // settles failed

	// The zombie's late close (gc.outcome=pass) diverges from the failed settle.
	var stderr bytes.Buffer
	code, handled := interceptTierBClose(cityPath,
		[]string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"},
		io.Discard, &stderr)
	if !handled || code == 0 {
		t.Fatalf("zombie close = (code=%d handled=%v), want a loud non-zero refusal", code, handled)
	}
	if !bytes.Contains(stderr.Bytes(), []byte("divergent re-close")) {
		t.Fatalf("stderr = %q, want a divergent-reclose refusal", stderr.String())
	}
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventOwnedSettled); n != 1 {
		t.Fatalf("owned.settled count = %d, want 1 (the zombie lost)", n)
	}
}

// TestFirewallSparesUnclaimedAndRecovered (T-E4) proves an OPEN (unclaimed)
// frontier row is never firewalled, and a candidate whose session reappears before
// grace elapses is dropped from the grace clock (never settled).
func TestFirewallSparesUnclaimedAndRecovered(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	fake := &clock.Fake{Time: time.Now()}
	cr.ensureLumenRuntime().clk = fake

	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
	cr.lumenRunsTick(ctx) // materialize hello (OPEN, unclaimed)

	gs := cr.lumenGraphStore(ctx)
	empty := newSessionBeadSnapshot(nil)

	// An unclaimed row is never a firewall candidate, no matter how much time passes.
	fake.Time = fake.Time.Add(10 * lumenStrandedGrace(cr.patrolInterval()))
	cr.lumenClaimOrphanFirewall(ctx, gs, empty)
	if st := lumenNodeStatus(t, cityPath, streamID, "hello"); st != "open" {
		t.Fatalf("unclaimed hello status = %q, want open (never firewalled)", st)
	}

	// Now claim it and start the grace clock with a dead session...
	claimGS := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, claimGS, streamID, "hello:0", "worker-a"); err != nil {
		_ = claimGS.Close()
		t.Fatalf("claim: %v", err)
	}
	_ = claimGS.Close()
	cr.lumenClaimOrphanFirewall(ctx, gs, empty) // records first-seen dead

	// ...then the session REAPPEARS (live) before grace elapses: the entry is cleared.
	recovered := newSessionBeadSnapshot([]beads.Bead{lumenWorkerSessionBead("sess-1", false)})
	fake.Time = fake.Time.Add(lumenStrandedGrace(cr.patrolInterval()) + time.Second)
	cr.lumenClaimOrphanFirewall(ctx, gs, recovered)
	if st := lumenNodeStatus(t, cityPath, streamID, "hello"); st != engine.StatusClaimed {
		t.Fatalf("recovered claimant status = %q, want in_progress (grace reset, not killed)", st)
	}
	if _, seen := cr.lumen.deadSince["hello:0"]; seen {
		t.Fatal("grace clock entry survived a recovery (should be cleared)")
	}
}

// TestFirewallInstanceLivenessKeysOnClaimantID (HIGH) is the firewall-wedge fix's
// core pin: the dead-claimant verdict keys on the recorded claimant_id (the claiming
// session's instance-unique GC_SESSION_ID = its session bead id), not the reused NAME.
// A same-NAME respawn has a DIFFERENT bead id, so it must NOT revive the verdict and
// reset the grace clock (the wedge) — it strands at the floor; a same-BEAD crash-resume
// (matching id) is alive and its clock resets (no false kill). A legacy no-id claim
// falls back to the NAME loop unchanged. The respawn subtest FAILS on the pre-fix
// name-keyed verdict (the respawn matches by name → alive → never strands).
func TestFirewallInstanceLivenessKeysOnClaimantID(t *testing.T) {
	ctx := context.Background()

	// claimWithID seeds a parked do run, claims hello:0 as worker-a recording claimantID,
	// and returns the runtime handles for the firewall sweep.
	claimWithID := func(t *testing.T, claimantID string) (*CityRuntime, string, *graphstore.Store, *clock.Fake, string) {
		t.Helper()
		cr, cityPath, _ := lumenTestRuntime(t)
		fake := &clock.Fake{Time: time.Now()}
		cr.ensureLumenRuntime().clk = fake
		streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
		cr.lumenRunsTick(ctx) // materialize hello:0 (parked)
		claimGS := tbHookOpenStore(t, cityPath)
		if err := engine.ClaimTierBWorkAs(ctx, claimGS, streamID, "hello:0", "worker-a", claimantID); err != nil {
			_ = claimGS.Close()
			t.Fatalf("claim: %v", err)
		}
		_ = claimGS.Close()
		return cr, cityPath, cr.lumenGraphStore(ctx), fake, streamID
	}

	// strandedAfterGrace runs the firewall twice (start the clock, then past grace) and
	// reports whether hello was settled (left StatusClaimed).
	strandedAfterGrace := func(t *testing.T, cr *CityRuntime, cityPath string, gs *graphstore.Store, fake *clock.Fake, streamID string, snap *sessionBeadSnapshot) bool {
		t.Helper()
		cr.lumenClaimOrphanFirewall(ctx, gs, snap) // t0: start the grace clock (if dead)
		fake.Time = fake.Time.Add(lumenStrandedGrace(cr.patrolInterval()) + time.Second)
		cr.lumenClaimOrphanFirewall(ctx, gs, snap)
		return lumenNodeStatus(t, cityPath, streamID, "hello") != engine.StatusClaimed
	}

	t.Run("respawn_new_bead_id_is_dead", func(t *testing.T) {
		cr, cityPath, gs, fake, streamID := claimWithID(t, "sess-A")
		// A same-NAME respawn B with a DIFFERENT bead id — must NOT reset the grace clock.
		snap := newSessionBeadSnapshot([]beads.Bead{lumenWorkerSessionBead("sess-B", false)})
		if !strandedAfterGrace(t, cr, cityPath, gs, fake, streamID, snap) {
			t.Fatal("a same-name respawn (new bead id) kept the claim alive (the wedge); want a strand at the floor")
		}
	})

	t.Run("crash_resume_same_bead_id_is_alive", func(t *testing.T) {
		cr, cityPath, gs, fake, streamID := claimWithID(t, "sess-A")
		// The SAME instance (same bead id) crash-resumed — a live claimant, never stranded.
		snap := newSessionBeadSnapshot([]beads.Bead{lumenWorkerSessionBead("sess-A", false)})
		if strandedAfterGrace(t, cr, cityPath, gs, fake, streamID, snap) {
			t.Fatal("a same-bead crash-resume was firewall-stranded (false kill of a live claimant)")
		}
	})

	t.Run("open_bead_with_marker_is_stranded", func(t *testing.T) {
		cr, cityPath, gs, fake, streamID := claimWithID(t, "sess-A")
		// The claimant's bead is open but carries the reconciler's stranded marker.
		snap := newSessionBeadSnapshot([]beads.Bead{lumenWorkerSessionBead("sess-A", true)})
		if !strandedAfterGrace(t, cr, cityPath, gs, fake, streamID, snap) {
			t.Fatal("a stranded-marked open claimant was not firewall-settled after grace")
		}
	})

	t.Run("empty_claimant_id_falls_back_to_name", func(t *testing.T) {
		// A legacy (no-id) claim consults the NAME loop: a matching-NAME live session is
		// alive (byte-identical to pre-hardening), regardless of its bead id.
		cr, cityPath, gs, fake, streamID := claimWithID(t, "")
		alive := newSessionBeadSnapshot([]beads.Bead{lumenWorkerSessionBead("sess-any", false)})
		if strandedAfterGrace(t, cr, cityPath, gs, fake, streamID, alive) {
			t.Fatal("a legacy no-id claim with a matching-NAME live session was stranded (name fallback broke)")
		}
	})
}

// TestFirewallVsWorkerSettleRace (T-E5) is the multi-writer settle race: a worker
// SettleTierBWork(pass) racing the firewall's SettleTierBWork(failed) on one claimed
// activation — exactly one owned.settled lands, the loser surfaces
// ErrTierBClaimConflict, and the journal converges (Verify clean).
func TestFirewallVsWorkerSettleRace(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	tbSeedClaimedPoolRow(t, cityPath) // "hello" claimed by worker-a on tbHookStream

	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()

	var (
		wg       sync.WaitGroup
		start    = make(chan struct{})
		errs     [2]error
		outcomes = [2]string{engine.OutcomePass, engine.OutcomeFailed}
	)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = engine.SettleTierBWork(ctx, gs, tbHookStream, "hello:0", outcomes[i], fmt.Sprintf("settle-%d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	winners, conflicts := 0, 0
	for i := 0; i < 2; i++ {
		switch {
		case errs[i] == nil:
			winners++
		case errors.Is(errs[i], engine.ErrTierBClaimConflict) || errors.Is(errs[i], graphstore.ErrLeaseFenced):
			conflicts++
		default:
			t.Fatalf("settle %d errored with an unexpected error: %v", i, errs[i])
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 (write-once settle)", winners)
	}
	if conflicts != 1 {
		t.Fatalf("conflicts = %d, want exactly 1 (the loser)", conflicts)
	}
	if n := lumenCountJournalType(t, cityPath, tbHookStream, engine.EventOwnedSettled); n != 1 {
		t.Fatalf("owned.settled rows = %d, want exactly 1", n)
	}
	if err := gs.Verify(ctx, tbHookStream); err != nil {
		t.Fatalf("Verify after settle race: %v", err)
	}
}
