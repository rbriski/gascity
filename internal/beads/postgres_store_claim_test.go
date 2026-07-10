package beads_test

import (
	"os"
	"sync"
	"sync/atomic"
	"testing"

	_ "github.com/lib/pq"

	"github.com/gastownhall/gascity/internal/beads"
)

// TestPostgresStoreClaimIsSingleWinnerUnderConcurrency proves PostgresStore.Claim's
// SELECT ... FOR UPDATE row lock makes a claim a single-winner compare-and-set even
// though the store serves concurrent pooled connections (unlike SQLite's single
// write connection). Many workers race to claim the SAME open bead; exactly one
// must win (ok=true) and every other must lose cleanly (ok=false, no error, no
// double-claim). This is the concurrency guarantee the graph-resident work-claim
// path depends on once the graph class is relocated onto Postgres.
//
// SKIPPED unless GC_TEST_POSTGRES_DSN points at a DISPOSABLE Postgres database.
func TestPostgresStoreClaimIsSingleWinnerUnderConcurrency(t *testing.T) {
	dsn := os.Getenv("GC_TEST_POSTGRES_DSN")
	if dsn == "" {
		t.Skip("set GC_TEST_POSTGRES_DSN to a disposable Postgres to run the claim concurrency test")
	}
	const schema = "gco_claim_race"
	if err := beads.ProvisionPostgres(dsn, schema); err != nil {
		t.Fatalf("ProvisionPostgres(%q): %v", schema, err)
	}
	truncatePostgresSchema(t, dsn, schema)

	store, err := beads.OpenPostgresStore(dsn, beads.WithPostgresStoreSchema(schema))
	if err != nil {
		t.Fatalf("OpenPostgresStore: %v", err)
	}
	t.Cleanup(func() {
		if c, ok := store.(interface{ CloseStore() error }); ok {
			_ = c.CloseStore() //nolint:errcheck
		}
	})
	claimer, ok := store.(beads.Claimer)
	if !ok {
		t.Fatalf("PostgresStore does not implement beads.Claimer — the ErrClaimUnsupported gap is back")
	}

	created, err := store.Create(beads.Bead{Title: "contended", Type: "task"})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}

	const workers = 24
	var wins int64
	var wg sync.WaitGroup
	start := make(chan struct{})
	winner := make([]string, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			assignee := "worker-" + string(rune('a'+n%26)) + string(rune('0'+n/26))
			<-start // release all goroutines at once to maximize contention
			claimed, ok, err := claimer.Claim(created.ID, assignee)
			if err != nil {
				t.Errorf("worker %d Claim: %v", n, err)
				return
			}
			if ok {
				atomic.AddInt64(&wins, 1)
				winner[n] = claimed.Assignee
			}
		}(i)
	}
	close(start)
	wg.Wait()

	if wins != 1 {
		t.Fatalf("concurrent claims produced %d winners, want exactly 1 (FOR UPDATE must serialize)", wins)
	}
	got, err := store.Get(created.ID)
	if err != nil {
		t.Fatalf("Get after race: %v", err)
	}
	if got.Status != "in_progress" || got.Assignee == "" {
		t.Fatalf("post-race bead = {status:%q assignee:%q}, want in_progress + a winner", got.Status, got.Assignee)
	}
	// The persisted assignee must be the single goroutine that observed ok=true.
	found := false
	for _, w := range winner {
		if w == got.Assignee {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("persisted assignee %q was not reported ok=true by any worker", got.Assignee)
	}
}
