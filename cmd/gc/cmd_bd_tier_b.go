package main

import (
	"context"
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
			// needed. Same mapped outcome ⇒ idempotent success; a divergent one is a
			// loud refusal (no new event either way).
			if ref.Outcome == mapped {
				continue
			}
			fmt.Fprintf(stderr, "gc bd: bead %q already settled %q; refusing a divergent re-close to %q\n", id, ref.Outcome, mapped) //nolint:errcheck
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

// settleTierBWithFenceRetry settles a Tier-B row, retrying a bounded number of times
// on a lease fence OR a Tier-A rebuild race (both cooperative driver⟷settle races,
// see isTierBFenceRetryable). The write-once settle token makes each retry idempotent
// — a settle that committed but whose projection rebuild raced dedupes to success. A
// persistent race surfaces as a loud, re-runnable close error.
func settleTierBWithFenceRetry(ctx context.Context, gs *graphstore.Store, ref engine.TierBWorkRef, beadID, outcome, closer, closerID string) error {
	err := settleTierBWorkAs(ctx, gs, ref.StreamID, ref.Activation, outcome, "", closer, closerID, false)
	for attempt := 0; attempt < tierBFenceRetries && isTierBFenceRetryable(err); attempt++ {
		time.Sleep(tierBFenceBackoff)
		r2, ok, rerr := engine.ResolveTierBWorkRef(ctx, gs, beadID)
		if rerr != nil {
			return rerr
		}
		if !ok || r2.Settled || r2.Activation == "" {
			// The row settled (our partial landed, or a racer won): the projection-level
			// dedup in interceptTierBClose already covers a re-close, so treat as done.
			return nil
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
