package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/benchmarks/coordstore"
)

func TestOpenUsesProductionSQLiteSettings(t *testing.T) {
	ctx := context.Background()
	a := openTestAdapter(ctx, t, coordstore.Config{DataDir: t.TempDir()})

	if got := a.readDB.Stats().MaxOpenConnections; got != 8 {
		t.Fatalf("read pool max open connections = %d, want 8", got)
	}
	if got := a.writeDB.Stats().MaxOpenConnections; got != 1 {
		t.Fatalf("write pool max open connections = %d, want 1", got)
	}

	if got := queryStringPragma(ctx, t, a.writeDB, "journal_mode"); got != "wal" {
		t.Fatalf("journal_mode = %q, want wal", got)
	}
	if got := queryIntPragma(ctx, t, a.writeDB, "synchronous"); got != 2 {
		t.Fatalf("synchronous = %d, want 2 (FULL)", got)
	}
	if got := queryIntPragma(ctx, t, a.writeDB, "wal_autocheckpoint"); got != 1000 {
		t.Fatalf("wal_autocheckpoint = %d, want 1000", got)
	}
}

func TestPoolEightConcurrentAccessHasNoErrors(t *testing.T) {
	ctx := context.Background()
	a := openTestAdapter(ctx, t, coordstore.Config{DataDir: t.TempDir()})

	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				r, err := a.Create(ctx, coordstore.Record{
					Title:    fmt.Sprintf("worker-%d-%d", worker, i),
					Status:   "open",
					Type:     "task",
					Assignee: fmt.Sprintf("agent-%d", worker),
				})
				if err != nil {
					errs <- fmt.Errorf("create: %w", err)
					return
				}
				if _, err := a.Get(ctx, r.ID); err != nil {
					errs <- fmt.Errorf("get: %w", err)
					return
				}
				if _, err := a.FilterScan(ctx, coordstore.Query{Assignee: r.Assignee, Limit: 5}); err != nil {
					errs <- fmt.Errorf("filter scan: %w", err)
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
}

func TestPurgeTerminalRemovesOnlyOldTerminalMainRecords(t *testing.T) {
	ctx := context.Background()
	a := openTestAdapter(ctx, t, coordstore.Config{DataDir: t.TempDir()})
	old := time.Now().Add(-5 * time.Hour)
	recent := time.Now()
	olderThan := 4 * time.Hour

	oldClosed := mustCreateRecord(ctx, t, a, coordstore.Record{
		ID:        "old-closed",
		Title:     "old closed",
		Status:    "closed",
		Type:      "task",
		CreatedAt: old,
		Labels:    []string{"purge-me"},
		Metadata:  map[string]string{"scope": "terminal"},
	})
	oldCancelled := mustCreateRecord(ctx, t, a, coordstore.Record{
		ID:        "old-canceled",
		Title:     "old canceled",
		Status:    "canceled",
		Type:      "task",
		CreatedAt: old,
	})
	recentClosed := mustCreateRecord(ctx, t, a, coordstore.Record{
		ID:        "recent-closed",
		Title:     "recent closed",
		Status:    "closed",
		Type:      "task",
		CreatedAt: recent,
	})
	oldButUpdatedRecently := mustCreateRecord(ctx, t, a, coordstore.Record{
		ID:        "old-updated-recently",
		Title:     "old updated recently",
		Status:    "closed",
		Type:      "task",
		CreatedAt: old,
		UpdatedAt: recent,
	})
	oldOpen := mustCreateRecord(ctx, t, a, coordstore.Record{
		ID:        "old-open",
		Title:     "old open",
		Status:    "open",
		Type:      "task",
		CreatedAt: old,
	})
	if err := a.DepAdd(ctx, oldOpen.ID, oldClosed.ID, "blocks"); err != nil {
		t.Fatalf("add dep: %v", err)
	}

	purged, err := a.PurgeTerminal(ctx, olderThan)
	if err != nil {
		t.Fatalf("PurgeTerminal: %v", err)
	}
	if purged != 2 {
		t.Fatalf("PurgeTerminal purged %d records, want 2", purged)
	}

	for _, id := range []string{oldClosed.ID, oldCancelled.ID} {
		if _, err := a.Get(ctx, id); !errors.Is(err, coordstore.ErrNotFound) {
			t.Fatalf("Get(%q) error = %v, want ErrNotFound", id, err)
		}
	}
	for _, id := range []string{recentClosed.ID, oldButUpdatedRecently.ID, oldOpen.ID} {
		if _, err := a.Get(ctx, id); err != nil {
			t.Fatalf("Get(%q): %v", id, err)
		}
	}
	deps, err := a.DepList(ctx, oldOpen.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("deps after purging old terminal target = %v, want none", deps)
	}
}

func TestRetentionSweepStartsFromConfig(t *testing.T) {
	ctx := context.Background()
	a := openTestAdapter(ctx, t, coordstore.Config{
		DataDir: t.TempDir(),
		Extra: map[string]string{
			"retention_period":         "1ms",
			"retention_sweep_interval": "5ms",
		},
	})

	r := mustCreateRecord(ctx, t, a, coordstore.Record{
		ID:        "sweep-me",
		Title:     "sweep me",
		Status:    "closed",
		Type:      "task",
		CreatedAt: time.Now().Add(-time.Hour),
	})

	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		_, err := a.Get(ctx, r.ID)
		if errors.Is(err, coordstore.ErrNotFound) {
			return
		}
		if err != nil {
			t.Fatalf("Get(%q): %v", r.ID, err)
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("retention sweep did not purge %q before deadline", r.ID)
}

func TestSQLiteWALAutoCheckpointBoundsLog(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	a := openTestAdapter(ctx, t, coordstore.Config{DataDir: dir})

	for i := 0; i < 1200; i++ {
		mustCreateRecord(ctx, t, a, coordstore.Record{
			Title:  fmt.Sprintf("wal-%d", i),
			Status: "open",
			Type:   "task",
		})
	}
	if _, err := a.writeDB.ExecContext(ctx, "PRAGMA wal_checkpoint(PASSIVE)"); err != nil {
		t.Fatalf("wal checkpoint: %v", err)
	}
	info, err := os.Stat(filepath.Join(dir, "store.db-wal"))
	if err != nil {
		t.Fatalf("stat wal file: %v", err)
	}
	const maxWALSize = 8 << 20
	if info.Size() > maxWALSize {
		t.Fatalf("wal size = %d bytes, want <= %d", info.Size(), maxWALSize)
	}
}

func TestOpenRecoversGeneratedIDSequence(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()

	first := New()
	if err := first.Open(ctx, coordstore.Config{DataDir: dir}); err != nil {
		t.Fatalf("first open: %v", err)
	}
	created, err := first.Create(ctx, coordstore.Record{Title: "first", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if created.ID != "sq-1" {
		t.Fatalf("first generated ID = %q, want sq-1", created.ID)
	}
	if err := first.Close(); err != nil {
		t.Fatalf("first close: %v", err)
	}

	second := New()
	if err := second.Open(ctx, coordstore.Config{DataDir: dir}); err != nil {
		t.Fatalf("second open: %v", err)
	}
	t.Cleanup(func() {
		if err := second.Close(); err != nil {
			t.Fatalf("second close: %v", err)
		}
	})
	next, err := second.Create(ctx, coordstore.Record{Title: "second", Status: "open", Type: "task"})
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if next.ID != "sq-2" {
		t.Fatalf("next generated ID = %q, want sq-2", next.ID)
	}
}

func TestPurgeTerminalDeletesOldTerminalMainRecords(t *testing.T) {
	ctx := context.Background()
	adapter := New()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck

	now := time.Now()
	old := now.Add(-2 * time.Hour)
	recent := now.Add(-30 * time.Minute)

	oldClosed := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old closed",
		Status:    "closed",
		Type:      "task",
		CreatedAt: old,
		Labels:    []string{"stale"},
		Metadata:  map[string]string{"kind": "terminal"},
	})
	oldCancelled := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old canceled",
		Status:    "canceled",
		Type:      "task",
		CreatedAt: old,
	})
	oldExpired := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old expired",
		Status:    "expired",
		Type:      "task",
		CreatedAt: old,
	})
	recentClosed := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "recent closed",
		Status:    "closed",
		Type:      "task",
		CreatedAt: recent,
	})
	oldOpen := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "old open",
		Status:    "open",
		Type:      "task",
		CreatedAt: old,
	})
	depTarget := mustCreateTerminalTestRecord(ctx, t, adapter, coordstore.Record{
		Title:     "dep target",
		Status:    "open",
		Type:      "task",
		CreatedAt: old,
	})
	if err := adapter.DepAdd(ctx, oldClosed.ID, depTarget.ID, "blocks"); err != nil {
		t.Fatalf("DepAdd: %v", err)
	}

	purged, err := adapter.PurgeTerminal(ctx, time.Hour)
	if err != nil {
		t.Fatalf("PurgeTerminal: %v", err)
	}
	if purged != 3 {
		t.Fatalf("PurgeTerminal purged %d records, want 3", purged)
	}
	for _, id := range []string{oldClosed.ID, oldCancelled.ID, oldExpired.ID} {
		if _, err := adapter.Get(ctx, id); !coordstore.IsNotFound(err) {
			t.Fatalf("Get(%s) error = %v, want not found", id, err)
		}
	}
	for _, id := range []string{recentClosed.ID, oldOpen.ID, depTarget.ID} {
		if _, err := adapter.Get(ctx, id); err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
	}

	if count := countTerminalTestRows(t, adapter, "labels", oldClosed.ID); count != 0 {
		t.Fatalf("labels rows for purged record = %d, want 0", count)
	}
	if count := countTerminalTestRows(t, adapter, "metadata", oldClosed.ID); count != 0 {
		t.Fatalf("metadata rows for purged record = %d, want 0", count)
	}
	deps, err := adapter.DepList(ctx, oldClosed.ID, "down")
	if err != nil {
		t.Fatalf("DepList: %v", err)
	}
	if len(deps) != 0 {
		t.Fatalf("deps for purged record = %d, want 0", len(deps))
	}
}

func TestGetReturnsFreshUpdatedAtAfterMutation(t *testing.T) {
	ctx := context.Background()
	adapter := New()
	if err := adapter.Open(ctx, coordstore.Config{DataDir: t.TempDir()}); err != nil {
		t.Fatalf("Open: %v", err)
	}
	defer adapter.Close() //nolint:errcheck

	// Seed with an explicit past CreatedAt so the mutation's wall-clock
	// updated_at is unambiguously later than the original timestamps.
	created, err := adapter.Create(ctx, coordstore.Record{
		Title:     "mutating record",
		Status:    "open",
		Type:      "task",
		CreatedAt: time.Now().Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if !created.UpdatedAt.Equal(created.CreatedAt) {
		t.Fatalf("new record UpdatedAt = %v, want equal to CreatedAt %v", created.UpdatedAt, created.CreatedAt)
	}

	if err := adapter.SetMetadataBatch(ctx, created.ID, map[string]string{"phase": "running"}); err != nil {
		t.Fatalf("SetMetadataBatch: %v", err)
	}

	got, err := adapter.Get(ctx, created.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if !got.UpdatedAt.After(got.CreatedAt) {
		t.Fatalf("Get UpdatedAt = %v, want strictly after CreatedAt %v", got.UpdatedAt, got.CreatedAt)
	}
	if !got.UpdatedAt.After(created.UpdatedAt) {
		t.Fatalf("Get UpdatedAt = %v, want strictly after original UpdatedAt %v", got.UpdatedAt, created.UpdatedAt)
	}
}

func mustCreateTerminalTestRecord(ctx context.Context, t *testing.T, adapter *Adapter, r coordstore.Record) coordstore.Record {
	t.Helper()
	created, err := adapter.Create(ctx, r)
	if err != nil {
		t.Fatalf("Create(%q): %v", r.Title, err)
	}
	return created
}

func openTestAdapter(ctx context.Context, t *testing.T, cfg coordstore.Config) *Adapter {
	t.Helper()
	a := New()
	if err := a.Open(ctx, cfg); err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := a.Close(); err != nil {
			t.Fatalf("Close: %v", err)
		}
	})
	return a
}

func mustCreateRecord(ctx context.Context, t *testing.T, a *Adapter, r coordstore.Record) coordstore.Record {
	t.Helper()
	created, err := a.Create(ctx, r)
	if err != nil {
		t.Fatalf("Create(%q): %v", r.ID, err)
	}
	return created
}

func countTerminalTestRows(t *testing.T, adapter *Adapter, table, recordID string) int {
	t.Helper()
	var count int
	if err := adapter.readDB.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE record_id=?", recordID).Scan(&count); err != nil {
		t.Fatalf("count %s rows: %v", table, err)
	}
	return count
}

func queryIntPragma(ctx context.Context, t *testing.T, db *sql.DB, name string) int {
	t.Helper()
	var got int
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return got
}

func queryStringPragma(ctx context.Context, t *testing.T, db *sql.DB, name string) string {
	t.Helper()
	var got string
	if err := db.QueryRowContext(ctx, "PRAGMA "+name).Scan(&got); err != nil {
		t.Fatalf("PRAGMA %s: %v", name, err)
	}
	return got
}
