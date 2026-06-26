package main

import (
	"bytes"
	"database/sql"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

// postgresCfgFromDSN builds a city config + sets the password env from a test DSN.
func postgresCfgFromDSN(t *testing.T, dsn, class string) *config.City {
	t.Helper()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	password, _ := u.User.Password()
	t.Setenv("GC_POSTGRES_PASSWORD", password)
	return &config.City{Beads: config.BeadsConfig{
		Classes:  map[string]config.BeadClassConfig{class: {Backend: "postgres"}},
		Postgres: config.BeadsPostgresConfig{Host: host, Port: port, Database: strings.TrimPrefix(u.Path, "/"), User: u.User.Username(), SSLMode: u.Query().Get("sslmode")},
	}}
}

func TestOpenClassMigrationDest_RejectsBDBackend(t *testing.T) {
	cfg := &config.City{Beads: config.BeadsConfig{Classes: map[string]config.BeadClassConfig{config.BeadClassOrders: {Backend: "bd"}}}}
	if _, _, err := openClassMigrationDest(cfg, t.TempDir(), config.BeadClassOrders); err == nil {
		t.Fatal("openClassMigrationDest for a bd class should error (no relocated destination)")
	}
}

// TestBeadsMigrate_ToPostgres proves the generalized `gc beads migrate` core copies a
// class to its configured POSTGRES backend (the new dispatch in openClassMigrationDest),
// ID-preserving + idempotent. SKIPPED unless GC_TEST_POSTGRES_DSN is set.
func TestBeadsMigrate_ToPostgres(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres")
	}
	cfg := postgresCfgFromDSN(t, dsn, config.BeadClassNudges)
	schema, _ := config.ReservedClassPrefix(config.BeadClassNudges) // gcn
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	// Clean the schema so migrated counts are deterministic across runs.
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	if _, err := db.Exec(fmt.Sprintf(`TRUNCATE %[1]s.beads, %[1]s.labels, %[1]s.metadata, %[1]s.deps, %[1]s.kv CASCADE`, schema)); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	_ = db.Close() //nolint:errcheck

	src := beads.NewMemStore()
	seeded, err := src.Create(beads.Bead{Type: "chore", Title: "n", Labels: []string{"gc:nudge"}})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	cityPath := t.TempDir()
	deps := migrateDeps{
		openSource: func() (beads.Store, func(), error) { return src, func() {}, nil },
		openDest:   func(class string) (beads.Store, func(), error) { return openClassMigrationDest(cfg, cityPath, class) },
	}
	var out, errb bytes.Buffer
	if code := runBeadsMigrate([]string{config.BeadClassNudges}, deps, &out, &errb); code != 0 {
		t.Fatalf("runBeadsMigrate code=%d stderr=%s", code, errb.String())
	}
	if !strings.Contains(out.String(), "nudges: scanned=1 migrated=1 skipped=0") {
		t.Fatalf("report = %q, want a migrated=1 line", out.String())
	}
	// The migrated bead is in the Postgres gcn schema, ID-preserved.
	dst, closeDst, err := openClassMigrationDest(cfg, cityPath, config.BeadClassNudges)
	if err != nil {
		t.Fatalf("open dest: %v", err)
	}
	defer closeDst()
	if got, err := dst.Get(seeded.ID); err != nil || got.ID != seeded.ID {
		t.Fatalf("Get(%q) from postgres dest = (%+v, %v), want the migrated bead", seeded.ID, got, err)
	}
	// Idempotent re-run: skipped, not re-copied.
	var out2, errb2 bytes.Buffer
	if code := runBeadsMigrate([]string{config.BeadClassNudges}, deps, &out2, &errb2); code != 0 {
		t.Fatalf("re-run code=%d stderr=%s", code, errb2.String())
	}
	if !strings.Contains(out2.String(), "nudges: scanned=1 migrated=0 skipped=1") {
		t.Fatalf("re-run report = %q, want skipped=1", out2.String())
	}
}

func TestBuildPostgresDSN_RequiresDatabase(t *testing.T) {
	if _, err := buildPostgresDSN(config.BeadsPostgresConfig{Host: "h"}, t.TempDir()); err == nil {
		t.Fatal("buildPostgresDSN without a database should error")
	}
}

// TestOpenClassPostgresStore_RoundTrip proves the runtime wiring: openClassPostgresStore
// builds a DSN from [beads.postgres] + pgauth, opens the provisioned per-class schema,
// and resolveClassStore routes the class to it — i.e. Gas City actually reads/writes a
// class on Postgres. SKIPPED unless GC_TEST_POSTGRES_DSN points at a disposable DB.
func TestOpenClassPostgresStore_RoundTrip(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres")
	}
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("parse dsn: %v", err)
	}
	host, portStr, err := net.SplitHostPort(u.Host)
	if err != nil {
		t.Fatalf("split host: %v", err)
	}
	port, _ := strconv.Atoi(portStr)
	password, _ := u.User.Password()
	cfg := &config.City{Beads: config.BeadsConfig{
		Classes:  map[string]config.BeadClassConfig{config.BeadClassOrders: {Backend: "postgres"}},
		Postgres: config.BeadsPostgresConfig{Host: host, Port: port, Database: strings.TrimPrefix(u.Path, "/"), User: u.User.Username(), SSLMode: u.Query().Get("sslmode")},
	}}
	// Supply the password through the pgauth chain (process-env tier).
	t.Setenv("GC_POSTGRES_PASSWORD", password)

	// The runtime schema is the class's reserved prefix; provision it first
	// (openClassPostgresStore verifies, it does not provision).
	schema, _ := config.ReservedClassPrefix(config.BeadClassOrders)
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}

	cityPath := t.TempDir()
	store, ok := openClassPostgresStore(cfg, cityPath, config.BeadClassOrders, nil)
	if !ok {
		t.Fatal("openClassPostgresStore returned (_, false) — wiring failed to open")
	}
	created, err := store.Create(beads.Bead{Title: "wired", Type: "task", Labels: []string{"order-tracking"}})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !strings.HasPrefix(created.ID, schema+"-") {
		t.Fatalf("minted id %q does not use the class schema/prefix %q", created.ID, schema)
	}
	if got, err := store.Get(created.ID); err != nil || got.ID != created.ID {
		t.Fatalf("Get(%q) = (%+v, %v), want the created bead", created.ID, got, err)
	}

	// resolveClassStore must route the orders class to the postgres store (distinct
	// from the work store), and return the same cached handle.
	workStore := beads.NewMemStore()
	routed := resolveClassStore(workStore, cfg, cityPath, config.BeadClassOrders, nil)
	if routed == beads.Store(workStore) {
		t.Fatal("resolveClassStore(orders) returned the work store — postgres backend not routed")
	}
	if _, err := routed.Get(created.ID); err != nil {
		t.Fatalf("routed store Get(%q): %v (expected the same postgres handle)", created.ID, err)
	}
}
