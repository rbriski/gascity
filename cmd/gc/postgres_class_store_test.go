package main

import (
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
)

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
