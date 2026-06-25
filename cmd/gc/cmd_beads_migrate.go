package main

import (
	"fmt"
	"io"
	"path/filepath"
	"sort"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storemigrate"
	"github.com/spf13/cobra"
)

// classSQLitePrefix maps a coordination class to the id prefix its embedded
// SQLite store mints. Distinct prefixes keep cross-store ids unambiguous so a
// stranded bd-era id never resolves into the wrong SQLite store. Sourced from
// the single registry in internal/config; TestClassSQLitePrefixRegistryParity
// pins the graph entry to graphStoreIDPrefix.
var classSQLitePrefix = config.ReservedClassPrefixes()

// classSQLiteDir returns the per-class SQLite store directory under the city
// runtime root (each class gets its own file so lifecycles/ids stay independent).
func classSQLiteDir(cityPath, class string) string {
	return filepath.Join(cityPath, citylayout.RuntimeRoot, class)
}

// migrateDeps abstracts store opening so the migration command is unit-testable
// without a real city, bd, or on-disk SQLite.
type migrateDeps struct {
	openSource func() (store beads.Store, closeFn func(), err error)
	openDest   func(class string) (store beads.Store, closeFn func(), err error)
}

func newBeadsMigrateSQLiteCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate-sqlite [class...]",
		Short: "Copy dolt-backed infra beads into their SQLite stores",
		Long: `Copy beads of each SQLite-relocated coordination class from the bd/Dolt
work store into that class's embedded SQLite store, ID-preserving and
idempotent, so the read path no longer has to query Dolt for that class.

With no arguments, migrates every class whose [beads.classes.<class>].backend
is "sqlite". Pass class names (messaging, sessions, orders, nudges, graph) to
migrate a subset. Safe to re-run: already-migrated beads are skipped.`,
		Example: `  gc beads migrate-sqlite
  gc beads migrate-sqlite messaging orders`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc beads migrate-sqlite: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			classes, err := resolveMigrateClasses(cityPath, args, stderr)
			if err != nil {
				return errExit
			}
			deps := migrateDeps{
				openSource: func() (beads.Store, func(), error) {
					store, _, code := openCityStatusStore(cityPath, stderr)
					if code != 0 {
						return nil, nil, fmt.Errorf("opening city work store (see above)")
					}
					return store, closeBeadStoreFunc(store), nil
				},
				openDest: func(class string) (beads.Store, func(), error) {
					store, err := beads.OpenSQLiteStore(
						classSQLiteDir(cityPath, class),
						beads.WithSQLiteStoreIDPrefix(classSQLitePrefix[class]),
						beads.WithSQLiteStoreRetention(0, 0),
					)
					if err != nil {
						return nil, nil, err
					}
					return store, closeBeadStoreFunc(store), nil
				},
			}
			if runBeadsMigrateSQLite(classes, deps, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// closeBeadStoreFunc returns a best-effort closer for a store that owns a handle.
func closeBeadStoreFunc(store beads.Store) func() {
	if c, ok := store.(interface{ CloseStore() error }); ok {
		return func() { _ = c.CloseStore() } //nolint:errcheck // best-effort close
	}
	return func() {}
}

// resolveMigrateClasses returns the classes to migrate: the explicit args if
// given, otherwise every class configured onto the SQLite backend.
func resolveMigrateClasses(cityPath string, args []string, stderr io.Writer) ([]string, error) {
	if len(args) > 0 {
		for _, c := range args {
			if _, ok := classSQLitePrefix[c]; !ok {
				fmt.Fprintf(stderr, "gc beads migrate-sqlite: unknown class %q (want one of messaging, sessions, orders, nudges, graph)\n", c) //nolint:errcheck
				return nil, fmt.Errorf("unknown class %q", c)
			}
		}
		return args, nil
	}
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc beads migrate-sqlite: %v\n", err) //nolint:errcheck
		return nil, err
	}
	emitLoadCityConfigWarnings(stderr, prov)
	var classes []string
	for class := range classSQLitePrefix {
		if cfg.Beads.ClassUsesSQLite(class) {
			classes = append(classes, class)
		}
	}
	sort.Strings(classes)
	if len(classes) == 0 {
		fmt.Fprintln(stderr, "gc beads migrate-sqlite: no classes configured with backend=\"sqlite\"; nothing to migrate") //nolint:errcheck
	}
	return classes, nil
}

// runBeadsMigrateSQLite migrates each class from the source store into its
// destination SQLite store and reports per-class counts. It is the testable core
// of the command.
func runBeadsMigrateSQLite(classes []string, deps migrateDeps, stdout, stderr io.Writer) int {
	if len(classes) == 0 {
		return 0
	}
	src, closeSrc, err := deps.openSource()
	if err != nil {
		fmt.Fprintf(stderr, "gc beads migrate-sqlite: %v\n", err) //nolint:errcheck
		return 1
	}
	defer closeSrc()

	failed := false
	for _, class := range classes {
		dst, closeDst, err := deps.openDest(class)
		if err != nil {
			fmt.Fprintf(stderr, "gc beads migrate-sqlite: opening %s store: %v\n", class, err) //nolint:errcheck
			failed = true
			continue
		}
		rep, err := storemigrate.Migrate(src, dst, storemigrate.ClassSelector(class))
		closeDst()
		if err != nil {
			fmt.Fprintf(stderr, "gc beads migrate-sqlite: migrating %s: %v\n", class, err) //nolint:errcheck
			failed = true
			continue
		}
		fmt.Fprintf(stdout, "%s: scanned=%d migrated=%d skipped=%d\n", class, rep.Scanned, rep.Migrated, rep.Skipped) //nolint:errcheck
	}
	if failed {
		return 1
	}
	return 0
}
