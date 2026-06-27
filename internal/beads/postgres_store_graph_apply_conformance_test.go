package beads_test

import (
	"os"
	"testing"

	_ "github.com/lib/pq"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordclass/coordtest"
)

// TestPostgresStoreSatisfiesGraphStoreConformance runs the SHARED GraphStore
// conformance suite (the same coordtest.RunGraphStoreTests every graph backend
// must pass) against a real Postgres, un-skipped — proving its ApplyGraphPlan
// seam pours a graph identically to the bd/SQLite backends. This is the parity
// that unblocks graph=postgres: without ApplyGraphPlan on *PostgresStore a
// Postgres-backed graph class cannot perform the atomic topology pour.
//
// SKIPPED unless GC_TEST_POSTGRES_DSN points at a DISPOSABLE Postgres database —
// the factory truncates the bead tables between calls, so never aim it at a real
// store.
func TestPostgresStoreSatisfiesGraphStoreConformance(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres to run the conformance suite")
	}
	const schema = "gcg_conformance"
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	coordtest.RunGraphStoreTestsWithOptions(t,
		func() beads.GraphApplyStore {
			truncatePostgresSchema(t, dsn, schema) // clean slate per factory call
			s, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema))
			if err != nil {
				t.Fatalf("OpenPostgresStore: %v", err)
			}
			store := s.(*beads.PostgresStore)
			t.Cleanup(func() { _ = store.CloseStore() })
			return store
		},
		coordtest.Options{Skip: false})
}
