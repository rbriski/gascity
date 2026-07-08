//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/canon"
	"github.com/gastownhall/gascity/internal/graphstore/pgqmark"
)

// pgExitBaseDSN returns the Postgres DSN for the P6-EXIT hosted-wiring e2e, or
// skips cleanly when none is configured. GRAPHSTORE_PG_DSN is the primary gate
// (matching the graphstore conformance suite); GC_GRAPH_TEST_PG_DSN is the
// blueprint alias.
func pgExitBaseDSN(t *testing.T) string {
	t.Helper()
	for _, env := range []string{"GRAPHSTORE_PG_DSN", "GC_GRAPH_TEST_PG_DSN"} {
		if dsn := strings.TrimSpace(os.Getenv(env)); dsn != "" {
			return dsn
		}
	}
	t.Skip("GRAPHSTORE_PG_DSN not set; skipping P6-EXIT hosted Postgres e2e")
	return ""
}

// newPGSchemaDSN creates a fresh private schema on a bootstrap connection and
// returns a DSN whose search_path is pinned to it (via the libpq `options`
// parameter), so the opted city's whole journal lives in an isolated schema that
// is dropped on cleanup. It returns (dsn, schema).
func newPGSchemaDSN(t *testing.T, base string) (string, string) {
	t.Helper()
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "gc_p6exit_" + hex.EncodeToString(b[:])

	boot, err := sql.Open(pgqmark.DriverName, base)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	if _, err := boot.ExecContext(context.Background(), "CREATE SCHEMA "+schema); err != nil {
		_ = boot.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}
	t.Cleanup(func() {
		if _, err := boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
		_ = boot.Close()
	})

	u, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse base dsn: %v", err)
	}
	q := u.Query()
	q.Set("options", "-c search_path="+schema)
	u.RawQuery = q.Encode()
	return u.String(), schema
}

// countNodeRows reports how many rows in <schema>.nodes carry id, via a fresh
// bootstrap connection — proving the row physically lives in Postgres rather than
// an on-disk SQLite file.
func countNodeRows(t *testing.T, base, schema, id string) int {
	t.Helper()
	boot, err := sql.Open(pgqmark.DriverName, base)
	if err != nil {
		t.Fatalf("count open: %v", err)
	}
	defer func() { _ = boot.Close() }()
	var n int
	if err := boot.QueryRowContext(context.Background(),
		"SELECT count(*) FROM "+schema+".nodes WHERE id = ?", id).Scan(&n); err != nil {
		t.Fatalf("count nodes in %s: %v", schema, err)
	}
	return n
}

// TestP6ExitHostedPostgresJournalDrainsEndToEnd is the P6 EXIT gate: an opted city
// with `backend: postgres` (sourced via dsn_env AND via credential_command) opens
// its graph journal on Postgres through the SAME production opener the runtime
// uses, mints and reads a bead back, keeps NO on-disk SQLite journal, has its row
// physically resident in Postgres, drains from Postgres on a fresh handle, and its
// hash-chained journal Verifies from Postgres.
func TestP6ExitHostedPostgresJournalDrainsEndToEnd(t *testing.T) {
	base := pgExitBaseDSN(t)
	ctx := context.Background()

	for _, arm := range []string{"dsn_env", "credential_command"} {
		t.Run(arm, func(t *testing.T) {
			dsn, schema := newPGSchemaDSN(t, base)
			cityPath := t.TempDir()

			switch arm {
			case "dsn_env":
				const envName = "GC_GRAPH_P6EXIT_DSN"
				t.Setenv(envName, dsn)
				writeGraphBackendMarker(t, cityPath,
					"backend: postgres\npostgres:\n  dsn_env: "+envName+"\n")
			case "credential_command":
				writeGraphBackendMarker(t, cityPath,
					"backend: postgres\npostgres:\n  credential_command: printf '%s' "+shellSingleQuote(dsn)+"\n")
			}

			// 1) The production opener opens the PG-backed journal from config alone.
			result, present, err := openCityGraphJournalResultAt(cityPath)
			if err != nil {
				t.Fatalf("openCityGraphJournalResultAt: %v", err)
			}
			if !present || result.Store == nil {
				t.Fatalf("present=%v store=%v, want an opened Postgres-backed store", present, result.Store)
			}

			// 2) Round-trip a bead through the façade (the same path gc run uses).
			created, err := result.Store.Create(beads.Bead{Title: "hosted-root"})
			if err != nil {
				t.Fatalf("Create: %v", err)
			}
			got, err := result.Store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get(%q): %v", created.ID, err)
			}
			if got.Title != "hosted-root" {
				t.Fatalf("round-trip title = %q, want hosted-root", got.Title)
			}
			if err := closeBeadStoreHandle(result.Store); err != nil {
				t.Fatalf("close store: %v", err)
			}

			// 3) It lives in Postgres, not SQLite: no journal.db on disk.
			journalDB := filepath.Join(cityPath, ".gc", "graph", "journal.db")
			if _, statErr := os.Stat(journalDB); !os.IsNotExist(statErr) {
				t.Fatalf("no SQLite journal.db may exist for a postgres city; stat err = %v", statErr)
			}

			// 4) The row is physically resident in Postgres.
			if n := countNodeRows(t, base, schema, created.ID); n != 1 {
				t.Fatalf("nodes rows for %q in schema %s = %d, want 1 (bead not resident in Postgres)", created.ID, schema, n)
			}

			// 5) It drains from Postgres on a fresh handle (durability across a reopen).
			result2, _, err := openCityGraphJournalResultAt(cityPath)
			if err != nil {
				t.Fatalf("reopen: %v", err)
			}
			got2, err := result2.Store.Get(created.ID)
			if err != nil {
				t.Fatalf("Get after reopen: %v", err)
			}
			if got2.Title != "hosted-root" {
				t.Fatalf("reopened title = %q, want hosted-root", got2.Title)
			}
			if err := closeBeadStoreHandle(result2.Store); err != nil {
				t.Fatalf("close reopened store: %v", err)
			}

			// 6) The hash-chained journal itself lives in and Verifies from Postgres.
			//    Resolve the DSN via the SAME config path, open the engine directly,
			//    append a root event, read it back, and Verify the chain.
			cfg, err := loadGraphJournalBackendConfig(cityPath)
			if err != nil {
				t.Fatalf("loadGraphJournalBackendConfig: %v", err)
			}
			resolved, err := cfg.resolvePostgresDSN(ctx)
			if err != nil {
				t.Fatalf("resolvePostgresDSN: %v", err)
			}
			gs, err := graphstore.OpenPostgres(ctx, resolved, graphstore.Options{})
			if err != nil {
				t.Fatalf("OpenPostgres (verify handle): %v", err)
			}
			defer func() { _ = gs.Close() }()

			const engine = "lumen"
			const evType = "lumen.node.decision"
			const stream = "gcj-p6exit-root"
			gs.RegisterEventType(engine, evType)
			payload, err := canon.Canonicalize([]byte(`{"root":"hosted"}`))
			if err != nil {
				t.Fatalf("canonicalize: %v", err)
			}
			res, err := gs.Append(ctx, stream, engine, 0, 0, []graphstore.JournalEvent{{Type: evType, Payload: payload}})
			if err != nil {
				t.Fatalf("Append root: %v", err)
			}
			if res.FirstSeq != 1 {
				t.Fatalf("root FirstSeq = %d, want 1", res.FirstSeq)
			}
			events, err := gs.ReadStream(ctx, stream, 1, 0)
			if err != nil {
				t.Fatalf("ReadStream: %v", err)
			}
			if len(events) != 1 || string(events[0].Payload) != string(payload) {
				t.Fatalf("ReadStream returned %d events; payload round-trip mismatch", len(events))
			}
			if err := gs.Verify(ctx, stream); err != nil {
				t.Fatalf("Verify chain from Postgres: %v", err)
			}
		})
	}
}
