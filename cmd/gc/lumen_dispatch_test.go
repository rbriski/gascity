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
