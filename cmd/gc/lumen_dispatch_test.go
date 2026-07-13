package main

import (
	"context"
	"errors"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

func lumenPoolCfg() *config.City {
	return &config.City{Agents: []config.Agent{{Name: "workers"}}}
}

// TestLumenDispatchSeamLookupBeforeCreate proves the idempotency discipline (REDESIGN
// §2.3/§9.1): the seam looks up an existing bead by the (run, activation) metadata
// pair BEFORE creating, so a second dispatch of the same activation returns the SAME
// id and mints no duplicate.
func TestLumenDispatchSeamLookupBeforeCreate(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	dispatch := lumenDispatchWork(store, lumenPoolCfg())
	w := engine.WorkDispatch{StreamID: "gcg-run-x", Activation: "draft:0", NodeID: "draft", Route: "workers", Prompt: "do it", Attempt: 0}

	id1, err := dispatch(ctx, w)
	if err != nil {
		t.Fatalf("first dispatch: %v", err)
	}
	id2, err := dispatch(ctx, w)
	if err != nil {
		t.Fatalf("second dispatch: %v", err)
	}
	if id1 != id2 {
		t.Fatalf("dispatch not idempotent: id1=%q id2=%q", id1, id2)
	}

	all, err := store.List(beads.ListQuery{Metadata: map[string]string{beadmeta.LumenRunMetadataKey: "gcg-run-x"}, IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 1 {
		t.Fatalf("beads created = %d, want 1 (lookup-before-create)", len(all))
	}
	b := all[0]
	if b.Type != "task" || b.Description != "do it" ||
		b.Metadata[beadmeta.RoutedToMetadataKey] != "workers" ||
		b.Metadata[beadmeta.LumenActivationMetadataKey] != "draft:0" ||
		b.Metadata[beadmeta.LumenAttemptMetadataKey] != "0" {
		t.Fatalf("bead = %+v, want task/do it/workers/draft:0/attempt 0", b)
	}
}

// TestLumenDispatchStampsPassthroughMetadata proves the ITEM B passthrough: a
// WorkDispatch carrying static routing/affinity metadata (gc.continuation_group) stamps
// it onto the minted work bead ALONGSIDE the four engine-owned routing keys — so a
// translated pack's affinity vector rides onto the real claim surface.
func TestLumenDispatchStampsPassthroughMetadata(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	dispatch := lumenDispatchWork(store, lumenPoolCfg())
	w := engine.WorkDispatch{
		StreamID: "gcg-run-m", Activation: "draft:0", NodeID: "draft", Route: "workers", Prompt: "do it", Attempt: 0,
		Metadata: map[string]string{"gc.continuation_group": "main", "gc.scope_ref": "release-7"},
	}

	id, err := dispatch(ctx, w)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	all, err := store.List(beads.ListQuery{Metadata: map[string]string{beadmeta.LumenRunMetadataKey: "gcg-run-m"}, IncludeClosed: true, AllowScan: true})
	if err != nil || len(all) != 1 {
		t.Fatalf("list = (%d beads, %v), want exactly 1", len(all), err)
	}
	b := all[0]
	if b.ID != id {
		t.Fatalf("listed bead id %q != dispatched id %q", b.ID, id)
	}
	// The passthrough keys landed.
	if got := b.Metadata["gc.continuation_group"]; got != "main" {
		t.Errorf("bead gc.continuation_group = %q, want main", got)
	}
	if got := b.Metadata["gc.scope_ref"]; got != "release-7" {
		t.Errorf("bead gc.scope_ref = %q, want release-7", got)
	}
	// The engine-owned keys are still present and correct alongside them.
	if b.Metadata[beadmeta.RoutedToMetadataKey] != "workers" ||
		b.Metadata[beadmeta.LumenActivationMetadataKey] != "draft:0" ||
		b.Metadata[beadmeta.LumenAttemptMetadataKey] != "0" {
		t.Fatalf("engine-owned keys wrong on %+v", b.Metadata)
	}
}

// TestLumenDispatchEngineKeysWinOverPassthrough proves the stamp-LAST ordering: even if
// a WorkDispatch's passthrough map carries an engine-reserved key (which decodeDoMetadata
// already refuses upstream), the dispatch seam overwrites it with the authoritative value
// — so the routing keys can never be clobbered. Dropping the stamp-last ordering turns
// this RED (the clobber mutation).
func TestLumenDispatchEngineKeysWinOverPassthrough(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	dispatch := lumenDispatchWork(store, lumenPoolCfg())
	w := engine.WorkDispatch{
		StreamID: "gcg-run-c", Activation: "draft:0", NodeID: "draft", Route: "workers", Prompt: "p", Attempt: 3,
		Metadata: map[string]string{
			beadmeta.RoutedToMetadataKey:     "evil-pool",
			beadmeta.LumenAttemptMetadataKey: "999",
			"gc.continuation_group":          "main",
		},
	}

	id, err := dispatch(ctx, w)
	if err != nil {
		t.Fatalf("dispatch: %v", err)
	}
	all, _ := store.List(beads.ListQuery{Metadata: map[string]string{beadmeta.LumenRunMetadataKey: "gcg-run-c"}, IncludeClosed: true, AllowScan: true})
	if len(all) != 1 || all[0].ID != id {
		t.Fatalf("list = %d beads, want 1 matching %q", len(all), id)
	}
	b := all[0]
	// Engine values win the collision.
	if got := b.Metadata[beadmeta.RoutedToMetadataKey]; got != "workers" {
		t.Fatalf("gc.routed_to = %q, want workers (engine key must win over a passthrough clobber)", got)
	}
	if got := b.Metadata[beadmeta.LumenAttemptMetadataKey]; got != "3" {
		t.Fatalf("gc.lumen_attempt = %q, want 3 (engine key must win)", got)
	}
	// The non-colliding passthrough key still rides.
	if got := b.Metadata["gc.continuation_group"]; got != "main" {
		t.Errorf("gc.continuation_group = %q, want main", got)
	}
}

// TestLumenDispatchSeamValidatesRoute proves a route that resolves to no pool-capable
// agent template is refused LOUD, with NO bead created (REDESIGN §3 GAP mitigation).
func TestLumenDispatchSeamValidatesRoute(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	dispatch := lumenDispatchWork(store, lumenPoolCfg())

	_, err := dispatch(ctx, engine.WorkDispatch{StreamID: "gcg-run-x", Activation: "draft:0", NodeID: "draft", Route: "nonexistent", Prompt: "p"})
	if !errors.Is(err, errLumenDispatchRoute) {
		t.Fatalf("dispatch to a non-template route = %v, want errLumenDispatchRoute", err)
	}
	all, err := store.List(beads.ListQuery{Metadata: map[string]string{beadmeta.LumenRunMetadataKey: "gcg-run-x"}, IncludeClosed: true, AllowScan: true})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(all) != 0 {
		t.Fatalf("beads created = %d, want 0 (route refused before create)", len(all))
	}
}

// TestLumenObserveSeamStatuses proves the observe seam's status mapping (REDESIGN
// §2.4): open/in_progress are not terminal; a closed bead is terminal with its
// gc.outcome mapped through the fail-closed firewall (a bare close ⇒ failed); a
// missing bead is a loud error (never an auto-fail).
func TestLumenObserveSeamStatuses(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	observe := lumenObserveWork(store)

	// open → not terminal.
	open, _ := store.Create(beads.Bead{Type: "task", Title: "n"})
	if obs, err := observe(ctx, open.ID); err != nil || obs.Terminal {
		t.Fatalf("open bead observe = (%+v, %v), want not terminal", obs, err)
	}

	// in_progress → not terminal.
	ip := "in_progress"
	if err := store.Update(open.ID, beads.UpdateOpts{Status: &ip}); err != nil {
		t.Fatalf("mark in_progress: %v", err)
	}
	if obs, err := observe(ctx, open.ID); err != nil || obs.Terminal {
		t.Fatalf("in_progress bead observe = (%+v, %v), want not terminal", obs, err)
	}

	// closed + gc.outcome=pass → terminal, pass.
	if err := store.Update(open.ID, beads.UpdateOpts{Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}}); err != nil {
		t.Fatalf("stamp outcome: %v", err)
	}
	if err := store.Close(open.ID); err != nil {
		t.Fatalf("close: %v", err)
	}
	if obs, err := observe(ctx, open.ID); err != nil || !obs.Terminal || obs.Outcome != engine.OutcomePass {
		t.Fatalf("closed pass observe = (%+v, %v), want terminal pass", obs, err)
	}

	// bare close (no gc.outcome) → terminal FAILED (fail-closed).
	bare, _ := store.Create(beads.Bead{Type: "task", Title: "bare"})
	if err := store.Close(bare.ID); err != nil {
		t.Fatalf("close bare: %v", err)
	}
	if obs, err := observe(ctx, bare.ID); err != nil || !obs.Terminal || obs.Outcome != engine.OutcomeFailed {
		t.Fatalf("bare closed observe = (%+v, %v), want terminal failed (fail-closed)", obs, err)
	}

	// missing → error.
	if _, err := observe(ctx, "does-not-exist"); err == nil {
		t.Fatal("observe of a missing bead returned nil error; want a loud error (never auto-fail)")
	}
}

// TestLumenObserveSeamOutputAndRetryable proves the observe seam's output + retry
// plumbing: a closed bead's gc.output_json is reported as WorkObservation.Output (the
// downstream {{ref}} value, HIGH-2/3), an explicit gc.outcome=fail is failed AND
// retryable (a genuine worker failure the retry arm re-attempts, §5), but a BARE close
// is failed and NON-retryable (MEDIUM-2 — a missing outcome is a definitive contract
// violation, not a transient strand, so a retry loop must not re-run possibly-complete
// work).
func TestLumenObserveSeamOutputAndRetryable(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	observe := lumenObserveWork(store)

	closeWith := func(t *testing.T, meta map[string]string) beads.Bead {
		t.Helper()
		b, err := store.Create(beads.Bead{Type: "task", Title: "n"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		if len(meta) > 0 {
			if err := store.Update(b.ID, beads.UpdateOpts{Metadata: meta}); err != nil {
				t.Fatalf("stamp: %v", err)
			}
		}
		if err := store.Close(b.ID); err != nil {
			t.Fatalf("close: %v", err)
		}
		return b
	}

	// pass + gc.output_json → terminal, pass, Output carried, NOT retryable.
	passBead := closeWith(t, map[string]string{
		beadmeta.OutcomeMetadataKey:    beadmeta.OutcomePass,
		beadmeta.OutputJSONMetadataKey: "aval",
	})
	if obs, err := observe(ctx, passBead.ID); err != nil || !obs.Terminal ||
		obs.Outcome != engine.OutcomePass || obs.Output != "aval" || obs.Retryable {
		t.Fatalf("pass+output observe = (%+v, %v), want terminal pass Output=aval retryable=false", obs, err)
	}

	// explicit gc.outcome=fail → failed AND retryable.
	failBead := closeWith(t, map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail})
	if obs, err := observe(ctx, failBead.ID); err != nil ||
		obs.Outcome != engine.OutcomeFailed || !obs.Retryable {
		t.Fatalf("gc.outcome=fail observe = (%+v, %v), want failed + retryable", obs, err)
	}

	// bare close (no gc.outcome) → failed but NON-retryable (MEDIUM-2).
	bareBead := closeWith(t, nil)
	if obs, err := observe(ctx, bareBead.ID); err != nil ||
		obs.Outcome != engine.OutcomeFailed || obs.Retryable {
		t.Fatalf("bare close observe = (%+v, %v), want failed + NON-retryable (MEDIUM-2)", obs, err)
	}
}

// TestLumenAttemptHistoryQueryable is the fresh-bead-per-attempt VISIBILITY proof
// (Julian's requirement): after a do fails attempt 0 (fresh bead, closed fail) and
// passes attempt 1 (fresh bead, closed pass), the attempt-history query surfaces BOTH
// beads — distinct ids, each with its attempt index and outcome — so a failed attempt
// is never purged, orphaned, or overwritten by the next attempt.
func TestLumenAttemptHistoryQueryable(t *testing.T) {
	ctx := context.Background()
	store := beads.NewMemStore()
	dispatch := lumenDispatchWork(store, lumenPoolCfg())

	// Attempt 0: dispatch, close FAIL.
	id0, err := dispatch(ctx, engine.WorkDispatch{StreamID: "gcg-run-r", Activation: "draft:0", NodeID: "draft", Route: "workers", Prompt: "p", Attempt: 0})
	if err != nil {
		t.Fatalf("dispatch attempt 0: %v", err)
	}
	if err := store.Update(id0, beads.UpdateOpts{Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomeFail}}); err != nil {
		t.Fatalf("stamp fail: %v", err)
	}
	if err := store.Close(id0); err != nil {
		t.Fatalf("close attempt 0: %v", err)
	}

	// Attempt 1: FRESH dispatch (new activation), close PASS.
	id1, err := dispatch(ctx, engine.WorkDispatch{StreamID: "gcg-run-r", Activation: "draft:1", NodeID: "draft", Route: "workers", Prompt: "p", Attempt: 1})
	if err != nil {
		t.Fatalf("dispatch attempt 1: %v", err)
	}
	if id1 == id0 {
		t.Fatalf("attempt 1 reused attempt 0's bead id %q; want a FRESH bead", id1)
	}
	if err := store.Update(id1, beads.UpdateOpts{Metadata: map[string]string{beadmeta.OutcomeMetadataKey: beadmeta.OutcomePass}}); err != nil {
		t.Fatalf("stamp pass: %v", err)
	}
	if err := store.Close(id1); err != nil {
		t.Fatalf("close attempt 1: %v", err)
	}

	hist, err := lumenAttemptHistory(store, "gcg-run-r", "draft")
	if err != nil {
		t.Fatalf("attempt history: %v", err)
	}
	if len(hist) != 2 {
		t.Fatalf("attempt history = %d beads, want BOTH attempts (0 and 1) queryable", len(hist))
	}
	want := []lumenAttemptRecord{
		{Attempt: 0, BeadID: id0, Status: "closed", Outcome: beadmeta.OutcomeFail},
		{Attempt: 1, BeadID: id1, Status: "closed", Outcome: beadmeta.OutcomePass},
	}
	for i, w := range want {
		if hist[i] != w {
			t.Fatalf("history[%d] = %+v, want %+v", i, hist[i], w)
		}
	}
}
