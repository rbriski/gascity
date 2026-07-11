package main

import (
	"encoding/json"
	"fmt"
	"io"

	"github.com/spf13/cobra"
)

// newMigrateCmd is the `gc migrate` command group. It hosts owner-gated,
// stop-the-world data migrations that are opt-in (never auto-run at gc start).
// Today it has one subcommand: infra-store (the E3 domain/infra store split
// migration).
func newMigrateCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "migrate",
		Short: "Run opt-in data migrations",
		Long: `Run owner-gated, stop-the-world data migrations on the city.

These are explicit, one-time upgrades — never run automatically at gc start.
Stop the city first (gc stop) before running a migration.`,
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
	cmd.AddCommand(newMigrateInfraStoreCmd(stdout, stderr))
	return cmd
}

// newMigrateInfraStoreCmd is `gc migrate infra-store`: it upgrades an existing
// single-Dolt-db city to the domain/infra two-store layout, moving every
// infrastructure-class bead (sessions, mail, nudges, orders, graph/formula
// explosion) into a dedicated city infra store while leaving work beads
// untouched. It is idempotent, resumable, and crash-safe: re-running on a
// migrated city is a no-op.
func newMigrateInfraStoreCmd(stdout, stderr io.Writer) *cobra.Command {
	var dryRun, jsonOut bool
	cmd := &cobra.Command{
		Use:   "infra-store",
		Short: "Migrate a single-db city to the domain/infra two-store layout",
		Long: `Upgrade an existing single-Dolt-db city to the two-store layout.

Infrastructure-class beads (sessions, mail, nudges, orders, and the whole
formula/orchestration explosion) move into a dedicated city infra store; work
beads (tasks, epics, bugs, features, user convoys) stay in the domain stores
untouched. Bead ids are preserved — legacy infra beads keep their HQ/rig-era
prefix. Cross-boundary dependency edges are kept co-resident with their source
bead (dangling across the boundary, resolved by the two-store Go seams).

The migration is idempotent, resumable, and crash-safe: it recomputes its plan
from live store state on every run (no status file), copies before deleting, and
verifies before deleting, so a crash leaves only re-runnable states and a re-run
on a migrated city moves nothing.

Stop the city first (gc stop). This command refuses to run while a controller is
alive. External/hosted Dolt cities are not supported.`,
		Example: `  gc stop
  gc migrate infra-store
  gc migrate infra-store --dry-run
  gc migrate infra-store --json`,
		Args: cobra.NoArgs,
		RunE: func(_ *cobra.Command, _ []string) error {
			if doMigrateInfraStoreCmd(dryRun, jsonOut, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "report the migration plan without making any writes")
	cmd.Flags().BoolVar(&jsonOut, "json", false, "emit the reconciliation ledger as JSON")
	return cmd
}

// doMigrateInfraStoreCmd resolves the city, runs the migration, and prints the
// reconciliation ledger (JSON or human summary). Returns a non-zero exit code on
// failure.
func doMigrateInfraStoreCmd(dryRun, jsonOut bool, stdout, stderr io.Writer) int {
	cityPath, err := resolveCity()
	if err != nil {
		fmt.Fprintf(stderr, "gc migrate infra-store: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	ledger, err := doMigrateInfraStore(cityPath, dryRun, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc migrate infra-store: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	if jsonOut {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(ledger); err != nil {
			fmt.Fprintf(stderr, "gc migrate infra-store: encoding ledger: %v\n", err) //nolint:errcheck // best-effort stderr
			return 1
		}
		return 0
	}

	printMigrateLedger(stdout, ledger)
	return 0
}

// printMigrateLedger writes a human-readable summary of a migration run.
func printMigrateLedger(w io.Writer, l *migrateLedger) {
	if l.DryRun {
		fmt.Fprintf(w, "gc migrate infra-store (dry run): would move %d infra bead(s) to the infra store\n", l.Moved) //nolint:errcheck
		fmt.Fprintf(w, "  stores swept: %v\n", l.Stores)                                                              //nolint:errcheck
		if l.AlreadyPresent > 0 {
			fmt.Fprintf(w, "  already in infra store (crash-resume): %d\n", l.AlreadyPresent) //nolint:errcheck
		}
		fmt.Fprintf(w, "  infra store: %d → %d bead(s)\n", l.InfraBefore, l.InfraAfter) //nolint:errcheck
		if len(l.CrossBoundaryBlockingEdges) > 0 {
			fmt.Fprintf(w, "  work→infra blocking edges that will dangle: %d\n", len(l.CrossBoundaryBlockingEdges)) //nolint:errcheck
		}
		fmt.Fprintln(w, "  (no changes made — dry run)") //nolint:errcheck
		return
	}
	fmt.Fprintf(w, "gc migrate infra-store: moved %d, deleted %d, edges %d\n", l.Moved, l.Deleted, l.EdgesAdded) //nolint:errcheck
	fmt.Fprintf(w, "  stores swept: %v\n", l.Stores)                                                             //nolint:errcheck
	if l.AlreadyPresent > 0 {
		fmt.Fprintf(w, "  already in infra store before this run (crash-resume): %d\n", l.AlreadyPresent) //nolint:errcheck
	}
	fmt.Fprintf(w, "  infra store: %d → %d bead(s)\n", l.InfraBefore, l.InfraAfter) //nolint:errcheck
	for _, ref := range l.Stores {
		before := l.WorkBefore[ref]
		after := l.WorkAfter[ref]
		fmt.Fprintf(w, "  domain %q: %d → %d bead(s)\n", ref, before, after) //nolint:errcheck
	}
	if len(l.CrossBoundaryBlockingEdges) > 0 {
		fmt.Fprintf(w, "  work→infra blocking edges now dangling (no longer block bd ready): %d\n", len(l.CrossBoundaryBlockingEdges)) //nolint:errcheck
	}
	if l.Moved == 0 && l.Deleted == 0 {
		fmt.Fprintln(w, "  already migrated — nothing to do") //nolint:errcheck
	}
}
