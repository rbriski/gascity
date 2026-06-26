package main

import (
	"fmt"
	"log"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/pgauth"
)

// Register the postgres backend opener into the generic per-class backend registry
// (cmd/gc/class_store.go). Adding a backend is "register an opener here", not
// editing a dispatch switch — the package-level classBackendOpeners map is
// initialized before init() runs.
func init() {
	classBackendOpeners[config.BeadsBackendPostgres] = openClassPostgresStore
}

// postgresClassHandleCache shares one PostgresStore handle per (city, class) across
// in-process consumers, mirroring classStoreHandleCache for SQLite, so one
// consumer's close cannot pull the pool out from under the others.
var postgresClassHandleCache sync.Map // key string -> beads.Store

// openClassPostgresStore opens (or returns the cached) PostgresStore for a class:
// one shared database from [beads.postgres], the class's reserved prefix as both its
// Postgres schema and id prefix, and the controller's bead.* recorder so the
// relocated class keeps feeding the event bus. The schema must already be
// provisioned (`gc beads postgres init`); Open verifies and runs no DDL, so a
// misconfigured or unprovisioned backend LOUDLY falls back to the work store (the
// registry contract) rather than silently diverting.
func openClassPostgresStore(cfg *config.City, cityPath, class string, rec events.Recorder) (beads.Store, bool) {
	schema, ok := config.ReservedClassPrefix(class)
	if !ok {
		log.Printf("beads: class %q has no reserved prefix; cannot route it to postgres; staying on the work store", class)
		return nil, false
	}
	key := cityPath + "\x00" + class
	if cached, ok := postgresClassHandleCache.Load(key); ok {
		return cached.(beads.Store), true
	}
	dsn, err := buildPostgresDSN(cfg.Beads.Postgres, cityPath)
	if err != nil {
		log.Printf("beads: class %q backend=postgres: %v; staying on the work store", class, err)
		return nil, false
	}
	var opened beads.Store // late-bound so the recorder can read the post-commit bead
	opts := []beads.PostgresStoreOption{
		beads.WithPostgresStoreSchema(schema),
		beads.WithPostgresStoreIDPrefix(schema),
	}
	if rec != nil {
		opts = append(opts, beads.WithPostgresStoreRecorder(
			beadEventRowRecorder(func(id string) (beads.Bead, error) { return opened.Get(id) }, rec),
		))
	}
	store, err := beads.OpenPostgresStore(dsn, opts...)
	if err != nil {
		log.Printf("beads: class %q backend=postgres: opening schema %q failed: %v; staying on the work store", class, schema, err)
		return nil, false
	}
	opened = store
	if actual, loaded := postgresClassHandleCache.LoadOrStore(key, store); loaded {
		if closer, ok := store.(interface{ CloseStore() error }); ok {
			_ = closer.CloseStore() //nolint:errcheck // best-effort close of the losing duplicate
		}
		return actual.(beads.Store), true
	}
	return store, true
}

// buildPostgresDSN assembles a lib/pq DSN from the non-secret [beads.postgres]
// config plus the password resolved through the pgauth chain for scopeRoot (env /
// <scope>/.beads/.env / credentials file). The database is required; host, port,
// user, and sslmode have sensible defaults.
func buildPostgresDSN(pg config.BeadsPostgresConfig, scopeRoot string) (string, error) {
	host := strings.TrimSpace(pg.Host)
	if host == "" {
		host = "localhost"
	}
	port := pg.Port
	if port == 0 {
		port = 5432
	}
	database := strings.TrimSpace(pg.Database)
	if database == "" {
		return "", fmt.Errorf("beads.postgres.database is required for the postgres backend")
	}
	user := strings.TrimSpace(pg.User)
	if user == "" {
		user = "postgres"
	}
	sslmode := strings.TrimSpace(pg.SSLMode)
	if sslmode == "" {
		sslmode = "prefer"
	}
	resolved, err := pgauth.ResolveFromEnv(nil, scopeRoot, pgauth.Endpoint{Host: host, Port: strconv.Itoa(port), User: user})
	if err != nil {
		return "", fmt.Errorf("resolving postgres password: %w", err)
	}
	u := url.URL{
		Scheme:   "postgres",
		User:     url.UserPassword(resolved.User, resolved.Password),
		Host:     net.JoinHostPort(host, strconv.Itoa(port)),
		Path:     "/" + database,
		RawQuery: url.Values{"sslmode": {sslmode}}.Encode(),
	}
	return u.String(), nil
}
