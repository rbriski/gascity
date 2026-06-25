package beads_test

import (
	"database/sql"
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
// classifies beads identically to the bd/SQLite backends.
//
// SKIPPED unless GC_TEST_POSTGRES_DSN points at a DISPOSABLE Postgres database —
// the suite truncates the bead tables between factory calls, so never aim it at a
// real store. This is the harness to run while porting the scaffolded
// PostgresStore methods from sqlite_store.go; today every method is a stub, so the
// suite fails fast until they are implemented (which is the point — it is the
// executable definition of "done").
func TestPostgresStoreSatisfiesClassedStoreConformance(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres to run the conformance suite")
	}
	coordtest.RunClassedStoreTestsWithOptions(t, coordclass.ClassOrders,
		func() beads.Store {
			s, err := beads.OpenPostgresStore(dsn)
			if err != nil {
				t.Fatalf("OpenPostgresStore: %v", err)
			}
			truncatePostgresBeadTables(t, dsn) // clean slate per factory call
			t.Cleanup(func() {
				if c, ok := s.(interface{ CloseStore() error }); ok {
					_ = c.CloseStore() //nolint:errcheck // best-effort
				}
			})
			return s
		},
		coordtest.Options{Skip: false})
}

func truncatePostgresBeadTables(t *testing.T, dsn string) {
	t.Helper()
	db, err := sql.Open("postgres", dsn)
	if err != nil {
		t.Fatalf("truncate: open: %v", err)
	}
	defer db.Close() //nolint:errcheck // best-effort
	if _, err := db.Exec(`TRUNCATE beads, labels, metadata, deps, kv CASCADE`); err != nil {
		t.Fatalf("truncate bead tables (DSN must point at a disposable Postgres): %v", err)
	}
}
