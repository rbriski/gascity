package main

import (
	"context"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// lumenAdvance is engine.Advance behind a package var so tests can count driver
// passes (the level-trigger proof) and inject transient errors. Production always
// runs the real driver.
var lumenAdvance = engine.Advance

// lumenRunsTickFn is the controller loop's lumen-runs tick behind a package var so
// the controller-loop wiring test can observe select-case and patrol-branch firings
// (and their safeTick trigger tags) end-to-end through a running cr.run, without a
// graph-scoped city. Production always runs the real method.
var lumenRunsTickFn = (*CityRuntime).lumenRunsTick

// lumenRuntime is the controller loop's in-memory, single-goroutine state for
// driving Lumen runs: the lazily-opened long-lived graph store and the per-run
// level-trigger head cursors. It is touched only on the reconciler goroutine — the
// same single-threading discipline as controlDispatcherTick — so it needs no mutex.
// A controller restart drops it entirely: the first tick re-Advances every open run
// once (idempotent), which is exactly the crash-resume path.
type lumenRuntime struct {
	gs    *graphstore.Store
	heads map[string]uint64 // streamID -> journal head at last Advance (level trigger)
	// inflight tracks each run's dispatched-but-unsettled real work beads (REDESIGN
	// §2.5). A real bead's close lands in the WORK store and does NOT move the journal
	// head, so the pure head-compare level trigger would never observe it; a run with
	// in-flight work therefore always re-Advances so the observe arm can Get its beads.
	inflight map[string][]engine.PoolWork
}

// ensureLumenRuntime lazily initializes the loop's in-memory state.
func (cr *CityRuntime) ensureLumenRuntime() *lumenRuntime {
	if cr.lumen == nil {
		cr.lumen = &lumenRuntime{
			heads:    map[string]uint64{},
			inflight: map[string][]engine.PoolWork{},
		}
	}
	return cr.lumen
}

// lumenGraphStore returns the controller's long-lived Lumen graph store, opening
// it lazily on the first tick that finds the city graph-scoped. An opted-but-
// unopenable journal (a transient open error, a misconfigured postgres backend) is
// a LOUD stderr line and a nil return: the tick skips this pass and retries on the
// next, never silently degrading. The handle is closed on shutdown.
func (cr *CityRuntime) lumenGraphStore(ctx context.Context) *graphstore.Store {
	lr := cr.ensureLumenRuntime()
	if lr.gs != nil {
		return lr.gs
	}
	backend, err := loadGraphJournalBackendConfig(cr.cityPath)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: lumen runs: resolving graph backend: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		return nil
	}
	gs, err := backend.openGraphStore(ctx, cr.cityPath)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: lumen runs: opening graph store: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		return nil
	}
	lr.gs = gs
	return gs
}

// closeLumenGraphStore closes the loop's long-lived graph store handle. It MUST be
// called only on the run goroutine (from run()'s deferred cleanup) — the store is
// opened and used exclusively there, so closing it from any other goroutine (e.g. a
// forced-stop shutdown() on the supervisor goroutine) is a data race + use-after-close.
// Idempotent and nil-safe.
func (cr *CityRuntime) closeLumenGraphStore() {
	if cr.lumen != nil && cr.lumen.gs != nil {
		_ = cr.lumen.gs.Close()
		cr.lumen.gs = nil
	}
}

// lumenRunsTick is the controller's Lumen-runs loop body: discover every open run
// and re-Advance each whose journal head moved since the last pass (dispatching
// ready do work as ordinary city-store work beads, observing dispatched beads for
// closure, or sealing). It runs on the reconciler event goroutine, wrapped in
// safeTick for panic isolation, fired by the lumen-runs poke fast-path and the
// patrol backstop. It is a pure function of (journal, IR/input CAS, work-store
// bead state): a missed poke costs at most one patrol interval, never correctness.
func (cr *CityRuntime) lumenRunsTick(ctx context.Context) {
	if !cityHasGraphScope(cr.cityPath) {
		return
	}
	gs := cr.lumenGraphStore(ctx)
	if gs == nil {
		return
	}
	runs, err := engine.ListOpenRuns(ctx, gs)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: lumen runs: listing open runs: %v\n", cr.logPrefix, err) //nolint:errcheck // best-effort stderr
		return
	}
	// Per-run failures are contained: one run's loud refusal (bad blob, stall) must
	// not starve the others, so each run is advanced independently. A dispatched
	// worker that dies is recovered by gascity's ordinary orphan-release (the real
	// bead reopens and a fresh worker re-claims it) — no Lumen-side firewall.
	for _, r := range runs {
		cr.advanceLumenRun(ctx, gs, r)
	}
}

// advanceLumenRun drives one open run one parking pass, gated on the level-trigger
// head cursor. A transient error (CAS race / lease fence / busy) leaves the cursor
// untouched so the next tick retries; a loud error (bad blob, stall) is logged and
// the run is left for diagnosis; a seal drops the cursor; a park records the head
// and pokes the reconcile/demand tick so freshly-materialized claimable work is
// seen promptly.
func (cr *CityRuntime) advanceLumenRun(ctx context.Context, gs *graphstore.Store, r engine.OpenRun) {
	lr := cr.ensureLumenRuntime()
	head, err := gs.Head(ctx, r.StreamID)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: lumen runs: reading head %q: %v\n", cr.logPrefix, r.StreamID, err) //nolint:errcheck // best-effort stderr
		return
	}
	last, seen := lr.heads[r.StreamID]
	if seen && head == last && len(lr.inflight[r.StreamID]) == 0 {
		return // level trigger: nothing new since the last Advance, and no bead to observe
	}
	m, err := engine.ReadRunManifest(ctx, gs, r.StreamID)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: lumen runs: reading manifest %q: %v\n", cr.logPrefix, r.StreamID, err) //nolint:errcheck // best-effort stderr
		return
	}
	doc, input, err := loadLumenRunInputs(cr.cityPath, m)
	if err != nil {
		fmt.Fprintf(cr.stderr, "%s: lumen runs: loading inputs for %q: %v\n", cr.logPrefix, r.StreamID, err) //nolint:errcheck // best-effort stderr
		return
	}
	// Real-bead path (REDESIGN §2.5): the do's work is an ordinary work bead in the
	// city work store, created by DispatchWork and observed for closure by ObserveWork.
	workStore := cr.cityBeadStore()
	res, err := lumenAdvance(ctx, gs, doc, r.StreamID, input, engine.Options{
		PoolRouter:   lumenPoolRouter(m.DefaultRoute),
		DispatchWork: lumenDispatchWork(workStore, cr.cfg),
		ObserveWork:  lumenObserveWork(workStore),
	})
	switch {
	case isRetryableAdvanceErr(err):
		// Transient multi-writer race: the next tick (poke or patrol) re-Advances.
		// Leave head + inflight untouched so the retry is not level-trigger-suppressed.
		return
	case err != nil:
		// A loud typed refusal (ErrIRHashMismatch, ErrAdvanceStalled, ErrNoPoolRoute)
		// OR a transient observer error (§9.7): log and leave the run untouched for
		// diagnosis; do NOT auto-settle. inflight is left in place so a parked run
		// whose observe failed re-Advances next tick.
		fmt.Fprintf(cr.stderr, "%s: lumen runs: advancing %q: %v\n", cr.logPrefix, r.StreamID, err) //nolint:errcheck // best-effort stderr
		return
	case res.Sealed:
		delete(lr.heads, r.StreamID)
		delete(lr.inflight, r.StreamID)
	case res.Parked:
		lr.heads[r.StreamID] = res.Head
		lr.inflight[r.StreamID] = res.InFlight
		// Wake the demand/reconcile tick so the freshly-created work bead is picked up
		// promptly (a nil pokeCh is never ready, so this is safe in tests).
		select {
		case cr.pokeCh <- struct{}{}:
		default:
		}
	}
}

// lumenPoolRouter builds the Advance PoolRouter for a run from its default pool
// route: a do node with no agentRef routes to the run default (loud ErrNoPoolRoute
// when the default is empty — a pool-mode do with nowhere to run), and a do that
// names an agentRef routes there. ZERO role names: a route is a config-resolved
// pool template name, exactly like gc.routed_to.
func lumenPoolRouter(defaultRoute string) func(agentRef string) (string, bool) {
	return func(agentRef string) (string, bool) {
		if agentRef == "" {
			return defaultRoute, defaultRoute != ""
		}
		return agentRef, true
	}
}

// isRetryableAdvanceErr reports whether an Advance error is a transient
// multi-writer race the controller loop should retry on the next tick rather than
// surface as a terminal refusal: an expected-version CAS loss to a concurrent
// append, a lease FENCE from a driver re-acquire, a lease HELD by another concurrent
// controller instance (MEDIUM-1: the instance-unique holder makes a second driver
// wait rather than steal — the holder releases on park/seal, so the next tick
// proceeds), a busy store, or a Tier-A rebuild that raced a concurrent append. This
// mirrors engine isRetryableRaceErr so the same race is classified identically at
// every layer. Retry is safe: Advance is a re-entrant parking driver.
func isRetryableAdvanceErr(err error) bool {
	return err != nil && (errors.Is(err, graphstore.ErrWrongExpectedVersion) ||
		errors.Is(err, graphstore.ErrLeaseFenced) ||
		errors.Is(err, graphstore.ErrLeaseHeld) ||
		errors.Is(err, graphstore.ErrBusy) ||
		errors.Is(err, graphstore.ErrRebuildRaced))
}
