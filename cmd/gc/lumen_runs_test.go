package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/api"
	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
	"github.com/gastownhall/gascity/internal/runtime"
)

// lumenTestRuntime builds a graph-scoped temp city plus a minimal CityRuntime for
// driving lumenRunsTick directly, and returns the runtime, cityPath, and the
// stderr buffer (for loud-refusal assertions).
func lumenTestRuntime(t *testing.T) (*CityRuntime, string, *bytes.Buffer) {
	t.Helper()
	cityPath := tbHookGraphCity(t)
	var stderr bytes.Buffer
	cr := &CityRuntime{
		cityPath:  cityPath,
		cityName:  "test",
		logPrefix: "test",
		stdout:    io.Discard,
		stderr:    &stderr,
		pokeCh:    make(chan struct{}, 1),
		// Pool agents matching the routes these tests use ("rig/claude" and "workers")
		// so the real-bead dispatch seam's route validation resolves them (REDESIGN
		// §2.3): the do's work becomes an ordinary bead in standaloneCityStore.
		cfg: &config.City{Agents: []config.Agent{
			{Dir: "rig", Name: "claude"},
			{Name: "workers"},
		}},
		standaloneCityStore: beads.NewMemStore(),
		rec:                 events.Discard,
	}
	t.Cleanup(func() {
		if cr.lumen != nil && cr.lumen.gs != nil {
			_ = cr.lumen.gs.Close()
		}
	})
	return cr, cityPath, &stderr
}

// lumenSeedRun enqueues a run (BOTH content-addressed CAS blobs written first, then
// run.started) exactly as lumenEnqueue does, returning its stream id. It opens its
// own short-lived store handle, as a separate enqueue process would.
func lumenSeedRun(t *testing.T, cityPath string, doc *ir.IR, input map[string]any, route string) string {
	t.Helper()
	if err := writeLumenIRBlob(cityPath, engine.IRHash(doc), doc); err != nil {
		t.Fatalf("write IR blob: %v", err)
	}
	if err := writeLumenInputBlob(cityPath, engine.InputHash(input), input); err != nil {
		t.Fatalf("write input blob: %v", err)
	}
	gs := tbHookOpenStore(t, cityPath)
	streamID, err := engine.EnqueueRun(context.Background(), gs, doc, input, "test/formula@v1", route)
	if err != nil {
		_ = gs.Close()
		t.Fatalf("enqueue: %v", err)
	}
	_ = gs.Close()
	return streamID
}

// lumenStreamEventTypes reads streamID (through a fresh handle) and returns the
// ordered event types.
func lumenStreamEventTypes(t *testing.T, cityPath, streamID string) []string {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	events, err := gs.ReadStream(context.Background(), streamID, 1, 0)
	if err != nil {
		t.Fatalf("read stream: %v", err)
	}
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// TestLumenRunsTickDispatchesRealWorkBead (real-bead redesign, replaces T-B1) proves
// one controller tick over an enqueued do-only run creates an ORDINARY fold_owned=0
// work bead in the CITY WORK store — task-typed, routed, prompt in Description, run
// linkage metadata — and journals owned.admitted{work_bead}. NO claimable Tier-B fold
// row is minted (the real bead is the only claim surface).
func TestLumenRunsTickDispatchesRealWorkBead(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	cr.lumenRunsTick(ctx)

	// The real work bead lives in the city WORK store, not the graph journal.
	work := cr.cityBeadStore()
	rows, err := work.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.LumenRunMetadataKey: streamID},
		IncludeClosed: true,
		AllowScan:     true,
	})
	if err != nil {
		t.Fatalf("list work beads: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("work beads for run = %d, want 1 (the dispatched do)", len(rows))
	}
	b := rows[0]
	if b.Type != "task" {
		t.Errorf("bead type = %q, want task", b.Type)
	}
	if b.Description != "Say hello." {
		t.Errorf("description = %q, want the rendered prompt", b.Description)
	}
	if b.Status != "open" {
		t.Errorf("status = %q, want open (born claimable)", b.Status)
	}
	if b.Metadata[beadmeta.RoutedToMetadataKey] != tbHookRoute {
		t.Errorf("gc.routed_to = %q, want %q", b.Metadata[beadmeta.RoutedToMetadataKey], tbHookRoute)
	}
	if b.Metadata[beadmeta.LumenActivationMetadataKey] != "hello:0" {
		t.Errorf("gc.lumen_activation = %q, want hello:0", b.Metadata[beadmeta.LumenActivationMetadataKey])
	}
	if b.Metadata[beadmeta.LumenAttemptMetadataKey] != "0" {
		t.Errorf("gc.lumen_attempt = %q, want 0", b.Metadata[beadmeta.LumenAttemptMetadataKey])
	}

	// Journal: run.started, node.activated, owned.admitted(work_bead) — no owned.settled.
	if types := lumenStreamEventTypes(t, cityPath, streamID); len(types) != 3 ||
		types[0] != engine.EventRunStarted || types[1] != engine.EventNodeActivated || types[2] != engine.EventOwnedAdmitted {
		t.Fatalf("journal = %v, want [run.started node.activated owned.admitted]", types)
	}
}

// lumenCloseDoBead simulates an ordinary pooled worker closing the latest open real
// work bead for a do node: it stamps gc.outcome and closes the bead through the plain
// work store (NO Tier-B leg). Returns the closed bead id.
func lumenCloseDoBead(t *testing.T, store beads.Store, streamID, nodeID, outcome string) string { //nolint:unparam // nodeID kept explicit to document the (stream, node) identity at call sites (mirrors lumenAttemptHistory); the single-do fixture happens to always use "hello"
	t.Helper()
	hist, err := lumenAttemptHistory(store, streamID, nodeID)
	if err != nil {
		t.Fatalf("attempt history: %v", err)
	}
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Status != "closed" {
			id := hist[i].BeadID
			if uerr := store.Update(id, beads.UpdateOpts{Metadata: map[string]string{beadmeta.OutcomeMetadataKey: outcome}}); uerr != nil {
				t.Fatalf("stamp outcome on %q: %v", id, uerr)
			}
			if cerr := store.Close(id); cerr != nil {
				t.Fatalf("close %q: %v", id, cerr)
			}
			return id
		}
	}
	t.Fatalf("no open work bead for %s/%s to close", streamID, nodeID)
	return ""
}

// TestLumenRunsTickDispatchObserveSeals (real-bead redesign, replaces T-B2) is THE
// controller happy path: enqueue → tick dispatches the real bead → an ORDINARY close
// of that bead (gc.outcome=pass, no Tier-B leg) → tick observes the close → seals. The
// journal sequence is run.started → node.activated → owned.admitted(work_bead) →
// outcome.settled → run.closed (NOT owned.settled — the settle is outcome.settled).
func TestLumenRunsTickDispatchObserveSeals(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	// Tick 1: dispatch the real bead (park, in flight).
	cr.lumenRunsTick(ctx)

	// An ordinary pooled worker closes the real bead pass (no Tier-B leg).
	closedID := lumenCloseDoBead(t, cr.cityBeadStore(), streamID, "hello", beadmeta.OutcomePass)

	// Tick 2: the observe arm sees the close → outcome.settled → seal.
	cr.lumenRunsTick(ctx)

	want := []string{
		engine.EventRunStarted, engine.EventNodeActivated,
		engine.EventOwnedAdmitted, engine.EventOutcomeSettled, engine.EventRunClosed,
	}
	if got := lumenStreamEventTypes(t, cityPath, streamID); !reflect.DeepEqual(got, want) {
		t.Fatalf("journal sequence = %v, want %v", got, want)
	}
	assertLumenRunSealed(t, cityPath, streamID)

	// The settled work bead is an ordinary fold_owned=0 bead in the work store.
	b, err := cr.cityBeadStore().Get(closedID)
	if err != nil || b.Status != "closed" {
		t.Fatalf("closed real bead %q = (%+v, %v), want a closed work-store bead", closedID, b, err)
	}
}

// closeLumenDoBeadWithOutput closes the latest open real work bead for a do node the
// way an ordinary pooled worker would — stamping gc.outcome AND gc.output_json (the
// dispatcher's step-output convention the downstream {{ref}} consumes). Returns the id.
func closeLumenDoBeadWithOutput(t *testing.T, store beads.Store, streamID, nodeID, outcome, output string) string {
	t.Helper()
	hist, err := lumenAttemptHistory(store, streamID, nodeID)
	if err != nil {
		t.Fatalf("attempt history: %v", err)
	}
	for i := len(hist) - 1; i >= 0; i-- {
		if hist[i].Status != "closed" {
			id := hist[i].BeadID
			if uerr := store.Update(id, beads.UpdateOpts{Metadata: map[string]string{
				beadmeta.OutcomeMetadataKey:    outcome,
				beadmeta.OutputJSONMetadataKey: output,
			}}); uerr != nil {
				t.Fatalf("stamp outcome+output on %q: %v", id, uerr)
			}
			if cerr := store.Close(id); cerr != nil {
				t.Fatalf("close %q: %v", id, cerr)
			}
			return id
		}
	}
	t.Fatalf("no open work bead for %s/%s to close", streamID, nodeID)
	return ""
}

// lumenWorkBeadForActivation returns the (single) real work bead a run dispatched for
// an exact gc.lumen_activation.
func lumenWorkBeadForActivation(t *testing.T, store beads.Store, streamID, activation string) beads.Bead {
	t.Helper()
	rows, err := store.List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.LumenRunMetadataKey: streamID, beadmeta.LumenActivationMetadataKey: activation},
		IncludeClosed: true,
		AllowScan:     true,
	})
	if err != nil {
		t.Fatalf("list work beads for %s/%s: %v", streamID, activation, err)
	}
	if len(rows) != 1 {
		t.Fatalf("work beads for activation %s = %d, want 1", activation, len(rows))
	}
	return rows[0]
}

// TestLumenRunsTickValuePlumbingDownstreamPrompt is the controller-level HIGH-2/3
// value-plumbing proof: enqueue do A → do B (after A, prompt "use {{A}}"); tick 1
// dispatches A; an ordinary worker closes A with gc.output_json="aval"; tick 2 observes
// A, seeds the scope, and dispatches B in the SAME pass — so B's dispatched work bead
// Description is the RESOLVED "use aval", not "use {{A}}". This exercises the full
// chain end to end: the observe seam reads gc.output_json, the driver seeds scope, and
// the next do renders against it.
func TestLumenRunsTickValuePlumbingDownstreamPrompt(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	streamID := lumenSeedRun(t, cityPath, tbHookChainDoc(t), nil, "workers")
	work := cr.cityBeadStore()

	// Tick 1: dispatch A (B is deferred behind A).
	cr.lumenRunsTick(ctx)
	if _, err := work.List(beads.ListQuery{Metadata: map[string]string{beadmeta.LumenActivationMetadataKey: "B:0"}, AllowScan: true}); err != nil {
		t.Fatalf("list: %v", err)
	}

	// An ordinary worker closes A pass with an output.
	closeLumenDoBeadWithOutput(t, work, streamID, "A", beadmeta.OutcomePass, "aval")

	// Tick 2: observe A → seed scope → dispatch B same pass with the resolved prompt.
	cr.lumenRunsTick(ctx)

	b := lumenWorkBeadForActivation(t, work, streamID, "B:0")
	if b.Description != "use aval" {
		t.Fatalf("B dispatched prompt = %q, want \"use aval\" ({{A}} must resolve to A's output)", b.Description)
	}
}

// TestLumenRunsTickCrashResume (T-B3) is the L2 exit's crash-resume proof: after a
// park, discarding ALL in-memory tick state and the store handle (a controller
// restart) and re-ticking rebuilds from the journal + CAS dir, re-parks WITHOUT
// re-materializing (idempotent), and a subsequent scripted close + tick seals.
func TestLumenRunsTickCrashResume(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	cr.lumenRunsTick(ctx)
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventNodeActivated); n != 1 {
		t.Fatalf("node.activated after first tick = %d, want 1", n)
	}

	// Simulate a controller restart: drop the store handle and all cursors. The city
	// WORK store is DURABLE across a restart, so cr2 re-opens the SAME work store
	// (the dispatched bead survives) — modeled by sharing the memstore handle.
	if cr.lumen != nil && cr.lumen.gs != nil {
		_ = cr.lumen.gs.Close()
	}
	cr2, _, _ := lumenTestRuntime(t)
	cr2.cityPath = cityPath                          // same city, fresh runtime (empty cursors, nil store handle)
	cr2.standaloneCityStore = cr.standaloneCityStore // durable work store survives the restart

	// Fresh tick rebuilds from the journal + CAS dir; the run re-parks and is NOT
	// re-dispatched (the write-once dispatch fact + the metadata lookup dedupe).
	cr2.lumenRunsTick(ctx)
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventNodeActivated); n != 1 {
		t.Fatalf("node.activated after crash-resume tick = %d, want 1 (idempotent, no re-materialize)", n)
	}
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventOwnedAdmitted); n != 1 {
		t.Fatalf("owned.admitted after crash-resume tick = %d, want 1 (idempotent dispatch)", n)
	}

	// An ordinary close of the surviving real bead + a tick seals across the restart.
	lumenCloseDoBead(t, cr2.cityBeadStore(), streamID, "hello", beadmeta.OutcomePass)

	cr2.lumenRunsTick(ctx)
	assertLumenRunSealed(t, cityPath, streamID)
	if cr2.lumen != nil && cr2.lumen.gs != nil {
		_ = cr2.lumen.gs.Close()
	}
}

// TestLumenRunsTickAdvancesWhileInflight (real-bead redesign, replaces T-B4) proves
// the level trigger under the new transport: a run with in-flight real work ALWAYS
// re-Advances even though a bead close never moves the journal head (that is how the
// observe arm ever sees the close); a run with an unmoved head AND no in-flight work
// is skipped. The observed close then seals.
func TestLumenRunsTickAdvancesWhileInflight(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	var calls int32
	orig := lumenAdvance
	lumenAdvance = func(ctx context.Context, store *graphstore.Store, doc *ir.IR, sid string, input map[string]any, opts engine.Options) (engine.AdvanceResult, error) {
		atomic.AddInt32(&calls, 1)
		return orig(ctx, store, doc, sid, input, opts)
	}
	defer func() { lumenAdvance = orig }()

	cr.lumenRunsTick(ctx) // dispatch + park: Advance #1, one bead in flight
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("advance calls after first tick = %d, want 1", got)
	}
	// Head is unmoved (a bead close lands in the WORK store), but inflight>0 → the
	// tick MUST re-Advance so the observe arm can Get the (still-open) bead.
	cr.lumenRunsTick(ctx)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("advance calls with in-flight work = %d, want 2 (inflight forces re-Advance)", got)
	}

	// The ordinary worker closes the bead; the next tick observes → seals.
	lumenCloseDoBead(t, cr.cityBeadStore(), streamID, "hello", beadmeta.OutcomePass)
	cr.lumenRunsTick(ctx)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("advance calls after close = %d, want 3", got)
	}
	assertLumenRunSealed(t, cityPath, streamID)

	// Sealed: inflight cleared → a further tick is level-trigger-skipped.
	cr.lumenRunsTick(ctx)
	if got := atomic.LoadInt32(&calls); got != 3 {
		t.Fatalf("advance calls after seal = %d, want 3 (sealed run, no inflight, head unmoved → skip)", got)
	}
}

// TestLumenRunsTickRetriesRebuildRaced pins that ErrRebuildRaced from Advance (a
// driver materialize-append whose RebuildTierA races a concurrent worker settle)
// is classified transient/retryable — matching engine isRetryableRaceErr and the
// Tier-B claim/settle adapters — so the loop leaves the cursor untouched and
// re-Advances on the next tick rather than taking the loud-terminal branch.
func TestLumenRunsTickRetriesRebuildRaced(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, stderr := lumenTestRuntime(t)
	lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	var calls int32
	orig := lumenAdvance
	lumenAdvance = func(ctx context.Context, store *graphstore.Store, doc *ir.IR, sid string, input map[string]any, opts engine.Options) (engine.AdvanceResult, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return engine.AdvanceResult{}, graphstore.ErrRebuildRaced
		}
		return orig(ctx, store, doc, sid, input, opts)
	}
	defer func() { lumenAdvance = orig }()

	cr.lumenRunsTick(ctx) // Advance #1 → ErrRebuildRaced: must be retryable and silent.
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("advance calls after first tick = %d, want 1", got)
	}
	if strings.Contains(stderr.String(), "advancing") {
		t.Fatalf("ErrRebuildRaced took the loud branch (stderr=%q); want transient/retryable and silent", stderr.String())
	}

	// The cursor was left untouched, so the next tick re-Advances (not
	// level-trigger-suppressed) and drives the run forward.
	cr.lumenRunsTick(ctx)
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("advance calls after retry tick = %d, want 2 (retryable must not suppress the cursor)", got)
	}
}

// TestLumenRunsTickLoudRefusalOnCorruptBlob (T-B5) proves a corrupt IR blob is a
// loud per-run refusal that does NOT advance or settle the run, does NOT starve
// the other runs in the same tick, and recovers once the blob is restored.
func TestLumenRunsTickLoudRefusalOnCorruptBlob(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, stderr := lumenTestRuntime(t)

	goodStream := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
	badStream := lumenSeedRun(t, cityPath, lumenExecDoc(t, "badrun"), nil, tbHookRoute)

	// Corrupt the bad run's IR blob by swapping in a DIFFERENT valid IR — the
	// authoritative ir_hash guard in Advance's rebuild must loudly refuse it.
	badHash := lumenManifestIRHash(t, cityPath, badStream)
	if err := os.WriteFile(lumenIRBlobPath(cityPath, badHash), []byte(`{"contract":{"name":"lumen.ir","version":"0.2.5","producer":"t"},"name":"foreign","input":{"name":"main.input","fields":[],"origin":{"uri":"t","line":0,"col":0}},"origin":{"uri":"t","line":0,"col":0},"nodes":[]}`), 0o644); err != nil {
		t.Fatalf("corrupt blob: %v", err)
	}

	cr.lumenRunsTick(ctx)

	// The good run materialized; the bad run did not advance past run.started.
	if n := lumenCountJournalType(t, cityPath, goodStream, engine.EventNodeActivated); n != 1 {
		t.Fatalf("good run node.activated = %d, want 1 (other runs must still be processed)", n)
	}
	if n := lumenCountJournalType(t, cityPath, badStream, engine.EventNodeActivated); n != 0 {
		t.Fatalf("bad run node.activated = %d, want 0 (loud refusal, no divergent drive)", n)
	}
	if !strings.Contains(stderr.String(), badStream) {
		t.Fatalf("stderr did not name the refused run %q: %s", badStream, stderr.String())
	}

	// Restore the correct blob → the next tick recovers the bad run.
	_ = os.Remove(lumenIRBlobPath(cityPath, badHash))
	if err := writeLumenIRBlob(cityPath, badHash, lumenExecDoc(t, "badrun")); err != nil {
		t.Fatalf("restore blob: %v", err)
	}
	cr.lumenRunsTick(ctx)
	// The exec-only run seals in one Advance once its blob is valid.
	assertLumenRunSealed(t, cityPath, badStream)
}

// TestLumenRunsChannelAndPatrolFire (T-B6) proves the select-loop wiring: an
// injected LumenRunsCh is threaded onto cr.lumenRunsCh (the default construction is
// a non-nil cap-1 channel), and safeTick contains a panic thrown from the lumen
// tick under both the poke and patrol trigger tags.
func TestLumenRunsChannelAndPatrolFire(t *testing.T) {
	// Default construction: nil param → a fresh non-nil cap-1 channel.
	crDefault := newLumenTestCityRuntime(t, nil)
	if crDefault.lumenRunsCh == nil {
		t.Fatal("lumenRunsCh nil after default construction")
	}
	if cap(crDefault.lumenRunsCh) != 1 {
		t.Fatalf("lumenRunsCh cap = %d, want 1", cap(crDefault.lumenRunsCh))
	}

	// Injection: the provided channel is threaded through unchanged.
	injected := make(chan struct{}, 1)
	crInjected := newLumenTestCityRuntime(t, injected)
	if crInjected.lumenRunsCh != injected {
		t.Fatal("injected LumenRunsCh was not threaded onto cr.lumenRunsCh")
	}

	// safeTick contains a panic from the lumen tick body under both trigger tags.
	var stderr bytes.Buffer
	cr := &CityRuntime{logPrefix: "test", stdout: io.Discard, stderr: &stderr}
	for _, trigger := range []string{"lumen-runs", "lumen-runs-patrol"} {
		if !cr.safeTick(func() { panic("lumen tick boom") }, trigger) {
			t.Fatalf("safeTick(%q) did not report a contained panic", trigger)
		}
		if !strings.Contains(stderr.String(), trigger) {
			t.Fatalf("stderr missing trigger %q: %s", trigger, stderr.String())
		}
	}
}

// startLumenWiringRun builds a run()-capable CityRuntime (graph-unscoped — the
// lumenRunsTickFn seam stands in for the real tick) with an injected LumenRunsCh and
// patrol interval, drives cr.run in a goroutine, and returns the cancel + done
// handles. The caller passes onStarted to learn when startup finished and the select
// loop is live.
func startLumenWiringRun(t *testing.T, lumenCh chan struct{}, patrolInterval string, stderr io.Writer, onStarted func()) (context.CancelFunc, chan struct{}) {
	t.Helper()
	disableManagedDoltRecoveryForTest(t)
	stubManagedDoltStoreOpeners(t)
	cityPath := t.TempDir()
	cleanupManagedDoltTestCity(t, cityPath)
	tomlPath := filepath.Join(cityPath, "city.toml")
	writeCityRuntimeConfig(t, tomlPath, "fake")
	cfg, err := config.Load(osFS{}, tomlPath)
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	cfg.Daemon.PatrolInterval = patrolInterval
	sp := runtime.NewFake()
	cr := newTestCityRuntime(t, CityRuntimeParams{
		CityPath:    cityPath,
		CityName:    "test-city",
		TomlPath:    tomlPath,
		Cfg:         cfg,
		SP:          sp,
		LumenRunsCh: lumenCh,
		OnStarted:   onStarted,
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:              newDrainOps(sp),
		Rec:               events.Discard,
		Stdout:            io.Discard,
		Stderr:            stderr,
		ManagedDoltHealth: func(string) error { return nil },
		ManagedDoltOwned:  func(string) (bool, error) { return true, nil },
		ManagedDoltPort:   func(string) string { return "" },
	})
	cs := newControllerState(context.Background(), cfg, sp, events.NewFake(), "test-city", cityPath)
	cs.cityBeadStore = beads.NewMemStore()
	cr.setControllerState(cs)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		cr.run(ctx)
		close(done)
	}()
	return cancel, done
}

// TestLumenRunsChannelFiresDebouncedTick (F4) proves the select-case wiring
// end-to-end: a send on the injected LumenRunsCh, once cr.run is live, drives exactly
// one debounced lumen-runs tick. The seam counts firings; a long patrol keeps the
// backstop out of the way so the count is attributable solely to the channel.
func TestLumenRunsChannelFiresDebouncedTick(t *testing.T) {
	var ticks int32
	orig := lumenRunsTickFn
	lumenRunsTickFn = func(*CityRuntime, context.Context) { atomic.AddInt32(&ticks, 1) }
	defer func() { lumenRunsTickFn = orig }()

	lumenCh := make(chan struct{}, 1)
	started := make(chan struct{})
	cancel, done := startLumenWiringRun(t, lumenCh, "1h", io.Discard, func() { close(started) })
	defer func() { cancel(); <-done }()

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("run did not start within 5s")
	}
	if got := atomic.LoadInt32(&ticks); got != 0 {
		t.Fatalf("lumen tick fired %d times before any channel send, want 0", got)
	}

	lumenCh <- struct{}{} // the event-driven wake

	deadline := time.Now().Add(3 * time.Second)
	for atomic.LoadInt32(&ticks) == 0 && time.Now().Before(deadline) {
		time.Sleep(2 * time.Millisecond)
	}
	if got := atomic.LoadInt32(&ticks); got != 1 {
		t.Fatalf("lumen tick fired %d times after one channel send, want exactly 1 (select-case → debounce → tick)", got)
	}
	// No further ticks without another send (long patrol is quiescent).
	time.Sleep(80 * time.Millisecond)
	if got := atomic.LoadInt32(&ticks); got != 1 {
		t.Fatalf("lumen tick fired %d times, want a single debounced fire for a single send", got)
	}
}

// TestLumenRunsPatrolBranchFiresTick (F4) proves the patrol-branch wiring and its
// trigger tag: with a short patrol and no channel poke, the patrol branch fires the
// lumen tick through safeTick under the "lumen-runs-patrol" tag. A panicking seam
// makes the firing (and its tag) observable via safeTick's contained-panic log.
func TestLumenRunsPatrolBranchFiresTick(t *testing.T) {
	orig := lumenRunsTickFn
	lumenRunsTickFn = func(*CityRuntime, context.Context) { panic("patrol lumen boom") }
	defer func() { lumenRunsTickFn = orig }()

	var stderr bytes.Buffer
	started := make(chan struct{})
	cancel, done := startLumenWiringRun(t, nil, "5ms", &stderr, func() { close(started) })

	select {
	case <-started:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatal("run did not start within 5s")
	}
	// Give the 5ms patrol several intervals to fire the lumen backstop.
	time.Sleep(120 * time.Millisecond)
	cancel()
	<-done // stops all run-goroutine stderr writes before we read the buffer

	if !strings.Contains(stderr.String(), "trigger=lumen-runs-patrol") {
		t.Fatalf("patrol branch did not fire the lumen tick under the lumen-runs-patrol tag: %s", stderr.String())
	}
}

// TestHandleControllerConnLumenRuns (F4) proves the socket verb: a "lumen-runs" line
// signals the lumen-runs channel and acks "ok", while leaving the control-dispatcher
// and generic poke channels untouched (a dedicated verb — the generic poke runs the
// full reconcile tick, which does NOT include the lumen tick).
func TestHandleControllerConnLumenRuns(t *testing.T) {
	server, client := net.Pipe()
	defer client.Close() //nolint:errcheck

	convergenceReqCh := make(chan convergenceRequest, 1)
	pokeCh := make(chan struct{}, 1)
	controlDispatcherCh := make(chan struct{}, 1)
	lumenRunsCh := make(chan struct{}, 1)
	cityPath := t.TempDir()

	done := make(chan struct{})
	go func() {
		handleControllerConn(server, cityPath, func() {}, nil, nil, nil, convergenceReqCh, pokeCh, controlDispatcherCh, lumenRunsCh)
		close(done)
	}()

	if _, err := client.Write([]byte("lumen-runs\n")); err != nil {
		t.Fatalf("write command: %v", err)
	}
	buf := make([]byte, 16)
	n, err := client.Read(buf)
	if err != nil {
		t.Fatalf("read ack: %v", err)
	}
	if got := string(buf[:n]); got != "ok\n" {
		t.Fatalf("ack = %q, want %q", got, "ok\n")
	}

	select {
	case <-lumenRunsCh:
	default:
		t.Fatal("lumen-runs channel was not signaled")
	}
	select {
	case <-controlDispatcherCh:
		t.Fatal("control-dispatcher channel should remain untouched")
	default:
	}
	select {
	case <-pokeCh:
		t.Fatal("generic poke channel should remain untouched")
	default:
	}

	client.Close() //nolint:errcheck
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("handleControllerConn did not exit")
	}
}

// TestLumenRunLoadsInputFromCAS proves the input CAS blob round-trips through the
// loop: an input-bearing run routed to a distinct pool is loaded from
// runs/<stream>/input.json and driven WITHOUT an input_hash mismatch, and its do
// materializes routed to that pool.
func TestLumenRunLoadsInputFromCAS(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, stderr := lumenTestRuntime(t)
	input := map[string]any{"topic": "gears"}
	lumenSeedRun(t, cityPath, tbHookDoc(t), input, "workers")

	cr.lumenRunsTick(ctx)

	if strings.Contains(stderr.String(), "input hash mismatch") {
		t.Fatalf("input CAS blob did not round-trip (input_hash mismatch): %s", stderr.String())
	}
	// The do dispatched a real work bead routed to "workers".
	rows, err := cr.cityBeadStore().List(beads.ListQuery{
		Metadata:  map[string]string{beadmeta.RoutedToMetadataKey: "workers"},
		AllowScan: true,
	})
	if err != nil {
		t.Fatalf("list work beads: %v", err)
	}
	if len(rows) != 1 || rows[0].Title != "hello" {
		t.Fatalf("work beads routed(workers) = %+v, want the dispatched hello do", rows)
	}
	// The input blob is durable on disk, content-addressed by its input_hash.
	if _, err := os.Stat(lumenInputBlobPath(cityPath, engine.InputHash(input))); err != nil {
		t.Fatalf("input blob not written: %v", err)
	}
}

// TestLumenRunsTickInputBlobMissingLoudMismatch is the F1 guard-stays defense: a run
// that pinned a non-empty input_hash whose input blob is genuinely absent must NOT be
// driven — Advance's rebuild sees inputHash(nil) against the pinned hash and refuses
// loudly with ErrInputHashMismatch (the run is left untouched for diagnosis, never
// materialized against a foreign scope).
func TestLumenRunsTickInputBlobMissingLoudMismatch(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, stderr := lumenTestRuntime(t)
	input := map[string]any{"topic": "gears"}
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), input, tbHookRoute)

	// Remove the pinned input blob — the run is now discoverable but its pinned input
	// cannot be loaded.
	if err := os.Remove(lumenInputBlobPath(cityPath, engine.InputHash(input))); err != nil {
		t.Fatalf("remove input blob: %v", err)
	}

	cr.lumenRunsTick(ctx)

	if !strings.Contains(stderr.String(), "input hash mismatch") {
		t.Fatalf("stderr did not loud-fail with ErrInputHashMismatch: %s", stderr.String())
	}
	if !strings.Contains(stderr.String(), streamID) {
		t.Fatalf("stderr did not name the refused run %q: %s", streamID, stderr.String())
	}
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventNodeActivated); n != 0 {
		t.Fatalf("run advanced past run.started with a missing input blob (node.activated=%d, want 0)", n)
	}
}

// TestForcedStopDoesNotCloseLumenGraphStore is the F3 pin: the forced-stop path runs
// shutdown() on the SUPERVISOR goroutine, but the Lumen graph store is owned by the
// RUN goroutine (opened + used only inside its ticks). shutdown() must NOT close it —
// doing so is a data race + use-after-close against a run goroutine still mid-tick.
// After the fix the store is closed only by run()'s own deferred cleanup.
func TestForcedStopDoesNotCloseLumenGraphStore(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	// Make the minimal runtime survive shutdown()'s forced-stop branch: a fake
	// provider (empty ListRunning) and the force flag so it does not block on drains.
	cr.sp = runtime.NewFake()
	cr.stderr = io.Discard
	var force atomic.Bool
	force.Store(true)
	cr.forceStopShutdown = &force

	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
	cr.lumenRunsTick(ctx) // opens cr.lumen.gs on the "run goroutine"
	gs := cr.lumen.gs
	if gs == nil {
		t.Fatal("lumen graph store was not opened by the tick")
	}

	// The supervisor forced-stop path.
	cr.shutdown()

	if cr.lumen == nil || cr.lumen.gs == nil {
		t.Fatal("shutdown() closed the run goroutine's lumen store (F3: cross-goroutine use-after-close)")
	}
	// Still usable — proves it was not closed out from under the run goroutine.
	if _, err := gs.Head(ctx, streamID); err != nil {
		t.Fatalf("lumen store was closed by shutdown(): %v", err)
	}
	// The run goroutine owns the close.
	cr.closeLumenGraphStore()
}

// TestLumenRunGoroutineOwnsGraphStoreNoRace is the F3 -race proof: the run
// goroutine's use of the Lumen graph store (reading/opening cr.lumen.gs) concurrent
// with a supervisor forced-stop shutdown() must be race-free. Before the fix
// shutdown() closes+nils cr.lumen.gs while the run goroutine reads it — an
// unsynchronized field race + use-after-close of the *sql.DB; after the fix
// shutdown() never touches cr.lumen, so the store is single-goroutine-owned.
//
// Goroutine A deliberately exercises the store through lumenGraphStore()+Head() and
// NOT the full lumenRunsTick: a full tick takes shared store locks that shutdown()
// also takes, and that shared lock would inject a happens-before edge masking the
// cr.lumen.gs field race from the detector. Reading the store field directly keeps
// the race observable.
func TestLumenRunGoroutineOwnsGraphStoreNoRace(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	cr.sp = runtime.NewFake()
	cr.stderr = io.Discard
	var force atomic.Bool
	force.Store(true)
	cr.forceStopShutdown = &force
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	cr.lumenRunsTick(ctx) // open the store before the goroutines race

	// A barrier releases both goroutines together so the forced-stop overlaps the run
	// goroutine's store use (a bare `go f(); shutdown()` lets shutdown finish before
	// the run goroutine is even scheduled, hiding the race).
	barrier := make(chan struct{})
	runDone := make(chan struct{})
	shutDone := make(chan struct{})
	// The "run goroutine": a burst that repeatedly reads + uses cr.lumen.gs, then
	// closes it on ITS OWN goroutine (run()'s deferred ownership).
	go func() {
		defer close(runDone)
		<-barrier
		for i := 0; i < 20000; i++ {
			if gs := cr.lumenGraphStore(ctx); gs != nil {
				_, _ = gs.Head(ctx, streamID)
			}
		}
		cr.closeLumenGraphStore()
	}()
	// The supervisor forced-stop path, concurrent with the run goroutine's store use.
	go func() {
		defer close(shutDone)
		<-barrier
		cr.shutdown()
	}()
	close(barrier)
	<-shutDone
	<-runDone
}

// --- helpers ---------------------------------------------------------------

// lumenExecDoc builds a valid exec-only IR that seals in one Advance (no pool
// work), used as the "recovers on restore" run in the loud-refusal test.
func lumenExecDoc(t *testing.T, name string) *ir.IR {
	t.Helper()
	doc := `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "` + name + `",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [
           {"kind": "exec", "id": "step", "name": "step", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "interpreter": {"kind": "shell", "program": {"kind": "exec"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "echo ok", "language": "bash", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "exitMap": {"pass": [0], "retryable": []}}
         ]}
      ]
    }`
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode exec IR: %v", err)
	}
	return d
}

// lumenManifestIRHash reads the run's manifest and returns the pinned ir_hash.
func lumenManifestIRHash(t *testing.T, cityPath, streamID string) string {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	m, err := engine.ReadRunManifest(context.Background(), gs, streamID)
	if err != nil {
		t.Fatalf("read manifest: %v", err)
	}
	return m.IRHash
}

// lumenCountJournalType counts events of typ in streamID via a fresh handle.
func lumenCountJournalType(t *testing.T, cityPath, streamID, typ string) int {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	var n int
	if err := gs.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM journal WHERE stream_id = ? AND type = ?`, streamID, typ).Scan(&n); err != nil {
		t.Fatalf("count %s: %v", typ, err)
	}
	return n
}

// assertLumenRunSealed asserts streamID is no longer an open run (run.closed
// folded the root out of the open set).
func assertLumenRunSealed(t *testing.T, cityPath, streamID string) {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	runs, err := engine.ListOpenRuns(context.Background(), gs)
	if err != nil {
		t.Fatalf("list open runs: %v", err)
	}
	for _, r := range runs {
		if r.StreamID == streamID {
			t.Fatalf("run %q still open after seal", streamID)
		}
	}
	if n := lumenCountJournalType(t, cityPath, streamID, engine.EventRunClosed); n != 1 {
		t.Fatalf("run.closed count = %d, want 1 (sealed)", n)
	}
}

// newLumenTestCityRuntime constructs a CityRuntime through newCityRuntime (with the
// managed-dolt openers stubbed) to exercise the LumenRunsCh construction arm.
func newLumenTestCityRuntime(t *testing.T, lumenRunsCh chan struct{}) *CityRuntime {
	t.Helper()
	t.Setenv("GC_BEADS", "bd")
	disableManagedDoltRecoveryForTest(t)
	stubManagedDoltStoreOpeners(t)
	cityPath := t.TempDir()
	cleanupManagedDoltTestCity(t, cityPath)
	sp := runtime.NewFake()
	return newTestCityRuntime(t, CityRuntimeParams{
		CityPath:          cityPath,
		CityName:          "test-city",
		Cfg:               &config.City{},
		SP:                sp,
		LumenRunsCh:       lumenRunsCh,
		ManagedDoltHealth: func(string) error { return nil },
		ManagedDoltOwned:  func(string) (bool, error) { return true, nil },
		ManagedDoltPort:   func(string) string { return "" },
		BuildFn: func(*config.City, runtime.Provider, beads.Store) DesiredStateResult {
			return DesiredStateResult{State: map[string]TemplateParams{}}
		},
		Dops:   newDrainOps(sp),
		Rec:    events.Discard,
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
}

// runResolvedEvents returns the recorded run.resolved events, decoding each
// payload THROUGH the real events registry (not a raw json.Unmarshal) so the test
// also pins that RunResolved is registered with the RunResolvedPayload type — a
// wrong RegisterPayload sample would fail the type assertion here.
func runResolvedEvents(t *testing.T, rec *memRecorder) []struct {
	Envelope events.Event
	Payload  api.RunResolvedPayload
} {
	t.Helper()
	rec.mu.Lock()
	defer rec.mu.Unlock()
	var out []struct {
		Envelope events.Event
		Payload  api.RunResolvedPayload
	}
	for _, e := range rec.events {
		if e.Type != events.RunResolved {
			continue
		}
		decoded, registered, err := events.DecodePayload(events.RunResolved, e.Payload)
		if err != nil {
			t.Fatalf("decode run.resolved payload: %v (raw %s)", err, string(e.Payload))
		}
		if !registered {
			t.Fatalf("run.resolved has no registered payload type")
		}
		p, ok := decoded.(api.RunResolvedPayload)
		if !ok {
			t.Fatalf("run.resolved payload decoded to %T, want api.RunResolvedPayload", decoded)
		}
		out = append(out, struct {
			Envelope events.Event
			Payload  api.RunResolvedPayload
		}{e, p})
	}
	return out
}

// lumenSeedSelfRun enqueues a v1 agent-driven run (Driver=="self") the way
// `gc lumen run --driver self` does, returning its stream id. The controller loop
// must skip it (no pool dispatch, no seal), so it is the negative fixture for the
// run.resolved emit.
func lumenSeedSelfRun(t *testing.T, cityPath string, doc *ir.IR, input map[string]any, route string) string {
	t.Helper()
	if err := writeLumenIRBlob(cityPath, engine.IRHash(doc), doc); err != nil {
		t.Fatalf("write IR blob: %v", err)
	}
	if err := writeLumenInputBlob(cityPath, engine.InputHash(input), input); err != nil {
		t.Fatalf("write input blob: %v", err)
	}
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	streamID, err := engine.EnqueueRunWithDriver(context.Background(), gs, doc, input, "test/formula@v1", route, lumenDriverSelf)
	if err != nil {
		t.Fatalf("enqueue self run: %v", err)
	}
	return streamID
}

// TestLumenRunsTickEmitsRunResolvedOnSeal is the P5-OBS.3 happy path: a
// controller-driven run that dispatches, is closed pass by an ordinary worker,
// then observed and sealed emits exactly one run.resolved carrying the run root
// and outcome — and only at the seal, not on the earlier park, and not again on a
// redundant follow-up tick over the now-delisted run.
func TestLumenRunsTickEmitsRunResolvedOnSeal(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	rec := &memRecorder{}
	cr.rec = rec
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	// Tick 1: dispatch + park. The run has not sealed, so no run.resolved yet.
	cr.lumenRunsTick(ctx)
	if got := runResolvedEvents(t, rec); len(got) != 0 {
		t.Fatalf("run.resolved before seal = %d, want 0", len(got))
	}

	lumenCloseDoBead(t, cr.cityBeadStore(), streamID, "hello", beadmeta.OutcomePass)

	// Tick 2: observe the close → outcome.settled → seal → emit.
	cr.lumenRunsTick(ctx)
	assertLumenRunSealed(t, cityPath, streamID)

	got := runResolvedEvents(t, rec)
	if len(got) != 1 {
		t.Fatalf("run.resolved after seal = %d, want exactly 1", len(got))
	}
	e := got[0]
	if e.Envelope.Subject != streamID {
		t.Errorf("envelope Subject = %q, want stream id %q", e.Envelope.Subject, streamID)
	}
	if e.Envelope.RunID != streamID {
		t.Errorf("envelope RunID = %q, want stream id %q", e.Envelope.RunID, streamID)
	}
	if e.Envelope.Actor == "" {
		t.Errorf("envelope Actor is empty, want eventActor()")
	}
	if e.Payload.RootID != streamID {
		t.Errorf("payload RootID = %q, want %q", e.Payload.RootID, streamID)
	}
	if e.Payload.Outcome != beadmeta.OutcomePass {
		t.Errorf("payload Outcome = %q, want %q", e.Payload.Outcome, beadmeta.OutcomePass)
	}
	if e.Payload.Ts.IsZero() {
		t.Errorf("payload Ts is zero, want a resolution timestamp")
	}

	// A redundant tick over the sealed (delisted) run does not re-enter the seal
	// arm: the common path emits exactly once.
	cr.lumenRunsTick(ctx)
	if got := runResolvedEvents(t, rec); len(got) != 1 {
		t.Fatalf("run.resolved after redundant tick = %d, want still 1", len(got))
	}
}

// TestLumenRunResolvedCarriesNonPassOutcome pins that the emitted outcome is read
// from the run's aggregated result, not hard-coded to pass: a run whose single do
// closes degraded seals with outcome "degraded" and the payload reflects it.
// (degraded is used rather than fail because a bare gc.outcome=fail is a RETRYABLE
// close that can re-dispatch rather than seal; degraded is a terminal non-pass.)
func TestLumenRunResolvedCarriesNonPassOutcome(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	rec := &memRecorder{}
	cr.rec = rec
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	cr.lumenRunsTick(ctx)
	lumenCloseDoBead(t, cr.cityBeadStore(), streamID, "hello", beadmeta.OutcomeDegraded)
	cr.lumenRunsTick(ctx)
	assertLumenRunSealed(t, cityPath, streamID)

	got := runResolvedEvents(t, rec)
	if len(got) != 1 {
		t.Fatalf("run.resolved = %d, want 1", len(got))
	}
	if got[0].Payload.Outcome != engine.OutcomeDegraded {
		t.Errorf("payload Outcome = %q, want %q", got[0].Payload.Outcome, engine.OutcomeDegraded)
	}
}

// TestLumenRunResolvedAtLeastOnceOnAlreadySealedArm proves the at-least-once
// contract: driving advanceLumenRun on an already-sealed stream (the crash-recovery
// / concurrent-controller shape where the run is re-observed at seal via Advance's
// already-sealed arm) re-emits run.resolved. Consumers key on root_id and tolerate
// the redelivery; the recovery path is how a seal-emit lost to a crash is regained.
func TestLumenRunResolvedAtLeastOnceOnAlreadySealedArm(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)
	cr.lumenRunsTick(ctx)
	lumenCloseDoBead(t, cr.cityBeadStore(), streamID, "hello", beadmeta.OutcomePass)
	cr.lumenRunsTick(ctx)
	assertLumenRunSealed(t, cityPath, streamID)
	if cr.lumen != nil && cr.lumen.gs != nil {
		_ = cr.lumen.gs.Close()
	}

	// Fresh controller (empty cursors) re-observes the already-sealed run directly,
	// modeling the H1 crash window / a second concurrent controller. The Advance
	// already-sealed arm returns Sealed with the run result, so the emit fires again.
	cr2, _, _ := lumenTestRuntime(t)
	cr2.cityPath = cityPath
	cr2.standaloneCityStore = cr.standaloneCityStore
	rec2 := &memRecorder{}
	cr2.rec = rec2
	gs2 := cr2.lumenGraphStore(ctx)
	if gs2 == nil {
		t.Fatal("re-open graph store returned nil")
	}
	cr2.advanceLumenRun(ctx, gs2, engine.OpenRun{RootID: streamID, StreamID: streamID})

	got := runResolvedEvents(t, rec2)
	if len(got) != 1 {
		t.Fatalf("run.resolved on already-sealed re-observe = %d, want 1 (at-least-once recovery)", len(got))
	}
	if got[0].Payload.RootID != streamID || got[0].Payload.Outcome != beadmeta.OutcomePass {
		t.Errorf("payload = %+v, want RootID=%q Outcome=pass", got[0].Payload, streamID)
	}
}

// TestLumenRunResolvedNotEmittedForSelfDriven pins the scope: a v1 agent-driven run
// (Driver=="self") is skipped by the controller, so no work is dispatched, the run
// never seals under the controller, and no run.resolved is emitted — v1 emission is
// a separate, deferred path. The run is seeded with a VALID pool route so the ONLY
// reason nothing is dispatched is the skip (a missing route would also dispatch
// nothing, but for a different reason); removing the skip would dispatch a work bead
// here, so the zero-dispatch assertion actually pins the skip.
func TestLumenRunResolvedNotEmittedForSelfDriven(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	rec := &memRecorder{}
	cr.rec = rec
	streamID := lumenSeedSelfRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	cr.lumenRunsTick(ctx)
	cr.lumenRunsTick(ctx)

	// The skip records the head and leaves the run to its agent: zero work beads are
	// dispatched for the stream. Removing the Driver=="self" early-return would
	// dispatch the do here (valid route), so this assertion kills that mutant.
	rows, err := cr.cityBeadStore().List(beads.ListQuery{
		Metadata:      map[string]string{beadmeta.LumenRunMetadataKey: streamID},
		IncludeClosed: true,
		AllowScan:     true,
	})
	if err != nil {
		t.Fatalf("list work beads: %v", err)
	}
	if len(rows) != 0 {
		t.Fatalf("work beads dispatched for self-driven run = %d, want 0 (controller skips v1)", len(rows))
	}
	if got := runResolvedEvents(t, rec); len(got) != 0 {
		t.Fatalf("run.resolved for self-driven run = %d, want 0 (controller skips v1)", len(got))
	}
}

// TestLumenRunResolvedEmitNilRecorderSafe pins the never-fail guard: sealing a run
// when the CityRuntime has no recorder wired (cr.rec == nil) must not panic — the
// emit is best-effort and a nil recorder is a silent no-op.
func TestLumenRunResolvedEmitNilRecorderSafe(t *testing.T) {
	ctx := context.Background()
	cr, cityPath, _ := lumenTestRuntime(t)
	cr.rec = nil // no recorder wired
	streamID := lumenSeedRun(t, cityPath, tbHookDoc(t), nil, tbHookRoute)

	cr.lumenRunsTick(ctx)
	lumenCloseDoBead(t, cr.cityBeadStore(), streamID, "hello", beadmeta.OutcomePass)
	cr.lumenRunsTick(ctx) // seal → emit with nil recorder must not panic
	assertLumenRunSealed(t, cityPath, streamID)
}
