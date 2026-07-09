package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// errTierBDivergentReclose is the loud, no-launder result when a settle loses the head
// CAS to a CONCURRENT settle that committed a DIFFERENT outcome — e.g. the firewall
// stranded the row `failed` while this worker's `pass` close was in flight. It mirrors
// interceptTierBClose's first-resolve divergent-reclose refusal (a divergent re-close is
// never laundered into success), so the retry loop that chases a lost settle CAS cannot
// silently adopt a firewall strand as a worker pass.
var errTierBDivergentReclose = errors.New("divergent Tier-B re-close")

// errTierBAttemptSuperseded is the loud, no-launder result when the settle retry loop's
// bare-id re-resolve returns a DIFFERENT activation than the one this close pinned — the
// firewall stranded the pinned attempt :N and the same-tick re-Advance minted a fresh
// attempt :N+1 while this stale closer was retrying. Re-settling :N+1 with the stale
// outcome (an empty-closer bypass would slip past both guard axes) would falsely settle
// the fresh attempt; instead the loop refuses loudly and the fresh attempt is left for a
// new worker to claim and complete (M-1).
var errTierBAttemptSuperseded = errors.New("Tier-B attempt superseded by a fresh re-attempt")

// settleTierBWorkAs is the engine settle seam interceptTierBClose routes through, a
// package var so a test can inject a lease fence to exercise the bounded retry. It
// carries the closer identity so the engine's closer-identity guard can reject a
// zombie (non-claimant) close.
var settleTierBWorkAs = engine.SettleTierBWorkAs

// openTierBWriteStore opens the write-capable journal graph store a Tier-B settle
// needs (SettleTierBWork takes a *graphstore.Store, which the read-only beads
// claim surface deliberately does not expose). It is a package var so a test can
// assert that an ordinary (non-fold) close never reaches it — the classification
// that decides whether to open it runs first, on the cached read-only handle.
var openTierBWriteStore = func(ctx context.Context, cityPath string) (*graphstore.Store, error) {
	backend, err := loadGraphJournalBackendConfig(cityPath)
	if err != nil {
		return nil, err
	}
	return backend.openGraphStore(ctx, cityPath)
}

// interceptTierBClose translates a `gc bd close` / `gc bd update --status=closed`
// of a fold-owned pool work bead into an engine owned.settled append. It returns
// (code, handled): handled=false means "not ours — fall through to real bd
// byte-identically" (not a close, no graph scope, or no fold-owned pool target);
// handled=true means the close was fully serviced (code 0) or loudly refused
// (code non-zero).
//
// It runs BEFORE the bd provider check and the ADR-0009 work-record gate in doBd:
// a graph-scoped city's work-store provider may be non-bd (or absent), and real bd
// can never see the journal anyway. The provider check / work-record gate open the
// WORK store, which cannot see fold-owned journal rows, so they never fire on a
// handled Tier-B close and stay byte-identical on the fall-through path.
//
// Outcome mapping is the S7 firewall (LumenOutcomeForGCOutcome): a bare close maps
// to failed, unknown values fail-closed, only pass/fail/degraded pass. A settled
// target is deduped at the PROJECTION level (§2): the settled row dropped its
// activation, so SettleTierBWork is unreachable — a same-outcome re-close is
// idempotent success, a divergent one a loud error.
func interceptTierBClose(cityPath string, bdArgs []string, _, stderr io.Writer) (int, bool) {
	if !cityHasGraphScope(cityPath) {
		return 0, false
	}
	targets, ok := workRecordCloseTargets(bdArgs)
	if !ok {
		return 0, false
	}
	ctx := context.Background()

	// Classify targets through the CACHED read-only journal handle FIRST, so an
	// ordinary work-bead close never opens a separate write-capable store (which
	// would spin up a second connection pool, run migrate, and take the journal
	// writer lock for a seedCityID INSERT — contending with the controller on
	// every routine close). The write store opens only once at least one target
	// is confirmed fold-owned pool work. (Accepted residual: cachedCityGraphJournal
	// itself runs openGraphStore/seedCityID once per process on first open; that is
	// a graphstore read-open concern, not this shim's — do not fix it here.)
	store := cachedCityGraphJournal(cityPath)
	if store == nil {
		return 0, false // cannot classify (no scope / transient open failure) — fall through
	}
	surface, ok := beads.TierBClaimSurfaceStoreFor(store)
	if !ok {
		return 0, false
	}
	var poolIDs []string
	others := 0
	for _, id := range targets {
		bead, found, rerr := surface.FoldOwnedGet(ctx, id)
		if rerr != nil {
			return 0, false // cannot classify — fall through to real bd
		}
		if found && bead.Metadata[engine.DispatchModeMetaKey] == engine.DispatchModePool {
			poolIDs = append(poolIDs, id)
		} else {
			others++
		}
	}
	if len(poolIDs) == 0 {
		return 0, false // no fold-owned pool target: byte-identical fall-through, no write store opened
	}
	if others > 0 {
		fmt.Fprintf(stderr, "gc bd: refusing a close that mixes Tier-B journal beads with ordinary beads %v; close them separately\n", targets) //nolint:errcheck
		return 1, true
	}

	// At least one fold-owned pool target: NOW open the write-capable store the
	// settle needs and resolve each target's full journal coordinates through it.
	gs, err := openTierBWriteStore(ctx, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: opening Tier-B journal for settle: %v\n", err) //nolint:errcheck
		return 1, true
	}
	defer func() { _ = gs.Close() }()

	rawOutcome, extras := tierBCloseMetadata(bdArgs)
	if len(extras) > 0 {
		fmt.Fprintf(stderr, "gc bd: Tier-B close does not persist extra metadata %v (only gc.outcome maps to the settle outcome)\n", extras) //nolint:errcheck
	}
	mapped := engine.LumenOutcomeForGCOutcome(rawOutcome)

	// The closer identity is the settling worker's session, derived from the same
	// vocabulary the claim used (GC_SESSION_NAME/ID/ALIAS, cf. cmd_hook.go). A worker
	// that claimed the current attempt matches its assignee and settles cleanly; a
	// straggler from a prior attempt is a non-claimant and loses loudly (S16). An
	// EMPTY identity (a human operator's `gc bd close` outside any session) leaves the
	// guard unengaged — attribution is best-effort transport integrity, not authz.
	closer := firstNonEmptyHookValue(
		strings.TrimSpace(os.Getenv("GC_SESSION_NAME")),
		strings.TrimSpace(os.Getenv("GC_SESSION_ID")),
		strings.TrimSpace(os.Getenv("GC_ALIAS")),
	)
	// The instance-unique guard field is the session bead id (GC_SESSION_ID) — the
	// closer's per-instance identity the claim recorded as claimant_id. It distinguishes
	// a false-killed A's straggler close from its same-named respawn B's live claim
	// (§4.3), which the NAME (shared under a singleton pool identity) cannot. Empty
	// outside a session, exactly like closer.
	closerID := strings.TrimSpace(os.Getenv("GC_SESSION_ID"))

	for _, id := range poolIDs {
		ref, found, rerr := engine.ResolveTierBWorkRef(ctx, gs, id)
		if rerr != nil {
			fmt.Fprintf(stderr, "gc bd: resolving Tier-B bead %q: %v\n", id, rerr) //nolint:errcheck
			return 1, true
		}
		if !found || ref.DispatchMode != engine.DispatchModePool {
			// The read handle classified this as fold-owned pool; a write-handle miss
			// means the row changed under us (no concurrent driver in L1). Refuse
			// loudly rather than silently drop the close.
			fmt.Fprintf(stderr, "gc bd: Tier-B bead %q no longer resolves as fold-owned pool work\n", id) //nolint:errcheck
			return 1, true
		}
		if ref.Settled {
			// Idempotent re-close dedup at the projection level: the settled row
			// dropped its activation, so SettleTierBWork is unreachable and must not be
			// needed. Success only when the mapped outcome matches AND the recorded settle
			// was NOT a firewall retryable strand (L-1): a worker's fail close over a
			// firewall failed{retryable:true} strand shares the outcome STRING but is a
			// divergent settle, so it must lose loudly rather than be laundered. An honest
			// worker self-replay (retryable=false) still dedupes.
			if ref.Outcome == mapped && !ref.Retryable {
				continue
			}
			fmt.Fprintf(stderr, "gc bd: bead %q already settled %q (retryable=%v); refusing a divergent re-close to %q\n", id, ref.Outcome, ref.Retryable, mapped) //nolint:errcheck
			return 1, true
		}
		if err := settleTierBWithFenceRetry(ctx, gs, ref, id, mapped, closer, closerID); err != nil {
			fmt.Fprintf(stderr, "gc bd: settling Tier-B bead %q: %v\n", id, err) //nolint:errcheck
			return 1, true
		}
	}
	// Best-effort: nudge the controller's Lumen-runs loop so the parked run re-Advances
	// promptly (the DRIVER must wake, not the general reconciler — S7). A missed poke
	// costs one patrol interval, never correctness.
	_ = pokeLumenRuns(cityPath)
	return 0, true
}

// settleTierBWithFenceRetry settles a Tier-B row, retrying a bounded number of times on
// a cooperative multi-writer race (isTierBSettleRetryable): a lease fence, a Tier-A
// rebuild race, OR a head-CAS conflict lost to a CONCURRENT close of a DIFFERENT bead on
// the same run stream (§0.2 — the multi-do close race). The write-once settle token
// makes each retry idempotent — a settle that committed but whose projection rebuild
// raced dedupes to success. A persistent race surfaces as a loud, re-runnable close
// error.
//
// The re-resolve's settled arm does NOT blanket-return nil: it dedups at the projection
// level exactly like interceptTierBClose's first resolve. A concurrent settle that won
// the CAS with the SAME outcome is idempotent success; a DIVERGENT one — the firewall
// stranding the row `failed` under this worker's `pass` close — is a loud
// errTierBDivergentReclose, never laundered into a success.
func settleTierBWithFenceRetry(ctx context.Context, gs *graphstore.Store, ref engine.TierBWorkRef, beadID, outcome, closer, closerID string) error {
	err := settleTierBWorkAs(ctx, gs, ref.StreamID, ref.Activation, outcome, "", closer, closerID, false)
	for attempt := 0; attempt < tierBSettleRetries && isTierBSettleRetryable(err); attempt++ {
		time.Sleep(tierBSettleBackoff(attempt))
		r2, ok, rerr := engine.ResolveTierBWorkRef(ctx, gs, beadID)
		if rerr != nil {
			return rerr
		}
		if !ok {
			// The row vanished under us (no concurrent driver deletes rows in L1); the
			// projection-level dedup in interceptTierBClose already covers a re-close, so
			// treat as done.
			return nil
		}
		// Activation pin (M-1): the firewall may have stranded the pinned attempt and, in
		// the SAME tick, re-Advanced the stream to mint a FRESH attempt (:N+1). The bare-id
		// re-resolve then returns that fresh, unsettled attempt. Never re-settle a DIFFERENT
		// activation with this stale outcome — the empty-closer bypass would slip past both
		// guard axes and falsely settle the fresh attempt. Refuse loudly; a new worker
		// claims and completes :N+1. (A settled r2 drops its activation to "", so this pin
		// is a no-op there and the settled arm below handles the dedup.)
		if r2.Activation != "" && r2.Activation != ref.Activation {
			return fmt.Errorf("bead %q pinned attempt %q was superseded by a fresh re-attempt %q; refusing to settle it with the stale outcome %q: %w",
				beadID, ref.Activation, r2.Activation, outcome, errTierBAttemptSuperseded)
		}
		if r2.Settled || r2.Activation == "" {
			// A concurrent settle (another worker's identical close, or the firewall) won
			// the head CAS and dropped the activation. Compare (outcome, retryable) rather
			// than blanket-laundering, exactly like interceptTierBClose's first resolve: a
			// same-outcome NON-retryable settle is idempotent success; a DIVERGENT one — a
			// firewall failed{retryable:true} strand under this worker's close, matching
			// outcome string or not — loses loudly and is never adopted as a success (L-1).
			if r2.Outcome == outcome && !r2.Retryable {
				return nil
			}
			return fmt.Errorf("bead %q already settled %q (retryable=%v) under a concurrent close; refusing a divergent re-close to %q: %w",
				beadID, r2.Outcome, r2.Retryable, outcome, errTierBDivergentReclose)
		}
		err = settleTierBWorkAs(ctx, gs, r2.StreamID, r2.Activation, outcome, "", closer, closerID, false)
	}
	return err
}

// interceptTierBShow serves a read-only `gc bd show <id>` of a fold-owned row from
// the journal projection, so a worker can inspect a Lumen bead real bd cannot see.
// handled=false falls through to real bd (not a show, no graph scope, or a
// non-fold id). It is read-only and never opens a fold row to writes.
func interceptTierBShow(cityPath string, bdArgs []string, stdout, stderr io.Writer) (int, bool) {
	if len(bdArgs) < 2 || bdArgs[0] != "show" {
		return 0, false
	}
	if !cityHasGraphScope(cityPath) {
		return 0, false
	}
	store := cachedCityGraphJournal(cityPath)
	if store == nil {
		return 0, false
	}
	surface, ok := beads.TierBClaimSurfaceStoreFor(store)
	if !ok {
		return 0, false
	}
	id := bdArgs[1]
	bead, found, err := surface.FoldOwnedGet(context.Background(), id)
	if err != nil || !found {
		return 0, false // not a fold-owned row: fall through to real bd
	}
	if tierBShowWantsJSON(bdArgs) {
		if err := writeCLIJSONLine(stdout, bead); err != nil {
			fmt.Fprintf(stderr, "gc bd show: writing JSON: %v\n", err) //nolint:errcheck
			return 1, true
		}
		return 0, true
	}
	fmt.Fprintf(stdout, "id: %s\ntitle: %s\nstatus: %s\ntype: %s\nassignee: %s\nroute: %s\ndescription: %s\n", //nolint:errcheck
		bead.ID, bead.Title, bead.Status, bead.Type, bead.Assignee,
		bead.Metadata[beadmeta.RoutedToMetadataKey], bead.Description)
	return 0, true
}

func tierBShowWantsJSON(bdArgs []string) bool {
	for _, a := range bdArgs {
		if a == "--json" {
			return true
		}
	}
	return false
}

// tierBCloseMetadata extracts the gc.outcome value a close carries and reports any
// OTHER gc.* metadata keys it sets (which a Tier-B settle cannot persist, edge 5).
// The `close` form carries no --set-metadata, so outcome is "" (bare ⇒ failed via
// the firewall).
func tierBCloseMetadata(bdArgs []string) (outcome string, extras []string) {
	for _, kv := range scanSetMetadataPairs(bdArgs) {
		k, v, ok := strings.Cut(kv, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		if k == beadmeta.OutcomeMetadataKey {
			outcome = strings.TrimSpace(v)
			continue
		}
		if strings.HasPrefix(k, "gc.") {
			extras = append(extras, k)
		}
	}
	return outcome, extras
}

// scanSetMetadataPairs returns every `key=value` a bd arg list sets via
// --set-metadata, in both the space-separated (`--set-metadata k=v`) and
// attached (`--set-metadata=k=v`) forms.
func scanSetMetadataPairs(bdArgs []string) []string {
	var pairs []string
	for i := 0; i < len(bdArgs); i++ {
		arg := bdArgs[i]
		if rest, ok := strings.CutPrefix(arg, "--set-metadata="); ok {
			pairs = append(pairs, rest)
			continue
		}
		if arg == "--set-metadata" && i+1 < len(bdArgs) {
			pairs = append(pairs, bdArgs[i+1])
			i++
		}
	}
	return pairs
}
