package main

import (
	"database/sql"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strings"

	"github.com/lib/pq"
	"github.com/spf13/cobra"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/storemigrate"
)

func newBeadsPostgresCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "postgres",
		Short: "Provision and migrate the Postgres internal-beads backend",
		Long: `Create, initialize, and migrate the Postgres database that backs the infra
coordination classes ([beads.classes.<class>].backend = "postgres").

Connection details come from [beads.postgres] in city.toml; the password resolves
through the pgauth chain (GC_POSTGRES_PASSWORD / BEADS_POSTGRES_PASSWORD env,
<scope>/.beads/.env, or ~/.config/beads/credentials). Each relocated class lives in
its own schema (named for the class's reserved id prefix) in one shared database.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			if len(args) == 0 {
				fmt.Fprintln(stderr, "gc beads postgres: missing subcommand (init, migrate, health)") //nolint:errcheck
			} else {
				fmt.Fprintf(stderr, "gc beads postgres: unknown subcommand %q\n", args[0]) //nolint:errcheck
			}
			return errExit
		},
	}
	cmd.AddCommand(
		newBeadsPostgresInitCmd(stdout, stderr),
		newBeadsPostgresMigrateCmd(stdout, stderr),
		newBeadsPostgresHealthCmd(stdout, stderr),
	)
	return cmd
}

// loadPostgresConfig loads the city config and returns its [beads.postgres] block
// plus the classes to act on: the explicit args, otherwise every class configured
// with backend="postgres".
func loadPostgresConfig(cityPath string, args []string, stderr io.Writer) (config.BeadsPostgresConfig, []string, error) {
	cfg, prov, err := config.LoadWithIncludes(fsys.OSFS{}, filepath.Join(cityPath, "city.toml"))
	if err != nil {
		fmt.Fprintf(stderr, "gc beads postgres: %v\n", err) //nolint:errcheck
		return config.BeadsPostgresConfig{}, nil, err
	}
	emitLoadCityConfigWarnings(stderr, prov)
	var classes []string
	if len(args) > 0 {
		for _, c := range args {
			if _, ok := config.ReservedClassPrefix(c); !ok {
				fmt.Fprintf(stderr, "gc beads postgres: unknown class %q (want one of messaging, sessions, orders, nudges, graph)\n", c) //nolint:errcheck
				return config.BeadsPostgresConfig{}, nil, fmt.Errorf("unknown class %q", c)
			}
		}
		classes = append(classes, args...)
	} else {
		for class := range config.ReservedClassPrefixes() {
			if cfg.Beads.NormalizedClassBackend(class) == config.BeadsBackendPostgres {
				classes = append(classes, class)
			}
		}
	}
	sort.Strings(classes)
	return cfg.Beads.Postgres, classes, nil
}

func newBeadsPostgresInitCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "init [class...]",
		Short: "Create the database (if missing) and provision per-class schemas",
		Long: `Create the [beads.postgres].database if it does not exist, then provision a
schema (named for the class's reserved id prefix) with its tables, indexes, and id
sequence for each class. Idempotent and advisory-locked, so it is safe to re-run and
to run from multiple hosts. With no args, provisions every class whose backend is
"postgres".`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc beads postgres init: %v\n", err) //nolint:errcheck
				return errExit
			}
			pg, classes, err := loadPostgresConfig(cityPath, args, stderr)
			if err != nil {
				return errExit
			}
			if len(classes) == 0 {
				fmt.Fprintln(stderr, `gc beads postgres init: no classes configured with backend="postgres"; nothing to do`) //nolint:errcheck
				return nil
			}
			if err := ensurePostgresDatabase(pg, cityPath, stdout, stderr); err != nil {
				return errExit
			}
			dsn, err := buildPostgresDSN(pg, cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc beads postgres init: %v\n", err) //nolint:errcheck
				return errExit
			}
			failed := false
			for _, class := range classes {
				schema, _ := config.ReservedClassPrefix(class)
				if err := beads.ProvisionPostgres(dsn, schema); err != nil {
					fmt.Fprintf(stderr, "gc beads postgres init: %s: %v\n", class, err) //nolint:errcheck
					failed = true
					continue
				}
				fmt.Fprintf(stdout, "%s: provisioned schema %q\n", class, schema) //nolint:errcheck
			}
			if failed {
				return errExit
			}
			return nil
		},
	}
}

// ensurePostgresDatabase creates the configured database if absent, connecting to
// the "postgres" maintenance database. CREATE DATABASE cannot run in a transaction,
// so it runs standalone after a presence check.
func ensurePostgresDatabase(pg config.BeadsPostgresConfig, scopeRoot string, stdout, stderr io.Writer) error {
	database := strings.TrimSpace(pg.Database)
	if database == "" {
		fmt.Fprintln(stderr, "gc beads postgres init: beads.postgres.database is required") //nolint:errcheck
		return fmt.Errorf("database required")
	}
	adminDSN, err := buildPostgresDSNTo(pg, scopeRoot, "postgres")
	if err != nil {
		fmt.Fprintf(stderr, "gc beads postgres init: %v\n", err) //nolint:errcheck
		return err
	}
	db, err := sql.Open("postgres", adminDSN)
	if err != nil {
		fmt.Fprintf(stderr, "gc beads postgres init: connect to maintenance db: %v\n", err) //nolint:errcheck
		return err
	}
	defer db.Close() //nolint:errcheck
	var exists bool
	if err := db.QueryRow(`SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1)`, database).Scan(&exists); err != nil {
		fmt.Fprintf(stderr, "gc beads postgres init: checking database: %v\n", err) //nolint:errcheck
		return err
	}
	if exists {
		return nil
	}
	if _, err := db.Exec(`CREATE DATABASE ` + pq.QuoteIdentifier(database)); err != nil {
		fmt.Fprintf(stderr, "gc beads postgres init: create database %q: %v\n", database, err) //nolint:errcheck
		return err
	}
	fmt.Fprintf(stdout, "created database %q\n", database) //nolint:errcheck
	return nil
}

func newBeadsPostgresMigrateCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "migrate [class...]",
		Short: "Copy dolt-backed infra beads into their Postgres schemas",
		Long: `Copy each class's beads from the bd/Dolt work store into its Postgres schema,
ID-preserving and idempotent (already-migrated beads are skipped). Run after init.
With no args, migrates every class whose backend is "postgres".`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc beads postgres migrate: %v\n", err) //nolint:errcheck
				return errExit
			}
			pg, classes, err := loadPostgresConfig(cityPath, args, stderr)
			if err != nil {
				return errExit
			}
			if len(classes) == 0 {
				fmt.Fprintln(stderr, `gc beads postgres migrate: no classes configured with backend="postgres"; nothing to migrate`) //nolint:errcheck
				return nil
			}
			dsn, err := buildPostgresDSN(pg, cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc beads postgres migrate: %v\n", err) //nolint:errcheck
				return errExit
			}
			src, _, code := openCityStatusStore(cityPath, stderr)
			if code != 0 {
				return errExit
			}
			defer closeBeadStoreFunc(src)()
			failed := false
			for _, class := range classes {
				schema, _ := config.ReservedClassPrefix(class)
				dst, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema), beads.WithPostgresStoreIDPrefix(schema))
				if err != nil {
					fmt.Fprintf(stderr, "gc beads postgres migrate: %s: %v\n", class, err) //nolint:errcheck
					failed = true
					continue
				}
				rep, err := storemigrate.Migrate(src, dst, storemigrate.ClassSelector(class))
				if c, ok := dst.(interface{ CloseStore() error }); ok {
					_ = c.CloseStore() //nolint:errcheck
				}
				if err != nil {
					fmt.Fprintf(stderr, "gc beads postgres migrate: %s: %v\n", class, err) //nolint:errcheck
					failed = true
					continue
				}
				fmt.Fprintf(stdout, "%s: scanned=%d migrated=%d skipped=%d\n", class, rep.Scanned, rep.Migrated, rep.Skipped) //nolint:errcheck
			}
			if failed {
				return errExit
			}
			return nil
		},
	}
}

func newBeadsPostgresHealthCmd(stdout, stderr io.Writer) *cobra.Command {
	return &cobra.Command{
		Use:   "health [class...]",
		Short: "Check Postgres connectivity and per-class schema provisioning",
		Long: `Open each configured class's Postgres schema, which verifies connectivity and
that the schema is provisioned (it runs no DDL). Reports per-class status; exits
non-zero if any class is unreachable or unprovisioned.`,
		Args: cobra.ArbitraryArgs,
		RunE: func(_ *cobra.Command, args []string) error {
			cityPath, err := resolveCity()
			if err != nil {
				fmt.Fprintf(stderr, "gc beads postgres health: %v\n", err) //nolint:errcheck
				return errExit
			}
			pg, classes, err := loadPostgresConfig(cityPath, args, stderr)
			if err != nil {
				return errExit
			}
			if len(classes) == 0 {
				fmt.Fprintln(stdout, `no classes configured with backend="postgres"`) //nolint:errcheck
				return nil
			}
			dsn, err := buildPostgresDSN(pg, cityPath)
			if err != nil {
				fmt.Fprintf(stderr, "gc beads postgres health: %v\n", err) //nolint:errcheck
				return errExit
			}
			failed := false
			for _, class := range classes {
				schema, _ := config.ReservedClassPrefix(class)
				store, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema))
				if err != nil {
					fmt.Fprintf(stderr, "%s: %v\n", class, err) //nolint:errcheck
					failed = true
					continue
				}
				if c, ok := store.(interface{ CloseStore() error }); ok {
					_ = c.CloseStore() //nolint:errcheck
				}
				fmt.Fprintf(stdout, "%s: ok (schema %q)\n", class, schema) //nolint:errcheck
			}
			if failed {
				return errExit
			}
			return nil
		},
	}
}
