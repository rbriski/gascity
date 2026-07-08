//go:build integration

package beads

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/graphstore/pgqmark"
)

// pgFacadeTestDSN returns the Postgres DSN for the beads façade concurrency arm, or
// skips cleanly when none is configured — the same gate the graphstore Postgres arm
// uses (GRAPHSTORE_PG_DSN primary; GC_GRAPH_TEST_PG_DSN alias).
func pgFacadeTestDSN(t *testing.T) string {
	t.Helper()
	for _, env := range []string{"GRAPHSTORE_PG_DSN", "GC_GRAPH_TEST_PG_DSN"} {
		if dsn := strings.TrimSpace(os.Getenv(env)); dsn != "" {
			return dsn
		}
	}
	t.Skip("GRAPHSTORE_PG_DSN not set; skipping Postgres façade tests")
	return ""
}

// withSearchPathDSN pins every connection opened from the returned DSN to schema by
// setting the search_path runtime parameter. lib/pq forwards any non-driver
// connection-string key as a startup GUC, so graphstore.OpenPostgres lands its
// tables and operates entirely inside the private schema — the exported opener needs
// no per-connection setup hook. Handles both the postgres:// URL and keyword DSN
// forms lib/pq accepts.
func withSearchPathDSN(dsn, schema string) string {
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		if u, err := url.Parse(dsn); err == nil {
			q := u.Query()
			q.Set("search_path", schema)
			u.RawQuery = q.Encode()
			return u.String()
		}
	}
	return dsn + " search_path=" + schema
}

// newPGJournalStorePair creates a fresh private schema and opens TWO independent
// JournalStores, each over its own graphstore.OpenPostgres write pool, pinned to it.
// Two pools = two processes: the only way to model a genuine cross-process race,
// since a single store's write pool is capped at one connection and would serialize
// the racing goroutines on the pool instead of on the Postgres row/index locks under
// test. It drops the schema in cleanup and skips when no DSN is configured.
func newPGJournalStorePair(t *testing.T, cityID string) (*JournalStore, *JournalStore) {
	t.Helper()
	dsn := pgFacadeTestDSN(t)
	ctx := context.Background()

	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		t.Fatalf("rand: %v", err)
	}
	schema := "gs_beads_" + hex.EncodeToString(raw[:])

	boot, err := sql.Open(pgqmark.DriverName, dsn)
	if err != nil {
		t.Fatalf("bootstrap open: %v", err)
	}
	if _, err := boot.ExecContext(ctx, "CREATE SCHEMA "+schema); err != nil {
		_ = boot.Close()
		t.Fatalf("create schema %s: %v", schema, err)
	}

	spDSN := withSearchPathDSN(dsn, schema)
	gsA, err := graphstore.OpenPostgres(ctx, spDSN, graphstore.Options{CityID: cityID})
	if err != nil {
		_, _ = boot.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = boot.Close()
		t.Fatalf("OpenPostgres A: %v", err)
	}
	gsB, err := graphstore.OpenPostgres(ctx, spDSN, graphstore.Options{CityID: cityID})
	if err != nil {
		_ = gsA.Close()
		_, _ = boot.ExecContext(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE")
		_ = boot.Close()
		t.Fatalf("OpenPostgres B: %v", err)
	}

	t.Cleanup(func() {
		_ = gsA.Close()
		_ = gsB.Close()
		if _, err := boot.ExecContext(context.Background(), "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Logf("cleanup drop schema %s: %v", schema, err)
		}
		_ = boot.Close()
	})
	return NewJournalStore(gsA), NewJournalStore(gsB)
}

// TestPGJournalStoreSetMetadataIfCrossPoolSingleWinner is the HIGH-1 headline gate on
// real Postgres: two independent JournalStore write pools (two processes) race a
// changing SetMetadataIf on ONE bead at the same expected value. The SQL-conditioned
// compare-and-set — a guarded UPDATE (present key) or an ON CONFLICT DO NOTHING
// insert (absent/empty key) — elects exactly one winner on Postgres; a Go-side
// read-then-write would let both processes pass the compare and both write (the S0.4
// silent double-win the control-epoch fence resurrects on a hosted PG city). It is
// the façade analog of the engine's TestPGConcurrentSameHeadAppendCAS. Runs under
// -race.
func TestPGJournalStoreSetMetadataIfCrossPoolSingleWinner(t *testing.T) {
	const key = "gc.control_epoch"

	t.Run("PresentKeyRace", func(t *testing.T) {
		storeA, storeB := newPGJournalStorePair(t, "cas-city")
		// storeA mints and seeds the bead with the expected value; storeB observes the
		// committed bead through its own pool.
		bead, err := storeA.Create(Bead{Title: "cas", Metadata: map[string]string{key: "E"}})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		singleWinnerRace(t, storeA, storeB, bead.ID, key, "E", "A", "B")
	})

	t.Run("AbsentEmptyKeyRace", func(t *testing.T) {
		storeA, storeB := newPGJournalStorePair(t, "cas-city")
		// The key is absent (expected == "" matches absent-or-empty); the racing
		// INSERT ... ON CONFLICT DO NOTHING must still elect exactly one winner.
		bead, err := storeA.Create(Bead{Title: "cas"})
		if err != nil {
			t.Fatalf("create: %v", err)
		}
		singleWinnerRace(t, storeA, storeB, bead.ID, key, "", "first", "second")
	})
}

// singleWinnerRace fires SetMetadataIf(expected, nextA) from storeA and
// SetMetadataIf(expected, nextB) from storeB concurrently on the same bead, then
// asserts exactly one swapped=true, no errors, and that BOTH pools read back the
// winner's value.
func singleWinnerRace(t *testing.T, storeA, storeB *JournalStore, id, key, expected, nextA, nextB string) {
	t.Helper()
	ctx := context.Background()
	stores := [2]*JournalStore{storeA, storeB}
	nexts := [2]string{nextA, nextB}

	type outcome struct {
		next    string
		swapped bool
		err     error
	}
	var results [2]outcome
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			swapped, err := stores[i].SetMetadataIf(ctx, id, key, expected, nexts[i])
			results[i] = outcome{next: nexts[i], swapped: swapped, err: err}
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	winningNext := ""
	for _, r := range results {
		if r.err != nil {
			t.Fatalf("cross-pool SetMetadataIf(next=%q) errored, want at most one winner and never an error: %v", r.next, r.err)
		}
		if r.swapped {
			winners++
			winningNext = r.next
		}
	}
	if winners != 1 {
		t.Fatalf("winners = %d, want exactly 1 — a Go-side compare would let both pass and both write (the S0.4 silent double-win)", winners)
	}

	// Both pools must agree on the final value = the winner's next.
	for name, s := range map[string]*JournalStore{"A": storeA, "B": storeB} {
		got, err := s.Get(id)
		if err != nil {
			t.Fatalf("get via store %s: %v", name, err)
		}
		if got.Metadata[key] != winningNext {
			t.Fatalf("store %s final value = %q, want the winner's %q", name, got.Metadata[key], winningNext)
		}
	}
}
