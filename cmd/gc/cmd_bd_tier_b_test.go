package main

import (
	"bytes"
	"context"
	"fmt"
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

// TestGcBdCloseCarriesCloserIdentity (T-F2) proves the close shim derives the
// closer from the session env and refuses a non-claimant close of a live attempt
// (loud stderr, exit 1, no settle), while the real claimant settles cleanly and a
// human operator (no session env) keeps the pre-L5 behavior.
func TestGcBdCloseCarriesCloserIdentity(t *testing.T) {
	closeArgs := []string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"}

	t.Run("non_claimant_refused", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath) // claimed by worker-a
		t.Setenv("GC_SESSION_NAME", "zombie-worker")
		t.Setenv("GC_SESSION_ID", "")
		t.Setenv("GC_ALIAS", "")
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code == 0 {
			t.Fatalf("non-claimant close = (code %d, handled %v); want a loud non-zero refusal; stderr=%s", code, handled, stderr.String())
		}
		if !strings.Contains(stderr.String(), "hello") {
			t.Fatalf("stderr not loud about the refused close: %q", stderr.String())
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 0 {
			t.Fatalf("owned.settled = %d, want 0 (a non-claimant close appends nothing)", n)
		}
		if st := tbHookNodeStatus(t, cityPath); st != engine.StatusClaimed {
			t.Fatalf("status after refused close = %q, want in_progress (unchanged)", st)
		}
	})

	t.Run("claimant_settles", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath) // claimed by worker-a
		t.Setenv("GC_SESSION_NAME", "worker-a")
		t.Setenv("GC_SESSION_ID", "")
		t.Setenv("GC_ALIAS", "")
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("claimant close = (code %d, handled %v); want (0,true); stderr=%s", code, handled, stderr.String())
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
			t.Fatalf("owned.settled = %d, want 1", n)
		}
	})

	t.Run("operator_no_env_unchanged", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		t.Setenv("GC_SESSION_NAME", "")
		t.Setenv("GC_SESSION_ID", "")
		t.Setenv("GC_ALIAS", "")
		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("operator close = (code %d, handled %v); want (0,true) pre-L5; stderr=%s", code, handled, stderr.String())
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
			t.Fatalf("owned.settled = %d, want 1 (operator path unchanged)", n)
		}
	})
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

// TestSettleLeaseFencedRetries (T-F2) pins the S17 settle fence mapping: a single
// lease fence is retried and the settle lands (close succeeds); a persistent fence
// surfaces as a loud, re-runnable close error.
func TestSettleLeaseFencedRetries(t *testing.T) {
	closeArgs := []string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"}

	t.Run("fence_once_retries_success", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		calls := 0
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(ctx context.Context, gs *graphstore.Store, streamID, activation, outcome, output, closer, closerID string, retryable bool) error {
			calls++
			if calls == 1 {
				return graphstore.ErrLeaseFenced
			}
			return engine.SettleTierBWorkAs(ctx, gs, streamID, activation, outcome, output, closer, closerID, retryable)
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("fence-once close = (code=%d handled=%v); want (0,true); stderr=%s", code, handled, stderr.String())
		}
		if calls < 2 {
			t.Fatalf("settle calls = %d, want >= 2 (a fence must retry)", calls)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
			t.Fatalf("owned.settled = %d, want 1 (retry settled exactly once, idempotent token)", n)
		}
	})

	t.Run("fence_persistent_loud", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(context.Context, *graphstore.Store, string, string, string, string, string, string, bool) error {
			return graphstore.ErrLeaseFenced
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code == 0 {
			t.Fatalf("persistent fence close = (code=%d handled=%v); want a loud non-zero", code, handled)
		}
	})
}

// TestSettleRebuildRacedRetries (F2) pins ErrRebuildRaced on the settle path exactly
// like a lease fence: a concurrent driver append that raced the settle's Tier-A
// projection rebuild is a transient multi-writer race, retried under the write-once
// settle token (idempotent), NOT a hard close failure. A single race retries and the
// settle lands (close succeeds); a persistent race surfaces as a loud, re-runnable
// close error — never a spurious non-zero close for a settle that raced.
func TestSettleRebuildRacedRetries(t *testing.T) {
	closeArgs := []string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"}

	t.Run("race_once_retries_success", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		calls := 0
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(ctx context.Context, gs *graphstore.Store, streamID, activation, outcome, output, closer, closerID string, retryable bool) error {
			calls++
			if calls == 1 {
				return graphstore.ErrRebuildRaced
			}
			return engine.SettleTierBWorkAs(ctx, gs, streamID, activation, outcome, output, closer, closerID, retryable)
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("race-once close = (code=%d handled=%v); want (0,true); stderr=%s", code, handled, stderr.String())
		}
		if calls < 2 {
			t.Fatalf("settle calls = %d, want >= 2 (a rebuild race must retry)", calls)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
			t.Fatalf("owned.settled = %d, want 1 (retry settled exactly once, idem token)", n)
		}
	})

	t.Run("race_persistent_loud", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(context.Context, *graphstore.Store, string, string, string, string, string, string, bool) error {
			return graphstore.ErrRebuildRaced
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code == 0 {
			t.Fatalf("persistent rebuild-race close = (code=%d handled=%v); want a loud non-zero", code, handled)
		}
	})
}

// TestGcBdCloseRetriesSettleHeadCASLoss (T-U1, §0.2) pins the L4 fix: a worker close
// whose settle loses the head CAS to a CONCURRENT close of a DIFFERENT bead (mapped to
// engine.ErrTierBClaimConflict) is CHASED by the close shim's bounded retry, not surfaced
// as a hard exit 1 that would strand the run for the 60s firewall grace. The re-resolve's
// settled arm compares outcomes: a same-outcome concurrent settle is idempotent success;
// a DIVERGENT one (a firewall strand under a worker pass) loses loudly and is never
// laundered. Subtest (a) FAILS on HEAD (the pre-fix loop chased only fence/rebuild races).
func TestGcBdCloseRetriesSettleHeadCASLoss(t *testing.T) {
	closeArgs := []string{"update", "hello", "--set-metadata", "gc.outcome=pass", "--status", "closed"}

	// wrapConflict models the engine's wrapping of a lost head-CAS: the underlying
	// graphstore.ErrWrongExpectedVersion is dropped, ErrTierBClaimConflict is the chain.
	wrapConflict := func() error {
		return fmt.Errorf("lumen tier-b: settle of %q lost the race: %w", "hello", engine.ErrTierBClaimConflict)
	}

	t.Run("conflict_once_then_retry_succeeds", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		t.Setenv("GC_SESSION_NAME", "worker-a") // matches the seeded claimant
		t.Setenv("GC_SESSION_ID", "")
		t.Setenv("GC_ALIAS", "")
		calls := 0
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(ctx context.Context, gs *graphstore.Store, streamID, activation, outcome, output, closer, closerID string, retryable bool) error {
			calls++
			if calls == 1 {
				return wrapConflict()
			}
			return engine.SettleTierBWorkAs(ctx, gs, streamID, activation, outcome, output, closer, closerID, retryable)
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("conflict-once close = (code=%d handled=%v); want (0,true) — the settle CAS loss must be retried; stderr=%s", code, handled, stderr.String())
		}
		if calls != 2 {
			t.Fatalf("settle calls = %d, want exactly 2 (one conflict, one successful retry)", calls)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
			t.Fatalf("owned.settled = %d, want 1 (retry settled exactly once)", n)
		}
		if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomePass {
			t.Fatalf("settled outcome = %q, want pass", o)
		}
	})

	t.Run("conflict_then_settled_same_outcome_idempotent", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		t.Setenv("GC_SESSION_NAME", "worker-a")
		t.Setenv("GC_SESSION_ID", "")
		t.Setenv("GC_ALIAS", "")
		calls := 0
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(ctx context.Context, gs *graphstore.Store, streamID, activation, outcome, output, closer, closerID string, retryable bool) error {
			calls++
			if calls == 1 {
				// A concurrent identical close won the CAS: commit the SAME outcome, then
				// report the loss so the retry loop re-resolves and finds it settled pass.
				if err := engine.SettleTierBWorkAs(ctx, gs, streamID, activation, outcome, output, closer, closerID, retryable); err != nil {
					t.Fatalf("seed concurrent-same settle: %v", err)
				}
				return wrapConflict()
			}
			t.Fatalf("settle called again (%d); want the re-resolve to dedup a same-outcome settle", calls)
			return nil
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code != 0 {
			t.Fatalf("same-outcome re-resolve = (code=%d handled=%v); want (0,true) idempotent; stderr=%s", code, handled, stderr.String())
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
			t.Fatalf("owned.settled = %d, want 1 (no second settle appended)", n)
		}
		if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomePass {
			t.Fatalf("outcome = %q, want pass", o)
		}
	})

	t.Run("conflict_then_settled_divergent_loud_no_launder", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		t.Setenv("GC_SESSION_NAME", "worker-a")
		t.Setenv("GC_SESSION_ID", "")
		t.Setenv("GC_ALIAS", "")
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(ctx context.Context, gs *graphstore.Store, streamID, activation, _, _, _, _ string, _ bool) error {
			// The firewall stranded this row `failed` (controller override: empty closer,
			// retryable) while this worker's `pass` close was in flight; then this settle
			// loses the head CAS. The retry must NOT launder the strand into a pass.
			if err := engine.SettleTierBWorkAs(ctx, gs, streamID, activation, engine.OutcomeFailed, "stranded: worker-a", "", "", true); err != nil {
				t.Fatalf("seed firewall strand: %v", err)
			}
			return wrapConflict()
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code == 0 {
			t.Fatalf("divergent re-resolve = (code=%d handled=%v); want a loud non-zero (no laundering)", code, handled)
		}
		if !strings.Contains(stderr.String(), "divergent") {
			t.Fatalf("divergent re-close stderr = %q, want a divergent-reclose refusal", stderr.String())
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 1 {
			t.Fatalf("owned.settled = %d, want 1 (the firewall strand stands; the pass was refused)", n)
		}
		if o := tbTierBSettledOutcome(t, cityPath); o != engine.OutcomeFailed {
			t.Fatalf("outcome = %q, want failed (the firewall strand is not laundered to pass)", o)
		}
	})

	t.Run("persistent_conflict_loud_after_bounded_retries", func(t *testing.T) {
		cityPath := tbHookGraphCity(t)
		tbSeedClaimedPoolRow(t, cityPath)
		t.Setenv("GC_SESSION_NAME", "worker-a")
		t.Setenv("GC_SESSION_ID", "")
		t.Setenv("GC_ALIAS", "")
		calls := 0
		orig := settleTierBWorkAs
		settleTierBWorkAs = func(context.Context, *graphstore.Store, string, string, string, string, string, string, bool) error {
			calls++
			return wrapConflict()
		}
		defer func() { settleTierBWorkAs = orig }()

		var stdout, stderr bytes.Buffer
		code, handled := interceptTierBClose(cityPath, closeArgs, &stdout, &stderr)
		if !handled || code == 0 {
			t.Fatalf("persistent conflict close = (code=%d handled=%v); want a loud non-zero after the bounded retries", code, handled)
		}
		if calls != tierBFenceRetries+1 {
			t.Fatalf("settle calls = %d, want %d (initial + %d bounded retries)", calls, tierBFenceRetries+1, tierBFenceRetries)
		}
		if n := tbHookCountJournalType(t, cityPath, engine.EventOwnedSettled); n != 0 {
			t.Fatalf("owned.settled = %d, want 0 (nothing settled; the row stays claimed for a re-run)", n)
		}
		if st := tbHookNodeStatus(t, cityPath); st != engine.StatusClaimed {
			t.Fatalf("status after persistent conflict = %q, want in_progress (unchanged)", st)
		}
	})
}
