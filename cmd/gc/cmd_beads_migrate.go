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

func newBeadsMigrateCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:     "migrate [class...]",
		Aliases: []string{"migrate-sqlite"},
		Short:   "Copy dolt-backed infra beads into their configured backend (sqlite/postgres)",
		Long: `Copy beads of each relocated coordination class from the bd/Dolt work store
into that class's configured backend — its embedded SQLite store
([beads.classes.<class>].backend = "sqlite") or its Postgres schema
("postgres", which must already be provisioned via 'gc beads postgres init').
ID-preserving and idempotent, so re-running skips already-migrated beads.

With no arguments, migrates every class whose backend is "sqlite" or "postgres".
Pass class names (messaging, sessions, orders, nudges, graph) to migrate a subset.`,
		Example: `  gc beads migrate
  gc beads migrate messaging orders`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc beads migrate: %v\n", err) //nolint:errcheck // best-effort stderr
				return errExit
			}
			classes, err := resolveMigrateClasses(cityPath, args, stderr)
			if err != nil {
				return errExit
			}
			cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
			if err != nil {
				fmt.Fprintf(stderr, "gc beads migrate: %v\n", err) //nolint:errcheck
				return errExit
			}
			emitLoadCityConfigWarnings(stderr, prov)
			deps := migrateDeps{
				openSource: func() (beads.Store, func(), error) {
					store, _, code := openCityStatusStore(cityPath, stderr)
					if code != 0 {
						return nil, nil, fmt.Errorf("opening city work store (see above)")
					}
					return store, closeBeadStoreFunc(store), nil
				},
				openDest: func(class string) (beads.Store, func(), error) {
					return openClassMigrationDest(cfg, cityPath, class)
				},
			}
			if runBeadsMigrate(classes, deps, stdout, stderr) != 0 {
				return errExit
			}
			return nil
		},
	}
}

// openClassMigrationDest opens a class's migration DESTINATION store for its
// configured backend, WITHOUT an event recorder — a bulk copy should not flood the
// event bus. SQLite stores are created on open; a Postgres schema must already be
// provisioned ('gc beads postgres init'). A class on the bd backend has no relocated
// destination and is an error here.
func openClassMigrationDest(cfg *config.City, cityPath, class string) (beads.Store, func(), error) {
	switch cfg.Beads.NormalizedClassBackend(class) {
	case config.BeadsBackendSQLite:
		store, err := beads.OpenSQLiteStore(
			classSQLiteDir(cityPath, class),
			beads.WithSQLiteStoreIDPrefix(classSQLitePrefix[class]),
			beads.WithSQLiteStoreRetention(0, 0),
		)
		if err != nil {
			return nil, nil, err
		}
		return store, closeBeadStoreFunc(store), nil
	case config.BeadsBackendPostgres:
		dsn, err := buildPostgresDSN(cfg.Beads.Postgres, cityPath)
		if err != nil {
			return nil, nil, err
		}
		schema, _ := config.ReservedClassPrefix(class)
		store, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema), beads.WithPostgresStoreIDPrefix(schema))
		if err != nil {
			return nil, nil, err
		}
		return store, closeBeadStoreFunc(store), nil
	default:
		return nil, nil, fmt.Errorf("class %q is not configured for a relocated backend (sqlite or postgres)", class)
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
		switch cfg.Beads.NormalizedClassBackend(class) {
		case config.BeadsBackendSQLite, config.BeadsBackendPostgres:
			classes = append(classes, class)
		}
	}
	sort.Strings(classes)
	if len(classes) == 0 {
		fmt.Fprintln(stderr, `gc beads migrate: no classes configured with backend="sqlite" or "postgres"; nothing to migrate`) //nolint:errcheck
	}
	return classes, nil
}

// runBeadsMigrate migrates each class from the source store into its
// destination SQLite store and reports per-class counts. It is the testable core
// of the command.
func runBeadsMigrate(classes []string, deps migrateDeps, stdout, stderr io.Writer) int {
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
