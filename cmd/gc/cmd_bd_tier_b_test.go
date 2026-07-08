package main

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// tbTierBSettledOutcome reads the projected outcome metadata of a settled
// fold-owned node.
func tbTierBSettledOutcome(t *testing.T, cityPath string) string {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	var v string
	_ = gs.DB().QueryRowContext(context.Background(),
		`SELECT value FROM node_metadata WHERE node_id = 'hello' AND key = 'outcome'`).Scan(&v)
	return v
}

// tbSeedClaimedPoolRow parks a do-only pool run and claims the "hello" row, the
// realistic state a worker closes from.
func tbSeedClaimedPoolRow(t *testing.T, cityPath string) {
	t.Helper()
	tbHookSeedParked(t, cityPath)
	gs := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(context.Background(), gs, tbHookStream, "hello:0", "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := gs.Close(); err != nil {
		t.Fatalf("close claim store: %v", err)
	}
}

func TestGcBdCloseSettlesFoldOwnedPass(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	tbSeedClaimedPoolRow(t, cityPath)

	var stdout, stderr bytes.Buffer
	code, handled := interceptTierBClose(cityPath,
		[]string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"},
		&stdout, &stderr)
	if !handled {
		t.Fatal("close of a fold-owned pool bead was not handled by the Tier-B shim")
	}
	if code != 0 {
		t.Fatalf("close code = %d, want 0; stderr=%s", code, stderr.String())
	}
	if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
		t.Fatalf("owned.settled count = %d, want 1", n)
	}
	if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomePass {
		t.Fatalf("settled outcome = %q, want pass", o)
	}
	if st := tbHookNodeStatus(t, cityPath); st != "done" {
		t.Fatalf("projected status = %q, want done", st)
	}
	// Frontier was recomputed: the settled row carries no frontier entry.
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	var inFrontier int
	if err := gs.DB().QueryRowContext(context.Background(),
		`SELECT COUNT(*) FROM frontier WHERE node_id = 'hello'`).Scan(&inFrontier); err != nil {
		t.Fatalf("frontier count: %v", err)
	}
	if inFrontier != 0 {
		t.Fatalf("settled row still in frontier (count=%d), want 0", inFrontier)
	}
}

func TestGcBdCloseFirewallMapsBareAndUnknownToFailed(t *testing.T) {
	t.Run("bare_close_maps_to_failed", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, []string{"close", "hello"}, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("bare close = (code %d, handled %v); stderr=%s", code, handled, stderr.String())
		}
		if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomeFailed {
			t.Fatalf("bare close outcome = %q, want failed", o)
		}
		if st := tbHookNodeStatus(t, cityPath); st != "failed" {
			t.Fatalf("status = %q, want failed", st)
		}
	})

	t.Run("unknown_outcome_maps_to_failed", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath,
			[]string{"update", "hello", "--set-metadata", "gc.outcome=banana", "--status", "closed"}, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("unknown-outcome close = (code %d, handled %v)", code, handled)
		}
		if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomeFailed {
			t.Fatalf("banana outcome = %q, want failed (fail-closed)", o)
		}
	})

	t.Run("degraded_passes_through", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath,
			[]string{"update", "hello", "--set-metadata", "gc.outcome=degraded", "--status", "closed"}, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("degraded close = (code %d, handled %v)", code, handled)
		}
		if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomeDegraded {
			t.Fatalf("degraded outcome = %q, want degraded (Lumen vocab passes through)", o)
		}
	})

	t.Run("extra_metadata_reported_not_persisted", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath,
			[]string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--set-metadata", "gc.failure_class=hard", "--status", "closed"},
			&stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("close with extra metadata = (code %d, handled %v)", code, handled)
		}
		if !strings.Contains(stderr.String(), "gc.failure_class") || !strings.Contains(stderr.String(), "does not persist") {
			t.Fatalf("stderr did not report the not-persisted key: %q", stderr.String())
		}
		if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomePass {
			t.Fatalf("outcome = %q, want pass (the extra key was dropped, not the outcome)", o)
		}
	})
}

func TestGcBdCloseIdempotentAndDivergentReclose(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	tbSeedClaimedPoolRow(t, cityPath)

	closeWith := func(outcome string) (int, bool, string) {
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath,
			[]string{"update", "hello", "--set-metadata", "gc.outcome=" + outcome, "--status", "closed"}, &stdout, &stderr)
		return code, handled, stderr.String()
	}

	if code, handled, _ := closeWith("pass"); !handled || code != 0 {
		t.Fatalf("first close = (code %d, handled %v)", code, handled)
	}
	if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
		t.Fatalf("owned.settled after first close = %d, want 1", n)
	}

	// Same outcome: idempotent success, no new event.
	if code, handled, _ := closeWith("pass"); !handled || code != 0 {
		t.Fatalf("idempotent re-close = (code %d, handled %v)", code, handled)
	}
	if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
		t.Fatalf("owned.settled after idempotent re-close = %d, want 1 (no new event)", n)
	}

	// Divergent outcome: loud failure, no new event.
	code, handled, stderr := closeWith("fail")
	if !handled || code == 0 {
		t.Fatalf("divergent re-close = (code %d, handled %v), want a loud non-zero", code, handled)
	}
	if !strings.Contains(stderr, "divergent") {
		t.Fatalf("divergent re-close stderr = %q, want a divergent-reclose refusal", stderr)
	}
	if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
		t.Fatalf("owned.settled after divergent re-close = %d, want 1 (no new event)", n)
	}
	if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomePass {
		t.Fatalf("outcome after divergent re-close = %q, want pass (first settle stands)", o)
	}
}

func TestGcBdClosePassthroughUntouchedForNonFoldTargets(t *testing.T) {
	// A graph-scoped city, but the close target is not a fold-owned pool bead.
	graphCity := tbHookGraphCity(t)
	tbHookSeedParked(t, graphCity)
	var stdout, stderr bytes.Buffer
	if code, handled := interceptTierBClose(graphCity, []string{"close", "not-a-fold-bead"}, &stdout, &stderr); handled {
		t.Fatalf("close of a non-fold id was handled (code %d); want fall-through to real bd", code)
	}

	// A non-scoped city: the intercept must never fire, so the real-bd path
	// (provider check + ADR-0009 gate + exec) runs untouched.
	plainCity := t.TempDir()
	if code, handled := interceptTierBClose(plainCity, []string{"close", "anything"}, &stdout, &stderr); handled {
		t.Fatalf("close in a non-scoped city was handled (code %d); want fall-through", code)
	}
}

// TestGcBdCloseOrdinaryDoesNotOpenWriteStore is the P3 pin: an ordinary (non-fold)
// close in a graph-scoped city classifies via the cached read-only handle and
// falls through WITHOUT opening a write-capable graph store (a second connection
// pool + migrate + a writer-lock seedCityID INSERT that would contend with the
// controller on every routine close). A confirmed fold-owned close still opens it.
func TestGcBdCloseOrdinaryDoesNotOpenWriteStore(t *testing.T) {
	t.Run("ordinary_close_opens_no_write_store", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbHookSeedParked(t, cityPath) // a fold row exists, but the close targets a DIFFERENT ordinary id

		opens := 0
		orig := openTierBWriteStore
		openTierBWriteStore = func(ctx context.Context, cp string) (*graphstore.Store, error) {
			opens++
			return orig(ctx, cp)
		}
		defer func() { openTierBWriteStore = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, []string{"close", "not-a-fold-bead"}, &stdout, &stderr)
		if handled {
			t.Fatalf("ordinary close was handled (code %d); want fall-through to real bd", code)
		}
		if opens != 0 {
			t.Fatalf("ordinary close opened the write store %d times; want 0 (classified via the cached read handle)", opens)
		}
	})

	t.Run("fold_owned_close_opens_write_store_once", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)

		opens := 0
		orig := openTierBWriteStore
		openTierBWriteStore = func(ctx context.Context, cp string) (*graphstore.Store, error) {
			opens++
			return orig(ctx, cp)
		}
		defer func() { openTierBWriteStore = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath,
			[]string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"}, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("fold-owned close = (code %d, handled %v); stderr=%s", code, handled, stderr.String())
		}
		if opens != 1 {
			t.Fatalf("fold-owned close opened the write store %d times; want exactly 1", opens)
		}
	})
}

func TestGcBdShowServesFoldOwnedRow(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	tbHookSeedParked(t, cityPath)

	var stdout, stderr bytes.Buffer
	code, handled := interceptTierBShow(cityPath, []string{"show", "hello"}, &stdout, &stderr)
	if !handled || code != 0 {
		t.Fatalf("show of a fold-owned row = (code %d, handled %v); stderr=%s", code, handled, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "open") || !strings.Contains(out, "Say hello.") {
		t.Fatalf("show output missing id/status/description: %q", out)
	}

	// A façade (fold_owned=0) row falls through to real bd.
	if _, err := tbHookOpenStoreExec(t, cityPath,
		`INSERT INTO nodes (id, status, bead_type, created_at, fold_owned) VALUES ('facade-1', 'open', 'task', '2026-07-08T00:00:00Z', 0)`); err != nil {
		t.Fatalf("insert façade: %v", err)
	}
	stdout.Reset()
	if _, handled := interceptTierBShow(cityPath, []string{"show", "facade-1"}, &stdout, &stderr); handled {
		t.Fatalf("show of a façade row was handled; want fall-through to real bd")
	}
}

// TestGcBdShowServesFoldOwnedRowInNonBdCity is the P4 placement pin: `gc bd show
// <fold-id>` must serve the hydrated journal row in a graph-scoped city whose work
// provider is NOT bd-backed (file), which requires the show intercept to run
// BEFORE doBd's provider check — exactly where the close shim runs. Were it still
// after the check, doBd would reject the show with "only supported for bd-backed
// beads providers" before ever consulting the journal, so a fold-owned show would
// fail in a non-bd graph city while a close of the same bead succeeds.
func TestGcBdShowServesFoldOwnedRowInNonBdCity(t *testing.T) {
	ctx := context.Background()
	cityPath := tbGateCity(t) // file provider + graph scope

	gs := tbHookOpenStore(t, cityPath)
	if _, err := engine.Advance(ctx, gs, tbHookDoc(t), tbHookStream, nil, engine.Options{PoolRouter: tbHookRouter}); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if err := gs.Close(); err != nil {
		t.Fatalf("close seed store: %v", err)
	}

	var stdout, stderr bytes.Buffer
	code := doBd([]string{"--city=" + cityPath, "show", "hello"}, &stdout, &stderr)
	if code != 0 {
		t.Fatalf("gc bd show <fold-id> in a file-backed graph city = %d; want 0 (show must precede the provider check); stderr=%s", code, stderr.String())
	}
	out := stdout.String()
	if !strings.Contains(out, "hello") || !strings.Contains(out, "Say hello.") {
		t.Fatalf("show output missing the fold row's id/description: %q", out)
	}
	if strings.Contains(stderr.String(), "only supported for bd-backed") {
		t.Fatalf("provider check fired before the show intercept: %q", stderr.String())
	}
}

// tbHookOpenStoreExec runs one statement against a fresh store handle.
func tbHookOpenStoreExec(t *testing.T, cityPath, stmt string) (int64, error) {
	t.Helper()
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	res, err := gs.DB().ExecContext(context.Background(), stmt)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}
