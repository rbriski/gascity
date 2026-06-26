package beads_test

import (
	"database/sql"
	"fmt"
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass"
	"github.com/gastownhall/gascity/internal/coordrouter/coordtest"
)

// TestPostgresStoreSatisfiesClassedStoreConformance runs the SHARED classed-store
// conformance suite (the same coordtest.RunClassedStoreTests every backend must
// pass) against a real Postgres, proving the PostgresStore round-trips and
// classifies beads identically to the bd/SQLite backends — in its own provisioned
// schema (the per-class isolation model).
//
// SKIPPED unless GC_TEST_POSTGRES_DSN points at a DISPOSABLE Postgres database —
// the suite truncates the bead tables between factory calls, so never aim it at a
// real store.
func TestPostgresStoreSatisfiesClassedStoreConformance(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres to run the conformance suite")
	}
	const schema = "gco_conformance"
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	coordtest.RunClassedStoreTestsWithOptions(t, coordclass.ClassOrders,
		func() beads.Store {
			truncatePostgresSchema(t, dsn, schema) // clean slate per factory call
			s, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema))
			if err != nil {
				t.Fatalf("OpenPostgresStore: %v", err)
			}
			t.Cleanup(func() {
				if c, ok := s.(interface{ CloseStore() error }); ok {
					_ = c.CloseStore() //nolint:errcheck // best-effort
				}
			})
			return s
		},
		coordtest.Options{Skip: false})
}

// TestPostgresStoreSatisfiesClassedStoreConformanceForSessions runs the shared
// classed-store conformance suite for ClassSessions against a real Postgres,
// proving the gcs-schema backend round-trips and classifies session-class beads —
// the storage requirement for relocating sessions onto Postgres at cutover.
//
// SKIPPED unless GC_TEST_POSTGRES_DSN points at a DISPOSABLE Postgres database.
func TestPostgresStoreSatisfiesClassedStoreConformanceForSessions(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres to run the conformance suite")
	}
	const schema = "gcs_conformance"
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	coordtest.RunClassedStoreTestsWithOptions(t, coordclass.ClassSessions,
		func() beads.Store {
			truncatePostgresSchema(t, dsn, schema) // clean slate per factory call
			s, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema))
			if err != nil {
				t.Fatalf("OpenPostgresStore: %v", err)
			}
			t.Cleanup(func() {
				if c, ok := s.(interface{ CloseStore() error }); ok {
					_ = c.CloseStore() //nolint:errcheck // best-effort
				}
			})
			return s
		},
		coordtest.Options{Skip: false})
}

// truncatePostgresSchema clears a provisioned class schema between runs and resets
// its id sequence so minted ids are deterministic. schema is a controlled test
// identifier (a reserved-prefix-style name), not user input.
func truncatePostgresSchema(t *testing.T, dsn, schema string) {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("truncate: open: %v", err)
	}
	defer db.Close() //nolint:errcheck // best-effort
	if _, err := db.Exec(fmt.Sprintf(`TRUNCATE %[1]s.beads, %[1]s.labels, %[1]s.metadata, %[1]s.deps, %[1]s.kv CASCADE`, schema)); err != nil {
		t.Fatalf("truncate schema %q (DSN must point at a disposable Postgres): %v", schema, err)
	}
	if _, err := db.Exec(fmt.Sprintf(`ALTER SEQUENCE %s.bead_seq RESTART WITH 1`, schema)); err != nil {
		t.Fatalf("reset sequence for schema %q: %v", schema, err)
	}
}
