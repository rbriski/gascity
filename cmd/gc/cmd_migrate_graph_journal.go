package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/spf13/cobra"
)

// migrateGraphJournalConfigYAML is the activation-by-presence marker `gc migrate
// graph-journal init` writes. Its mere presence at
// <city>/.gc/graph/.beads/config.yaml opts the city into the graph-journal scope
// (cityGraphScopePresence stats exactly this path); the content is a human-legible
// provider tag, never parsed for routing.
const migrateGraphJournalConfigYAML = "provider: journal\n"

// claimActiveStatuses are the non-terminal statuses a worker-claimable bead is in
// while a worker holds it or can claim it. These mirror the two `gc hook --claim`
// branches (cmd_hook_claim.go): `in_progress` + identity is `existing_assignment`,
// and `open` + identity is `ready_assignment`. The dead `claimed`/`assigned`
// statuses the earlier draft keyed on never appear in the ledger.
var claimActiveStatuses = map[string]bool{
	"open":        true,
	"in_progress": true,
}

// migrationBlockingDepTypes are the dependency types that make a dependent wait on
// its target closing (mirrors journalReadyBlockingDepClause). An INBOUND
// cross-root dependency of one of these types means closing the migrated subtree
// (tombstone) would prematurely unblock a bead OUTSIDE the root while the journal
// copy is still open — so a root carrying one is refused.
var migrationBlockingDepTypes = map[string]bool{
	"blocks":             true,
	"waits-for":          true,
	"conditional-blocks": true,
}

// errPostFlipExternalWrite is the typed alarm raised when an external (non-router)
// write lands on the legacy subtree in the residual re-verify→flip window: after
// the flip the journal copy is authoritative but missing that write, so the
// migration REFUSES to tombstone (closing a window-created child would destroy its
// state) and leaves the legacy leg intact for operator reconciliation. It is a
// loud, recoverable alarm — never silent data loss.
var errPostFlipExternalWrite = errors.New("post-flip external write detected: legacy subtree diverged from the authoritative journal copy; legacy left un-tombstoned for reconciliation")

func newMigrateCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Substrate migration commands",
		Long: `Substrate migration commands.

Subcommands move a city between substrate generations. They are inert until
invoked and reversible at every step.`,
		Args: cobra.NoArgs,
		RunE: func(c *cobra.Command, _ []string) error {
			return c.Help()
		},
	}
	cmd.AddCommand(newMigrateGraphJournalCmd(stdout, stderr))
	return cmd
}

func newMigrateGraphJournalCmd(stdout, stderr io.Writer) *cobra.Command {
	var (
		roots        []string
		dryRun       bool
		asJSON       bool
		forceRecover bool
	)
	cmd := &cobra.Command{
		Use:   "graph-journal",
		Short: "Migrate stranded graph roots onto the journal substrate",
		Long: `Migrate stranded graph roots onto the journal substrate.

With no subcommand this is the STRAND arm: for each --root it copies the root's
subgraph from the legacy graph store into the journal store, preserving ids, and
flips residence to journal — park -> copy -> fold-verify -> re-verify -> flip ->
tombstone. Every step is idempotent and crash-safe: a re-run resumes, the legacy
copy is tombstoned (never deleted), and each durable step is CAS-guarded.

Concurrency and external writes (read before relying on the fence):
  - Only ONE migrator may hold a root. A second invocation over a root another
    migrator is already migrating REFUSES rather than stomping it. If a migration
    genuinely crashed, reclaim its stale record with --force-recover.
  - The migrating fence blocks ROUTER-routed writes (the controller path). It does
    NOT fence external 'bd' writers — they bypass the router. Such a write is
    DETECTED, not prevented: re-verify catches one up to the flip, a pre-tombstone
    delta check catches one in the residual re-verify→flip window, and a post-close
    re-hash (status-excluded) catches one that races the tombstone close loop itself.
    Any of these raises a LOUD alarm (no tombstone, non-zero exit) for manual
    reconciliation rather than losing it silently. These hashes DETECT an external
    modification to an existing bead in the window; they do not PREVENT it — true
    prevention requires quiescing external bd writers (the P4/hosted path). A NEW
    child created in the window is not the lossy case: it stays on the intact legacy
    leg and is served via legacy fan-out. Only a modification to an EXISTING bead is
    the detect-not-prevent residual.

This is for stranded roots after an incident, never the mainline cutover (that
moves zero bytes).

Run "gc migrate graph-journal init" once to opt the city in.`,
		Example: `  gc migrate graph-journal init
  gc migrate graph-journal --root gcg-42
  gc migrate graph-journal --root gcg-42 --dry-run --json
  gc migrate graph-journal --root gcg-42 --force-recover`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if len(roots) == 0 {
				return fmt.Errorf("no roots given: pass --root <id> (or run \"gc migrate graph-journal init\" to opt in)")
			}
			if doMigrateGraphJournal(roots, dryRun, asJSON, forceRecover, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().StringArrayVar(&roots, "root", nil, "root bead id to migrate (repeatable)")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "compute and report the migration without writing anything")
	cmd.Flags().BoolVar(&asJSON, "json", false, "emit a JSON summary")
	cmd.Flags().BoolVar(&forceRecover, "force-recover", false, "reclaim a root left in the migrating state by a crashed migration (epoch-guarded discard + revert, then re-attempt)")
	cmd.AddCommand(newMigrateGraphJournalInitCmd(stdout, stderr))
	return cmd
}

func newMigrateGraphJournalInitCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "init",
		Short: "Opt the city into the graph-journal scope (idempotent)",
		Long: `Opt the city into the graph-journal scope.

Creates <city>/.gc/graph/.beads/config.yaml and the journal database. Presence
alone is inert: the city keeps minting new roots on the legacy leg until the
generational cutover. Reversal is deleting the scope (no roots reside there yet).
Idempotent — safe to run repeatedly.`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doMigrateGraphJournalInit(stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// --- init arm --------------------------------------------------------------

func doMigrateGraphJournalInit(stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc migrate graph-journal init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if err := migrateGraphJournalInit(cityPath); err != nil {
		fmt.Fprintf(stderr, "gc migrate graph-journal init: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	fmt.Fprintf(stdout, "graph-journal scope active at %s\n", graphScopeRoot(cityPath)) //nolint:errcheck // best-effort stdout
	return 0
}

// migrateGraphJournalInit creates the activation-by-presence scope marker and the
// journal database for cityPath. It is idempotent: an already-opted city is a
// no-op, and the journal store open is create-if-absent.
func migrateGraphJournalInit(cityPath string) error {
	markerDir := filepath.Join(graphScopeRoot(cityPath), ".beads")
	if err := os.MkdirAll(markerDir, 0o755); err != nil {
		return fmt.Errorf("creating graph scope dir %q: %w", markerDir, err)
	}
	marker := filepath.Join(markerDir, "config.yaml")
	if _, err := os.Stat(marker); errors.Is(err, os.ErrNotExist) {
		if err := atomicWriteFile(marker, []byte(migrateGraphJournalConfigYAML)); err != nil {
			return fmt.Errorf("writing graph scope marker %q: %w", marker, err)
		}
	} else if err != nil {
		return fmt.Errorf("stating graph scope marker %q: %w", marker, err)
	}
	// Open the journal store once so journal.db is created alongside the marker;
	// the open is create-if-absent and validates the schema ladder.
	result, present, err := openCityGraphJournalResultAt(cityPath)
	if err != nil {
		return fmt.Errorf("opening journal store: %w", err)
	}
	if !present {
		return fmt.Errorf("graph scope marker written but city still reads as not opted at %q", cityPath)
	}
	// One-shot CLI open: release the freshly opened journal handle rather than leak
	// its SQLite pools for the process lifetime.
	scheduleCloseBeadStoreHandle("migrate graph-journal init store", result.Store)
	return nil
}

// --- strand arm ------------------------------------------------------------

// migrateGraphJournalSummary is the E3-style ledger of a strand migration run.
type migrateGraphJournalSummary struct {
	DryRun       bool                `json:"dry_run"`
	RootsScanned int                 `json:"roots_scanned"`
	RootsCutover int                 `json:"roots_cutover"`
	Roots        []migrateRootResult `json:"roots"`
}

// hasFailure reports whether any root reached a failure/alarm outcome, so the
// command exits non-zero even though the summary is still emitted in full.
func (s migrateGraphJournalSummary) hasFailure() bool {
	for _, r := range s.Roots {
		if migrateOutcomeIsFailure(r.Outcome) {
			return true
		}
	}
	return false
}

// migrateRootResult is the per-root outcome.
type migrateRootResult struct {
	RootID                 string   `json:"root_id"`
	Outcome                string   `json:"outcome"`
	JournalEntriesAppended int      `json:"journal_entries_appended"`
	FoldHash               string   `json:"fold_hash"`
	Tombstoned             bool     `json:"tombstoned"`
	RacedWrite             bool     `json:"raced_write"`
	Mismatches             []string `json:"mismatches,omitempty"`
}

// Migration outcomes.
const (
	migrateOutcomeCutover       = "cutover"         // moved this run
	migrateOutcomeAlready       = "already"         // journal-resident already; resumed tombstone
	migrateOutcomeRevertedRace  = "reverted-race"   // racing external write pre-flip; aborted+reverted, no tombstone
	migrateOutcomeSkipped       = "skipped"         // lost the park CAS (a competing migrator)
	migrateOutcomeDryRun        = "dry-run"         // reported only, zero writes
	migrateOutcomeRefused       = "refused"         // open worker-claimable / cross-root dependent in the subtree
	migrateOutcomeMigratingLock = "migrating-lock"  // another migrator holds the root; use --force-recover to reclaim a crashed one
	migrateOutcomePostFlipDelta = "post-flip-delta" // external write in the re-verify→flip window; loud alarm, no tombstone
	migrateOutcomeError         = "error"           // unexpected failure
)

// migrateOutcomeIsFailure reports whether an outcome must drive a non-zero exit:
// a data-safety alarm or a refusal an operator must act on, as distinct from the
// safe/converging outcomes (cutover, already, dry-run, reverted-race, skipped).
func migrateOutcomeIsFailure(outcome string) bool {
	switch outcome {
	case migrateOutcomeRefused, migrateOutcomeMigratingLock, migrateOutcomePostFlipDelta, migrateOutcomeError:
		return true
	default:
		return false
	}
}

func doMigrateGraphJournal(roots []string, dryRun, asJSON, forceRecover bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc migrate graph-journal: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	legacy, err := openStoreAtForCity(cityPath, cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc migrate graph-journal: opening legacy store: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	result, present, err := openCityGraphJournalResultAt(cityPath)
	if err != nil {
		fmt.Fprintf(stderr, "gc migrate graph-journal: opening journal store: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if !present {
		fmt.Fprintln(stderr, "gc migrate graph-journal: city not opted in; run \"gc migrate graph-journal init\" first") //nolint:errcheck // best-effort stderr
		return 1
	}
	res, ok := beads.ResidenceMigrationStoreFor(result.Store)
	if !ok {
		fmt.Fprintln(stderr, "gc migrate graph-journal: journal store lacks the residence-migration capability") //nolint:errcheck // best-effort stderr
		return 1
	}
	deps := migrateGraphJournalDeps{legacy: legacy, journal: result.Store, res: res, now: time.Now}
	summary := runMigrateGraphJournal(context.Background(), deps, roots, dryRun, forceRecover, stderr)
	exit := 0
	if summary.hasFailure() {
		exit = 1
	}
	if asJSON {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(summary); err != nil {
			fmt.Fprintf(stderr, "gc migrate graph-journal: encoding summary: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return exit
	}
	for _, r := range summary.Roots {
		fmt.Fprintf(stdout, "%s\t%s\ttombstoned=%t\n", r.RootID, r.Outcome, r.Tombstoned) //nolint:errcheck // best-effort stdout
	}
	fmt.Fprintf(stdout, "scanned=%d cutover=%d dry_run=%t\n", summary.RootsScanned, summary.RootsCutover, summary.DryRun) //nolint:errcheck // best-effort stdout
	return exit
}

// migrateGraphJournalDeps bundles the surfaces a strand migration drives. The
// legacy and journal legs are the RAW stores (never the router), so a migration
// never blocks itself on the ErrRootMigrating quarantine.
type migrateGraphJournalDeps struct {
	legacy  beads.Store
	journal beads.Store
	res     beads.ResidenceMigrationStore
	now     func() time.Time

	// hooks is a test-only crash-injection seam. Each field, when non-nil, runs
	// immediately after that step's durable write; returning an error models a
	// crash at that boundary (state stays committed, no cleanup runs), so a re-run
	// must converge. Production leaves it nil.
	hooks *migrateRootHooks
}

type migrateRootHooks struct {
	afterPark       func() error
	afterCopy       func() error
	afterFoldVerify func() error
	afterReVerify   func() error
	afterFlip       func() error
	// duringTombstoneClose fires once inside ensureTombstone, just after the close
	// loop and BEFORE the post-close re-hash alarm — modeling an external bd write
	// that raced the tombstone close loop. Production leaves it nil.
	duringTombstoneClose func() error
}

func (d migrateGraphJournalDeps) fire(fn func() error) error {
	if fn == nil {
		return nil
	}
	return fn()
}

// runMigrateGraphJournal migrates each root, accumulating an E3-style summary.
func runMigrateGraphJournal(ctx context.Context, deps migrateGraphJournalDeps, roots []string, dryRun, forceRecover bool, stderr io.Writer) migrateGraphJournalSummary {
	summary := migrateGraphJournalSummary{DryRun: dryRun, RootsScanned: len(roots)}
	for _, root := range roots {
		r, err := migrateRoot(ctx, deps, root, dryRun, forceRecover)
		if err != nil {
			r.RootID = root
			if r.Outcome == "" {
				r.Outcome = migrateOutcomeError
			}
			prefix := "gc migrate graph-journal"
			if r.Outcome == migrateOutcomePostFlipDelta {
				prefix = "gc migrate graph-journal: ALARM"
			}
			fmt.Fprintf(stderr, "%s: root %s: %v\n", prefix, root, err) //nolint:errcheck // best-effort stderr
		}
		if r.Outcome == migrateOutcomeCutover {
			summary.RootsCutover++
		}
		summary.Roots = append(summary.Roots, r)
	}
	return summary
}

// migrateRoot runs the strand-migration state machine for one root (09a §A-2).
// It is idempotent and crash-safe: it first inspects residence to resume, then
// runs park -> snapshot -> copy -> fold-verify -> re-verify -> flip -> tombstone.
// Every durable step is a CAS or an epoch-tagged write, so a crash at any boundary
// leaves a recoverable state. A root left `migrating` by a crashed run is NOT
// auto-reclaimed by a plain re-run — that could stomp a still-live sibling
// migrator — the operator passes forceRecover to reclaim it (epoch-guarded).
func migrateRoot(ctx context.Context, deps migrateGraphJournalDeps, rootID string, dryRun, forceRecover bool) (migrateRootResult, error) {
	res := deps.res
	out := migrateRootResult{RootID: rootID}

	// Step 0: inspect residence to resume or (with force) reclaim.
	state, epoch, present, err := res.ResidenceOf(ctx, rootID)
	if err != nil {
		return out, fmt.Errorf("reading residence: %w", err)
	}
	if present {
		switch state {
		case beads.ResidenceStateJournal:
			// Already flipped; a crash may have left the legacy copy un-tombstoned.
			// Resume idempotently at the tombstone step.
			if dryRun {
				out.Outcome = migrateOutcomeAlready
				return out, nil
			}
			tombstoned, err := ensureTombstone(deps, rootID)
			if err != nil {
				if errors.Is(err, errPostFlipExternalWrite) {
					out.Outcome = migrateOutcomePostFlipDelta
					out.RacedWrite = true
					return out, err
				}
				return out, fmt.Errorf("resuming tombstone: %w", err)
			}
			out.Outcome = migrateOutcomeAlready
			out.Tombstoned = tombstoned
			return out, nil
		case beads.ResidenceStateMigrating:
			if dryRun {
				out.Outcome = migrateOutcomeDryRun
				return out, nil
			}
			// This invocation did NOT mint the in-flight epoch. Refuse rather than
			// blindly discard: a sibling migrator may still be live on epoch N, and
			// stomping its staged rows would destroy the authoritative copy in flight
			// (BLOCKER-2). Only an explicit --force-recover reclaims a crashed run,
			// and even then the discard/revert are epoch-guarded to N.
			if !forceRecover {
				out.Outcome = migrateOutcomeMigratingLock
				return out, fmt.Errorf("root %q is migrating under epoch %d; if that migration crashed, re-run with --force-recover", rootID, epoch)
			}
			if err := res.DiscardRoot(ctx, rootID, epoch); err != nil {
				return out, fmt.Errorf("force-recover: discarding staged copy: %w", err)
			}
			if _, err := res.RevertResidence(ctx, rootID, epoch); err != nil {
				return out, fmt.Errorf("force-recover: reverting residence: %w", err)
			}
			// Fall through to a fresh attempt.
		}
	}

	// Step 1: collect the legacy subtree; refuse the two states migration cannot
	// safely move.
	subtree, err := collectSubtree(deps.legacy, rootID)
	if err != nil {
		return out, err
	}
	if bead := firstOpenClaimableWorker(subtree.beads); bead != "" {
		out.Outcome = migrateOutcomeRefused
		return out, fmt.Errorf("root has an open worker-claimable bead %q (assigned or routed); migrate would strand it (worker journal beads are invisible until P4)", bead)
	}
	if dependent, target, err := firstCrossRootBlockingDependent(deps.legacy, subtree.beads); err != nil {
		return out, fmt.Errorf("checking cross-root dependents: %w", err)
	} else if dependent != "" {
		out.Outcome = migrateOutcomeRefused
		return out, fmt.Errorf("bead %q outside the root has a blocking dependency on subtree bead %q; tombstoning the subtree would prematurely unblock it while the journal copy is still open — resolve the cross-root dependency first", dependent, target)
	}

	if dryRun {
		out.Outcome = migrateOutcomeDryRun
		out.JournalEntriesAppended = len(subtree.beads)
		out.FoldHash = canonicalSubtreeHash(subtree)
		return out, nil
	}

	// Step 2: park — CAS residence ∅ -> migrating(N).
	fenceEpoch := deps.now().UTC().UnixNano()
	won, err := res.BeginResidenceMigration(ctx, rootID, fenceEpoch)
	if err != nil {
		return out, fmt.Errorf("park: %w", err)
	}
	if !won {
		// Another migrator won the record between our read and the CAS.
		out.Outcome = migrateOutcomeSkipped
		return out, nil
	}
	// Best-effort epoch bump so the level-triggered dispatcher parks the root. The
	// legacy leg is written directly (never the router), so this never self-blocks;
	// re-verify is the backstop when it loses.
	_ = deps.legacy.SetMetadata(rootID, "gc.control_epoch", fmt.Sprintf("%d", fenceEpoch))
	if err := deps.fire(deps.hooks.afterParkFn()); err != nil {
		return out, err
	}

	// Step 3: snapshot the parked subtree (AFTER the epoch bump so the copy and the
	// re-verify hash the same parked state).
	parked, err := collectSubtree(deps.legacy, rootID)
	if err != nil {
		return out, fmt.Errorf("snapshot: %w", err)
	}
	snapHash := canonicalSubtreeHash(parked)
	out.FoldHash = snapHash
	out.JournalEntriesAppended = len(parked.beads)

	// Step 4: copy — import the subtree into the journal leg, ids + edge metadata
	// preserved.
	if err := res.ImportSubtree(ctx, parked.beads, parked.edgeMeta, rootID); err != nil {
		return out, fmt.Errorf("copy: %w", err)
	}
	if err := deps.fire(deps.hooks.afterCopyFn()); err != nil {
		return out, err
	}

	// Step 5: fold-verify — the staged journal copy must hash-equal the snapshot.
	stagedBeads, err := res.StagedRootBeads(ctx, rootID)
	if err != nil {
		return out, fmt.Errorf("fold-verify read: %w", err)
	}
	staged, err := subtreeWithEdgeMeta(deps.journal, stagedBeads)
	if err != nil {
		return out, fmt.Errorf("fold-verify edge metadata: %w", err)
	}
	if got := canonicalSubtreeHash(staged); got != snapHash {
		out.Mismatches = append(out.Mismatches, fmt.Sprintf("fold hash %s != snapshot %s", got, snapHash))
		_ = res.DiscardRoot(ctx, rootID, fenceEpoch)
		_, _ = res.RevertResidence(ctx, rootID, fenceEpoch)
		return out, fmt.Errorf("fold-verify failed: staged journal copy does not match snapshot")
	}
	if err := deps.fire(deps.hooks.afterFoldVerifyFn()); err != nil {
		return out, err
	}

	// Step 6: re-verify (TOCTOU killer for the copy window) — re-read the legacy
	// subtree; any delta means a writer raced the quarantine before the flip. Abort:
	// epoch-guarded discard, revert, no tombstone. (External writers are only
	// detected here, never fenced — see ErrRootMigrating.)
	reread, err := collectSubtree(deps.legacy, rootID)
	if err != nil {
		return out, fmt.Errorf("re-verify: %w", err)
	}
	if got := canonicalSubtreeHash(reread); got != snapHash {
		if err := res.DiscardRoot(ctx, rootID, fenceEpoch); err != nil {
			return out, fmt.Errorf("re-verify abort: discarding staged copy: %w", err)
		}
		if _, err := res.RevertResidence(ctx, rootID, fenceEpoch); err != nil {
			return out, fmt.Errorf("re-verify abort: reverting residence: %w", err)
		}
		out.Outcome = migrateOutcomeRevertedRace
		out.RacedWrite = true
		return out, nil
	}
	if err := deps.fire(deps.hooks.afterReVerifyFn()); err != nil {
		return out, err
	}

	// Step 7: flip — CAS migrating(N) -> journal. The atomic cutover point.
	flipped, err := res.FlipResidenceToJournal(ctx, rootID, fenceEpoch)
	if err != nil {
		return out, fmt.Errorf("flip: %w", err)
	}
	if !flipped {
		// The record was reverted or re-epoched out from under us: discard our own
		// epoch's rows (guarded, so a sibling's are untouched) and bail.
		_ = res.DiscardRoot(ctx, rootID, fenceEpoch)
		_, _ = res.RevertResidence(ctx, rootID, fenceEpoch)
		return out, fmt.Errorf("flip CAS lost: residence changed during migration")
	}
	if err := deps.fire(deps.hooks.afterFlipFn()); err != nil {
		return out, err
	}

	// Step 8: tombstone — close the legacy copy, mark migrated. Never delete. The
	// pre-tombstone delta guard inside ensureTombstone converts a re-verify→flip
	// window write into a loud alarm rather than closing a window-created child.
	tombstoned, err := ensureTombstone(deps, rootID)
	if err != nil {
		if errors.Is(err, errPostFlipExternalWrite) {
			out.Outcome = migrateOutcomePostFlipDelta
			out.RacedWrite = true
			out.Mismatches = append(out.Mismatches, "legacy subtree diverged from the journal copy after flip")
			return out, err
		}
		return out, fmt.Errorf("tombstone: %w", err)
	}
	out.Outcome = migrateOutcomeCutover
	out.Tombstoned = tombstoned
	return out, nil
}

// hook accessors tolerate a nil hooks pointer so production (hooks == nil) skips
// every injection with no branch at each call site.
func (h *migrateRootHooks) afterParkFn() func() error {
	if h == nil {
		return nil
	}
	return h.afterPark
}

func (h *migrateRootHooks) afterCopyFn() func() error {
	if h == nil {
		return nil
	}
	return h.afterCopy
}

func (h *migrateRootHooks) afterFoldVerifyFn() func() error {
	if h == nil {
		return nil
	}
	return h.afterFoldVerify
}

func (h *migrateRootHooks) afterReVerifyFn() func() error {
	if h == nil {
		return nil
	}
	return h.afterReVerify
}

func (h *migrateRootHooks) afterFlipFn() func() error {
	if h == nil {
		return nil
	}
	return h.afterFlip
}

func (h *migrateRootHooks) duringTombstoneCloseFn() func() error {
	if h == nil {
		return nil
	}
	return h.duringTombstoneClose
}

// migrateSubtree bundles a subtree's beads with the raw metadata blob of each
// dependency edge, keyed by EdgeKey. The edge metadata rides alongside because a
// plain beads.Dep carries no metadata field, yet a waits-for gate
// (`{"gate":"any-children"}`) MUST survive the copy for post-migration gate
// evaluation — and any change to it MUST be caught by the verify hash (HIGH-1).
type migrateSubtree struct {
	beads    []beads.Bead
	edgeMeta map[beads.EdgeKey]string
}

// ensureTombstone closes every open bead in the legacy subtree and stamps
// gc.migrated=1 on the root, but only across two delta guards that bracket the
// close loop. It never deletes (tombstone, not delete) and is idempotent: an
// already-tombstoned root (gc.migrated=1) is a no-op, which also skips the guards
// whose closed-vs-open status diff would otherwise false-positive.
//
// Data-safety residual (honest statement of the limit). The migrating fence blocks
// ROUTER-routed writes, but external `bd` writers bypass the router; they are
// DETECTED, never fenced (see the ErrRootMigrating gap in guardNotMigrating). Two
// guards here narrow — but do NOT close — the residual window in which such a write
// to an EXISTING legacy bead could be lost (the flipped journal copy is
// authoritative and missing it, and the close loop would then close the bead):
//
//   - Pre-close guard: read the authoritative journal copy first, then re-read the
//     legacy subtree LAST — with no work between that read and the first Close — and
//     require the two to hash-equal. This catches a write that landed in the
//     re-verify→flip window, up to the instant before the close loop starts.
//   - Post-close alarm: after the close loop, re-read the legacy subtree and compare
//     its status-EXCLUDING hash to the pre-close status-excluding hash. The close
//     loop legitimately flips status to closed (excluded); ANY other divergence — a
//     metadata/dep/edge mutation of an existing bead, or a new child — is an external
//     write that raced the close loop, and trips the alarm.
//
// On either divergence it returns errPostFlipExternalWrite: it does NOT set
// gc.migrated=1, leaves the legacy leg fully intact for operator reconciliation, and
// its caller drives a loud, non-zero exit. gc.migrated=1 is stamped STRICTLY after a
// fully-verified close (never mark-first): a mark-first scheme would let a crash
// mid-close leave legacy beads open while short-circuiting the resume path, which is
// double-serve. What remains unclosed is genuine PREVENTION: a concurrent external
// write to an existing bead is detected (alarm, no tombstone), not prevented — true
// prevention requires quiescing external bd writers (P4/hosted). Note the two
// non-loss cases folded in here: a new child created in the window stays on the
// intact legacy leg and is served via legacy fan-out (mergeDedupeBeads), never lost;
// only a MODIFICATION to an existing bead is the detect-not-prevent residual.
func ensureTombstone(deps migrateGraphJournalDeps, rootID string) (bool, error) {
	legRoot, err := deps.legacy.Get(rootID)
	if err != nil {
		return false, fmt.Errorf("reading legacy root %q: %w", rootID, err)
	}
	if legRoot.Metadata["gc.migrated"] == "1" {
		return true, nil // already tombstoned; idempotent no-op
	}
	// Read the authoritative journal copy first so the legacy read the guard hashes
	// is the LAST thing before the close loop — minimizing the check→mutate window an
	// external bd write to an existing bead can slip into (there must be no other work
	// between this legacy read and the first Close below).
	journalSub, err := collectSubtree(deps.journal, rootID)
	if err != nil {
		return false, fmt.Errorf("reading journal copy of %q: %w", rootID, err)
	}
	journalHash := canonicalSubtreeHash(journalSub)
	legacySub, err := collectSubtree(deps.legacy, rootID)
	if err != nil {
		return false, err
	}
	if canonicalSubtreeHash(legacySub) != journalHash {
		return false, fmt.Errorf("%w: root %q", errPostFlipExternalWrite, rootID)
	}
	// Capture the pre-close status-excluding fingerprint for the post-close alarm,
	// then close with no intervening work.
	preCloseHash := canonicalSubtreeHashExcludingStatus(legacySub)
	for _, b := range legacySub.beads {
		if b.Status == "closed" {
			continue
		}
		if err := deps.legacy.Close(b.ID); err != nil {
			return false, fmt.Errorf("closing legacy bead %q: %w", b.ID, err)
		}
	}
	if err := deps.fire(deps.hooks.duringTombstoneCloseFn()); err != nil {
		return false, err
	}
	// Post-close alarm: re-read and compare status-excluding hashes. A divergence is
	// an external write that raced the close loop — leave legacy intact, do not
	// tombstone, raise the loud alarm.
	postCloseSub, err := collectSubtree(deps.legacy, rootID)
	if err != nil {
		return false, fmt.Errorf("re-reading legacy subtree of %q: %w", rootID, err)
	}
	if canonicalSubtreeHashExcludingStatus(postCloseSub) != preCloseHash {
		return false, fmt.Errorf("%w: root %q (external write during the tombstone close loop)", errPostFlipExternalWrite, rootID)
	}
	if err := deps.legacy.SetMetadata(rootID, "gc.migrated", "1"); err != nil {
		return false, fmt.Errorf("marking legacy root %q migrated: %w", rootID, err)
	}
	return true, nil
}

// collectSubtree returns the root plus every parent-child descendant, each
// hydrated with its down-dependencies (for edge preservation) and each edge's raw
// metadata blob, sorted by id for determinism. A missing root is a hard error. The
// walk is depth-bounded by the visited set, so a cyclic parent projection cannot
// loop.
func collectSubtree(store beads.Store, rootID string) (migrateSubtree, error) {
	if _, err := store.Get(rootID); err != nil {
		return migrateSubtree{}, fmt.Errorf("resolving root %q: %w", rootID, err)
	}
	visited := map[string]bool{}
	queue := []string{rootID}
	var out []beads.Bead
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if visited[id] {
			continue
		}
		visited[id] = true
		b, err := store.Get(id)
		if err != nil {
			return migrateSubtree{}, fmt.Errorf("reading subtree bead %q: %w", id, err)
		}
		deps, err := store.DepList(id, "down")
		if err != nil {
			return migrateSubtree{}, fmt.Errorf("reading deps of %q: %w", id, err)
		}
		b.Dependencies = deps
		out = append(out, b)
		children, err := store.Children(id, beads.IncludeClosed)
		if err != nil {
			return migrateSubtree{}, fmt.Errorf("reading children of %q: %w", id, err)
		}
		for _, c := range children {
			if !visited[c.ID] {
				queue = append(queue, c.ID)
			}
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return subtreeWithEdgeMeta(store, out)
}

// subtreeWithEdgeMeta pairs a set of already-hydrated beads with the raw metadata
// of each of their dependency edges, read from store's edge-metadata capability.
// A store WITHOUT that capability yields empty edge metadata (a documented
// degradation, symmetric on both compared subtrees, never a silent wrong answer).
func subtreeWithEdgeMeta(store beads.Store, beadList []beads.Bead) (migrateSubtree, error) {
	sub := migrateSubtree{beads: beadList}
	reader, ok := beads.EdgeMetadataReaderFor(store)
	if !ok {
		return sub, nil
	}
	for _, b := range beadList {
		for _, d := range b.Dependencies {
			if d.DependsOnID == "" {
				continue
			}
			meta, err := reader.EdgeMetadata(b.ID, d.DependsOnID, d.Type)
			if err != nil {
				return migrateSubtree{}, fmt.Errorf("reading edge metadata %s->%s: %w", b.ID, d.DependsOnID, err)
			}
			if meta == "" {
				continue
			}
			if sub.edgeMeta == nil {
				sub.edgeMeta = make(map[beads.EdgeKey]string)
			}
			sub.edgeMeta[beads.EdgeKey{FromID: b.ID, ToID: d.DependsOnID, DepType: d.Type}] = meta
		}
	}
	return sub, nil
}

// isWorkerClassBead reports whether b is a unit a worker (coding agent) may hold
// or claim — as opposed to control-dispatcher, workflow-topology, or structural
// scaffolding beads, which the control runtime owns and which the journal
// ControlFrontier keeps serving post-migration. It is kind-keyed off gc.kind (no
// role name): a bead with no kind is plain worker work. This keeps the
// refuse-open-claimable guard from tripping on control steps a migration is
// meant to move.
func isWorkerClassBead(b beads.Bead) bool {
	kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])
	if kind == "" {
		return true
	}
	if beadmeta.IsControlKind(kind) {
		return false
	}
	if slices.Contains(beadmeta.WorkflowTopologyKinds, kind) {
		return false
	}
	if slices.Contains(beadmeta.StructuralGraphKinds, kind) {
		return false
	}
	if kind == beadmeta.KindWisp {
		return false
	}
	return true
}

// isWorkerHoldableAttempt reports whether b is an attempt bead a worker could hold,
// closing NOTE-2: isWorkerClassBead classes the v1-era attempt kinds KindRun and
// KindRetryRun as non-worker (they sit in StructuralGraphKinds), so an open+assigned
// v1 attempt would otherwise slip firstOpenClaimableWorker's refusal. v2 attempts
// keep their original kind and carry gc.attempt instead. Either shape — a
// run/retry-run kind, or any bead bearing gc.attempt — is a real worker attempt, so
// it is folded into the refuse-open-claimable guard alongside plain worker beads.
// gc.attempt lands only on work/iteration/child beads; control beads carry
// gc.failed_attempt / gc.next_attempt instead, so this does not over-trip on control
// steps a migration is meant to move.
func isWorkerHoldableAttempt(b beads.Bead) bool {
	kind := strings.TrimSpace(b.Metadata[beadmeta.KindMetadataKey])
	if kind == beadmeta.KindRun || kind == beadmeta.KindRetryRun {
		return true
	}
	return strings.TrimSpace(b.Metadata[beadmeta.AttemptMetadataKey]) != ""
}

// firstOpenClaimableWorker returns the id of the first subtree bead that a worker
// holds or can claim: a WORKER-class bead (or an attempt bead a worker could hold,
// see isWorkerHoldableAttempt) in a non-terminal status (open or in_progress) that
// is either assigned (non-empty assignee) or routed (gc.routed_to set). This is the
// real claim signal (`gc hook --claim`: existing_assignment / ready_assignment), not
// the dead claimed/assigned statuses. Migrating such a bead would strand the claim —
// journal work beads are worker-invisible until P4.
func firstOpenClaimableWorker(subtree []beads.Bead) string {
	for _, b := range subtree {
		if !claimActiveStatuses[b.Status] {
			continue
		}
		if !isWorkerClassBead(b) && !isWorkerHoldableAttempt(b) {
			continue
		}
		assigned := strings.TrimSpace(b.Assignee) != ""
		routed := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey]) != ""
		if assigned || routed {
			return b.ID
		}
	}
	return ""
}

// firstCrossRootBlockingDependent finds the first bead OUTSIDE the subtree that
// has a blocking dependency (blocks / waits-for / conditional-blocks) ON a subtree
// bead. Closing the subtree at tombstone would make such an external dependent go
// ready while the journal copy is still worker-invisible — so its presence refuses
// the migration (the operator resolves the cross-root edge first). Returns the
// (dependent, target) ids, or ("","") when none.
func firstCrossRootBlockingDependent(store beads.Store, subtree []beads.Bead) (string, string, error) {
	inSubtree := make(map[string]bool, len(subtree))
	for _, b := range subtree {
		inSubtree[b.ID] = true
	}
	for _, b := range subtree {
		ups, err := store.DepList(b.ID, "up")
		if err != nil {
			return "", "", fmt.Errorf("reading dependents of %q: %w", b.ID, err)
		}
		for _, d := range ups {
			if inSubtree[d.IssueID] {
				continue
			}
			if migrationBlockingDepTypes[d.Type] {
				return d.IssueID, b.ID, nil
			}
		}
	}
	return "", "", nil
}

// canonicalSubtreeHash is the fold-verify / re-verify canonical form: nodes sorted
// by id, edges by (from,to,type) WITH their metadata, labels and metadata sorted,
// VOLATILE timestamps (created_at/updated_at) excluded so a copy that faithfully
// preserves ids and structure hashes equal while a genuine content delta (status,
// metadata, deps, edge metadata, membership) does not. It marshals to R-CANON
// bytes and SHA-256s them.
func canonicalSubtreeHash(subtree migrateSubtree) string {
	return canonicalSubtreeHashFiltered(subtree, true)
}

// canonicalSubtreeHashExcludingStatus is canonicalSubtreeHash with each node's
// status zeroed out. The tombstone close loop legitimately flips every open bead
// to "closed", so comparing a pre-close snapshot to a post-close re-read with
// status included would always differ. Excluding status lets the post-close alarm
// (ensureTombstone) isolate the change that MATTERS — an external metadata/dep/
// edge/child mutation that raced the close loop — from the close loop's own,
// expected status flips.
func canonicalSubtreeHashExcludingStatus(subtree migrateSubtree) string {
	return canonicalSubtreeHashFiltered(subtree, false)
}

// canonicalSubtreeHashFiltered backs the two hash entry points. includeStatus=false
// zeroes every node's status before hashing (see canonicalSubtreeHashExcludingStatus).
func canonicalSubtreeHashFiltered(subtree migrateSubtree, includeStatus bool) string {
	type canonEdge struct {
		To       string `json:"to"`
		Type     string `json:"type"`
		Metadata string `json:"metadata,omitempty"`
	}
	type canonNode struct {
		ID          string            `json:"id"`
		Title       string            `json:"title"`
		Status      string            `json:"status"`
		Type        string            `json:"type"`
		Priority    *int              `json:"priority,omitempty"`
		Description string            `json:"description"`
		Assignee    string            `json:"assignee"`
		From        string            `json:"from"`
		ParentID    string            `json:"parent"`
		Ref         string            `json:"ref"`
		DeferUntil  string            `json:"defer_until,omitempty"`
		Ephemeral   bool              `json:"ephemeral"`
		NoHistory   bool              `json:"no_history"`
		Labels      []string          `json:"labels"`
		Metadata    map[string]string `json:"metadata"`
		Edges       []canonEdge       `json:"edges"`
	}
	nodes := make([]canonNode, 0, len(subtree.beads))
	for _, b := range subtree.beads {
		labels := append([]string(nil), b.Labels...)
		sort.Strings(labels)
		meta := map[string]string{}
		for k, v := range b.Metadata {
			meta[k] = v
		}
		edges := make([]canonEdge, 0, len(b.Dependencies))
		for _, d := range b.Dependencies {
			edges = append(edges, canonEdge{
				To:       d.DependsOnID,
				Type:     d.Type,
				Metadata: subtree.edgeMeta[beads.EdgeKey{FromID: b.ID, ToID: d.DependsOnID, DepType: d.Type}],
			})
		}
		sort.Slice(edges, func(i, j int) bool {
			if edges[i].To != edges[j].To {
				return edges[i].To < edges[j].To
			}
			return edges[i].Type < edges[j].Type
		})
		deferUntilStr := ""
		if b.DeferUntil != nil && !b.DeferUntil.IsZero() {
			deferUntilStr = b.DeferUntil.UTC().Format(time.RFC3339Nano)
		}
		status := b.Status
		if !includeStatus {
			status = ""
		}
		nodes = append(nodes, canonNode{
			ID: b.ID, Title: b.Title, Status: status, Type: b.Type, Priority: b.Priority,
			Description: b.Description, Assignee: b.Assignee, From: b.From, ParentID: b.ParentID,
			Ref: b.Ref, DeferUntil: deferUntilStr, Ephemeral: b.Ephemeral, NoHistory: b.NoHistory,
			Labels: labels, Metadata: meta, Edges: edges,
		})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].ID < nodes[j].ID })
	raw, err := json.Marshal(nodes)
	if err != nil {
		// The struct is JSON-trivial; a marshal error is impossible in practice, but
		// return a distinct sentinel rather than pretend it hashed to zero.
		return "marshal-error"
	}
	canonical, err := canon.Canonicalize(raw)
	if err != nil {
		return "canon-error"
	}
	sum := canon.Hash(canonical)
	return fmt.Sprintf("%x", sum)
}
