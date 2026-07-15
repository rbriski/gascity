//go:build integration

package beads

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	beadslib "github.com/steveyegge/beads"
)

func TestNativeDoltAtomicReadWriteTerminalUpdateOnRealBackend(t *testing.T) {
	ctx := t.Context()
	beadsDir := filepath.Join(t.TempDir(), ".beads")
	storage, err := beadslib.OpenBestAvailable(ctx, beadsDir)
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := NewNativeDoltStoreWithStorageForTesting(storage)
	capability, ok := AtomicReadWriteFor(store)
	if !ok {
		t.Fatal("AtomicReadWriteFor(real NativeDolt) = false")
	}
	if err := store.PrepareAtomicReadSnapshot(ctx); err != nil {
		t.Fatalf("PrepareAtomicReadSnapshot: %v", err)
	}

	const id = "gc-atomic-terminal"
	if err := capability.AtomicReadWrite(ctx, "test: create durable row", func(tx AtomicReadWriteTx) error {
		_, err := tx.Create(Bead{
			ID:       id,
			Title:    "atomic terminal row",
			Status:   "open",
			Type:     "task",
			Assignee: "gc-partition-test",
		})
		return err
	}); err != nil {
		t.Fatalf("create durable row: %v", err)
	}

	closed := "closed"
	if err := capability.AtomicReadWrite(ctx, "test: terminalize durable row", func(tx AtomicReadWriteTx) error {
		return tx.Update(id, UpdateOpts{Status: &closed})
	}); err != nil {
		t.Fatalf("terminalize durable row: %v", err)
	}

	got, err := store.Get(id)
	if err != nil {
		t.Fatalf("read terminal row: %v", err)
	}
	if got.Status != closed {
		t.Fatalf("terminal row status = %q, want %q", got.Status, closed)
	}
}

func TestNativeDoltAtomicReadSnapshotRealBackendIsStableAcrossPages(t *testing.T) {
	ctx := t.Context()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	reader := newNativeDoltStoreWithStorageAndPrefix(storage, "snapshot-reader", "gc")
	writer := newNativeDoltStoreWithStorageAndPrefix(storage, "snapshot-writer", "gc")
	if err := reader.PrepareAtomicReadSnapshot(ctx); err != nil {
		t.Fatalf("PrepareAtomicReadSnapshot: %v", err)
	}
	if err := reader.PrepareAtomicReadSnapshot(ctx); err != nil {
		t.Fatalf("idempotent PrepareAtomicReadSnapshot: %v", err)
	}
	for _, id := range []string{"gc-snapshot-a", "gc-snapshot-b"} {
		if _, err := writer.Create(Bead{ID: id, Title: id}); err != nil {
			t.Fatalf("Create seed %s: %v", id, err)
		}
	}

	capability, ok := AtomicReadSnapshotFor(reader)
	if !ok {
		t.Fatal("AtomicReadSnapshotFor(real NativeDoltStore) = false, want true")
	}
	query := AtomicReadSnapshotPageQuery{IDPrefix: "gc-snapshot-", Status: "open", Order: AtomicReadSnapshotOrderID, Limit: 1}
	var stableIDs []string
	writeStarted := make(chan struct{})
	writeResult := make(chan error, 1)
	if err := capability.AtomicReadSnapshot(ctx, func(tx AtomicReadSnapshotTx) error {
		page, err := tx.ListHistoryPage(query)
		if err != nil {
			return err
		}
		if len(page.Rows) != 1 || page.Next == (AtomicReadSnapshotCursor{}) {
			return fmt.Errorf("first page = %#v, want one row and continuation", page)
		}
		stableIDs = append(stableIDs, page.Rows[0].ID)

		go func() {
			close(writeStarted)
			_, err := writer.Create(Bead{ID: "gc-snapshot-z", Title: "concurrent"})
			writeResult <- err
		}()
		<-writeStarted
		for page.Next != (AtomicReadSnapshotCursor{}) {
			query.After = page.Next
			page, err = tx.ListHistoryPage(query)
			if err != nil {
				return err
			}
			for _, row := range page.Rows {
				stableIDs = append(stableIDs, row.ID)
			}
		}
		return nil
	}); err != nil {
		t.Fatalf("AtomicReadSnapshot: %v", err)
	}
	if !slices.Equal(stableIDs, []string{"gc-snapshot-a", "gc-snapshot-b"}) {
		t.Fatalf("stable snapshot IDs = %v, want pre-write rows only", stableIDs)
	}
	if err := <-writeResult; err != nil {
		t.Fatalf("concurrent second-store Create: %v", err)
	}

	query.After = AtomicReadSnapshotCursor{}
	var freshIDs []string
	if err := capability.AtomicReadSnapshot(ctx, func(tx AtomicReadSnapshotTx) error {
		for {
			page, err := tx.ListHistoryPage(query)
			if err != nil {
				return err
			}
			for _, row := range page.Rows {
				freshIDs = append(freshIDs, row.ID)
			}
			if page.Next == (AtomicReadSnapshotCursor{}) {
				return nil
			}
			query.After = page.Next
		}
	}); err != nil {
		t.Fatalf("fresh AtomicReadSnapshot: %v", err)
	}
	if !slices.Equal(freshIDs, []string{"gc-snapshot-a", "gc-snapshot-b", "gc-snapshot-z"}) {
		t.Fatalf("fresh snapshot IDs = %v, want concurrent row visible", freshIDs)
	}
}

func TestNativeDoltAtomicReadSnapshotFiltersExactAssigneeWithOwnedIndex(t *testing.T) {
	ctx := t.Context()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "partition-snapshot", "gc")
	if err := store.PrepareAtomicReadSnapshot(ctx); err != nil {
		t.Fatalf("PrepareAtomicReadSnapshot: %v", err)
	}

	db, cleanup, err := openNativeDoltSnapshotDB(ctx, storage)
	if err != nil {
		t.Fatalf("open snapshot database for index verification: %v", err)
	}
	columns, _, present, err := nativeDoltSnapshotIndexDefinition(ctx, db, nativeDoltAssigneeStatusIDSnapshotIndex)
	if err != nil {
		t.Fatalf("read partition snapshot index: %v", err)
	}
	explainRows, err := db.QueryContext(ctx, `
		EXPLAIN FORMAT=TREE
		SELECT id
		FROM issues FORCE INDEX (`+nativeDoltAssigneeStatusIDSnapshotIndex+`)
		WHERE assignee = ? AND status = ? AND id LIKE ?
		ORDER BY id ASC
		LIMIT ?
	`, "partition-owned", "open", "gc-partition-%", 2)
	if err != nil {
		t.Fatalf("explain exact-assignee snapshot query: %v", err)
	}
	var plan strings.Builder
	for explainRows.Next() {
		var line string
		if err := explainRows.Scan(&line); err != nil {
			_ = explainRows.Close()
			t.Fatalf("scan exact-assignee snapshot plan: %v", err)
		}
		plan.WriteString(line)
		plan.WriteByte('\n')
	}
	if err := explainRows.Err(); err != nil {
		_ = explainRows.Close()
		t.Fatalf("iterate exact-assignee snapshot plan: %v", err)
	}
	if err := explainRows.Close(); err != nil {
		t.Fatalf("close exact-assignee snapshot plan: %v", err)
	}
	planText := plan.String()
	if !strings.Contains(planText, "[issues.assignee,issues.status,issues.id]") {
		t.Fatalf("exact-assignee snapshot plan does not prove the owned index columns:\n%s", planText)
	}
	// Dolt may retain a bounded TopN for LIMIT even though the index already
	// carries id order. A table scan or unbounded Sort would violate the
	// partition-pushdown contract; the bounded TopN does not.
	for _, forbidden := range []string{"TableScan", "Sort("} {
		if strings.Contains(planText, forbidden) {
			t.Fatalf("exact-assignee snapshot plan contains unbounded %s operator:\n%s", forbidden, planText)
		}
	}
	if err := cleanup(); err != nil {
		t.Fatalf("close snapshot database after index verification: %v", err)
	}
	if !present || columns != "assignee,status,id" {
		t.Fatalf("partition snapshot index present/columns = %t/%q, want true/assignee,status,id", present, columns)
	}

	for _, bead := range []Bead{
		{ID: "gc-partition-a-foreign", Title: "foreign", Assignee: "partition-foreign"},
		{ID: "gc-partition-b-owned", Title: "owned", Assignee: "partition-owned"},
		{ID: "gc-partition-c-foreign", Title: "foreign", Assignee: "partition-foreign"},
	} {
		if _, err := store.Create(bead); err != nil {
			t.Fatalf("Create %s: %v", bead.ID, err)
		}
	}
	query := AtomicReadSnapshotPageQuery{
		IDPrefix: "gc-partition-",
		Status:   "open",
		Order:    AtomicReadSnapshotOrderID,
		Limit:    2,
	}
	setAtomicSnapshotAssigneeForTest(t, &query, "partition-owned")
	if err := store.AtomicReadSnapshot(ctx, func(tx AtomicReadSnapshotTx) error {
		page, err := tx.ListHistoryPage(query)
		if err != nil {
			return err
		}
		if len(page.Rows) != 1 || page.Rows[0].ID != "gc-partition-b-owned" || page.Rows[0].Assignee != "partition-owned" {
			return fmt.Errorf("exact-assignee page = %#v, want only owned row", page)
		}
		if page.Next != (AtomicReadSnapshotCursor{}) {
			return fmt.Errorf("exact-assignee page continuation = %#v, want exhausted", page.Next)
		}
		return nil
	}); err != nil {
		t.Fatalf("AtomicReadSnapshot exact assignee: %v", err)
	}
}

func TestNativeDoltAtomicReadSnapshotFailsClosedOnPagingIndexSkew(t *testing.T) {
	tests := map[string]struct {
		index       string
		replacement string
	}{
		"missing updated-at index":      {index: "idx_issues_status_updated_at"},
		"wrong updated-at columns":      {index: "idx_issues_status_updated_at", replacement: "CREATE INDEX idx_issues_status_updated_at ON issues (status)"},
		"missing status-id index":       {index: nativeDoltStatusIDSnapshotIndex},
		"wrong status-id index columns": {index: nativeDoltStatusIDSnapshotIndex, replacement: "CREATE INDEX " + nativeDoltStatusIDSnapshotIndex + " ON issues (id)"},
		"missing assignee-status-id index": {
			index: nativeDoltAssigneeStatusIDSnapshotIndex,
		},
		"wrong assignee-status-id index columns": {
			index:       nativeDoltAssigneeStatusIDSnapshotIndex,
			replacement: "CREATE INDEX " + nativeDoltAssigneeStatusIDSnapshotIndex + " ON issues (status, id)",
		},
	}
	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
			if err != nil {
				t.Skipf("upstream native beads storage unavailable: %v", err)
			}
			t.Cleanup(func() {
				if err := storage.Close(); err != nil {
					t.Fatalf("close upstream storage: %v", err)
				}
			})
			preparer := newNativeDoltStoreWithStorageAndPrefix(storage, "snapshot-index-preparer", "gc")
			if err := preparer.PrepareAtomicReadSnapshot(ctx); err != nil {
				t.Fatalf("PrepareAtomicReadSnapshot: %v", err)
			}
			db, cleanup, err := openNativeDoltSnapshotDB(ctx, storage)
			if err != nil {
				t.Fatalf("open snapshot database for schema skew: %v", err)
			}
			if _, err := db.Exec("DROP INDEX " + test.index + " ON issues"); err != nil {
				t.Fatalf("drop paging index: %v", err)
			}
			if test.replacement != "" {
				if _, err := db.Exec(test.replacement); err != nil {
					t.Fatalf("create skewed paging index: %v", err)
				}
			}
			if err := cleanup(); err != nil {
				t.Fatalf("close snapshot database after schema skew: %v", err)
			}

			store := newNativeDoltStoreWithStorageAndPrefix(storage, "snapshot-index-skew", "gc")
			called := false
			err = store.AtomicReadSnapshot(ctx, func(AtomicReadSnapshotTx) error {
				called = true
				return nil
			})
			if !errors.Is(err, ErrAtomicReadSnapshotUnsupported) {
				t.Fatalf("AtomicReadSnapshot error = %v, want ErrAtomicReadSnapshotUnsupported", err)
			}
			if called {
				t.Fatal("AtomicReadSnapshot called callback with missing/skewed paging index")
			}
		})
	}
}

// TestNativeDoltStoreRegularUpdateEventRecording verifies that calling
// SetMetadata on a non-ephemeral bead succeeds. This exercises
// RecordEventInTable on the regular events table, which regresses when the
// INSERT omits the id column and the live schema has no DEFAULT for it.
func TestNativeDoltStoreRegularUpdateEventRecording(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "update-event-regression", "gc")

	bead, err := store.Create(Bead{Title: "regular update event regression bead"})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}
	if bead.Ephemeral {
		t.Fatalf("Ephemeral = true on regular bead, want false")
	}
	if err := store.SetMetadata(bead.ID, "gc.routed_to", "gascity/builder"); err != nil {
		t.Fatalf("SetMetadata: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get bead after SetMetadata: %v", err)
	}
	if got.Metadata["gc.routed_to"] != "gascity/builder" {
		t.Fatalf("Metadata[gc.routed_to] = %q, want %q", got.Metadata["gc.routed_to"], "gascity/builder")
	}
}

// TestNativeDoltStoreEphemeralMailSend verifies that creating an ephemeral message
// bead (the gc mail send code path) succeeds through the upstream beads library.
//
// Regression tripwire for the 2026-06-11 P0 incident: a beads version-skew broke
// gc mail send with "Field 'id' doesn't have a default value" because a newer
// schema migration dropped DEFAULT (UUID()) from wisp_events.id while the linked
// beads code still omitted id on INSERT. Released beads v1.0.5 is coherent, so
// this test PASSES today. It FAILS if a future go.mod upgrade ships a version
// where code and schema disagree on wisp_events.id.
func TestNativeDoltStoreEphemeralMailSend(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "mail-wisp-regression", "gc")

	// Create an ephemeral message bead — the beadmail.Send() path.
	// Ephemeral=true routes the INSERT to wisps + wisp_events tables.
	// A NOT NULL / missing-DEFAULT failure here reproduces the 2026-06-11 incident.
	sent, err := store.Create(Bead{
		Title:     "hello from mail regression",
		Type:      "message",
		Assignee:  "builder",
		Ephemeral: true,
	})
	if err != nil {
		t.Fatalf("Create ephemeral message bead (wisp_events INSERT): %v", err)
	}
	if !sent.Ephemeral {
		t.Fatalf("Ephemeral = false on returned bead %s, want true", sent.ID)
	}
	if sent.ID == "" {
		t.Fatal("returned bead has empty ID")
	}

	// List with TierWisps to confirm the bead is retrievable after the INSERT.
	results, err := store.List(ListQuery{
		TierMode: TierWisps,
		Assignee: "builder",
	})
	if err != nil {
		t.Fatalf("List wisp beads: %v", err)
	}
	var found bool
	for _, b := range results {
		if b.ID == sent.ID {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("created wisp bead %s not in List(TierWisps); got %d beads total", sent.ID, len(results))
	}
}

// TestNativeDoltStoreEventsIDDefaultRepair reproduces the live-DB regression
// where Dolt stripped DEFAULT (uuid()) from events.id: RecordEventInTable
// (reached via SetMetadata on a non-ephemeral bead) then fails because the
// upstream INSERT omits the id column. It proves repairIDDefault restores the
// default so the write succeeds — the same self-heal gc applies at store open.
func TestNativeDoltStoreEventsIDDefaultRepair(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	accessor, ok := storage.(rawDBGetter)
	if !ok {
		t.Skip("storage does not expose a raw DB")
	}
	db := accessor.DB()
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "events-default-repair", "gc")

	// Create while the default is intact (Create itself records an event).
	bead, err := store.Create(Bead{Title: "events id default repair bead"})
	if err != nil {
		t.Fatalf("Create bead: %v", err)
	}

	// Reproduce the regression: strip the DEFAULT from events.id.
	if _, err := db.Exec("ALTER TABLE `events` MODIFY COLUMN `id` char(36) NOT NULL"); err != nil {
		t.Fatalf("strip events.id default: %v", err)
	}
	if err := store.SetMetadata(bead.ID, "gc.routed_to", "gascity/builder"); err == nil {
		t.Fatalf("SetMetadata succeeded with events.id default stripped, want failure")
	}

	// Repair restores the default; the same write then succeeds.
	if err := repairIDDefault(db, "events"); err != nil {
		t.Fatalf("repairIDDefault(events): %v", err)
	}
	if err := store.SetMetadata(bead.ID, "gc.routed_to", "gascity/builder"); err != nil {
		t.Fatalf("SetMetadata after repair: %v", err)
	}
	got, err := store.Get(bead.ID)
	if err != nil {
		t.Fatalf("Get after repair: %v", err)
	}
	if got.Metadata["gc.routed_to"] != "gascity/builder" {
		t.Fatalf("Metadata[gc.routed_to] = %q, want %q", got.Metadata["gc.routed_to"], "gascity/builder")
	}
}

func TestNativeDoltStoreRealBackendRoundTrip(t *testing.T) {
	ctx := context.Background()
	storage, err := beadslib.OpenBestAvailable(ctx, filepath.Join(t.TempDir(), ".beads"))
	if err != nil {
		t.Skipf("upstream native beads storage unavailable: %v", err)
	}
	t.Cleanup(func() {
		if err := storage.Close(); err != nil {
			t.Fatalf("close upstream storage: %v", err)
		}
	})
	if err := storage.SetConfig(ctx, "issue_prefix", "gc"); err != nil {
		t.Fatalf("set issue prefix: %v", err)
	}
	store := newNativeDoltStoreWithStorageAndPrefix(storage, "native-integration", "gc")

	parent, err := store.Create(Bead{Title: "real native parent"})
	if err != nil {
		t.Fatalf("Create parent: %v", err)
	}
	blocker, err := store.Create(Bead{Title: "real native blocker"})
	if err != nil {
		t.Fatalf("Create blocker: %v", err)
	}
	child, err := store.Create(Bead{
		Title:    "real native child",
		ParentID: parent.ID,
		Needs:    []string{"blocks:" + blocker.ID},
	})
	if err != nil {
		t.Fatalf("Create child: %v", err)
	}
	got, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get child: %v", err)
	}
	if got.ParentID != parent.ID {
		t.Fatalf("ParentID = %q, want %q", got.ParentID, parent.ID)
	}
	assertNativeDependency(t, got.Dependencies, child.ID, blocker.ID, "blocks")
	if err := store.Close(child.ID); err != nil {
		t.Fatalf("Close child: %v", err)
	}
	closed, err := store.Get(child.ID)
	if err != nil {
		t.Fatalf("Get closed child: %v", err)
	}
	if closed.Status != "closed" {
		t.Fatalf("Status = %q, want closed", closed.Status)
	}
	if _, err := store.Get("gc-missing"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get missing error = %v, want ErrNotFound", err)
	}
}
