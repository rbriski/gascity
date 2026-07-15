package beads

import (
	"errors"
	"testing"
	"time"
)

func TestAtomicReadSnapshotPageQueryRequiresBoundedIndexedKeyset(t *testing.T) {
	t.Parallel()

	valid := AtomicReadSnapshotPageQuery{
		IDPrefix: "gc-nudge-",
		Status:   "open",
		Limit:    128,
	}
	if err := validateAtomicReadSnapshotPageQuery(valid); err != nil {
		t.Fatalf("valid query: %v", err)
	}
	valid.After = AtomicReadSnapshotCursor{
		UpdatedAt: time.Date(2026, 7, 15, 12, 0, 0, 123000, time.UTC),
		ID:        "gc-nudge-0123",
	}
	if err := validateAtomicReadSnapshotPageQuery(valid); err != nil {
		t.Fatalf("valid continuation query: %v", err)
	}

	tests := map[string]func(*AtomicReadSnapshotPageQuery){
		"missing prefix":  func(query *AtomicReadSnapshotPageQuery) { query.IDPrefix = "" },
		"wildcard prefix": func(query *AtomicReadSnapshotPageQuery) { query.IDPrefix = "gc-nudge-%" },
		"missing status":  func(query *AtomicReadSnapshotPageQuery) { query.Status = "" },
		"zero limit":      func(query *AtomicReadSnapshotPageQuery) { query.Limit = 0 },
		"oversized limit": func(query *AtomicReadSnapshotPageQuery) { query.Limit = MaxAtomicReadSnapshotPageSize + 1 },
		"cursor id without time": func(query *AtomicReadSnapshotPageQuery) {
			query.After = AtomicReadSnapshotCursor{ID: "gc-nudge-0123"}
		},
		"cursor time without id": func(query *AtomicReadSnapshotPageQuery) {
			query.After = AtomicReadSnapshotCursor{UpdatedAt: time.Now().UTC()}
		},
		"non UTC cursor": func(query *AtomicReadSnapshotPageQuery) {
			query.After = AtomicReadSnapshotCursor{UpdatedAt: time.Now().In(time.FixedZone("offset", 3600)), ID: "gc-nudge-0123"}
		},
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			query := valid
			mutate(&query)
			if err := validateAtomicReadSnapshotPageQuery(query); !errors.Is(err, ErrAtomicReadSnapshotQuery) {
				t.Fatalf("error = %v, want ErrAtomicReadSnapshotQuery", err)
			}
		})
	}
}

func TestValidateAtomicReadSnapshotPageRejectsBackendCursorAndSelectorViolations(t *testing.T) {
	t.Parallel()

	baseTime := time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC)
	query := AtomicReadSnapshotPageQuery{IDPrefix: "gc-nudge-", Status: "open", Limit: 2}
	valid := AtomicReadSnapshotPage{
		Rows: []Bead{
			{ID: "gc-nudge-a", Status: "open", UpdatedAt: baseTime},
			{ID: "gc-nudge-b", Status: "open", UpdatedAt: baseTime.Add(time.Second)},
		},
		Next: AtomicReadSnapshotCursor{UpdatedAt: baseTime.Add(time.Second), ID: "gc-nudge-b"},
	}
	if err := validateAtomicReadSnapshotPage(query, valid); err != nil {
		t.Fatalf("valid page: %v", err)
	}

	tests := map[string]func(*AtomicReadSnapshotPage){
		"too many rows": func(page *AtomicReadSnapshotPage) {
			page.Rows = append(page.Rows, Bead{ID: "gc-nudge-c", Status: "open", UpdatedAt: baseTime.Add(2 * time.Second)})
		},
		"wrong status":    func(page *AtomicReadSnapshotPage) { page.Rows[0].Status = "closed" },
		"wrong prefix":    func(page *AtomicReadSnapshotPage) { page.Rows[0].ID = "other-a" },
		"zero updated at": func(page *AtomicReadSnapshotPage) { page.Rows[0].UpdatedAt = time.Time{} },
		"out of order":    func(page *AtomicReadSnapshotPage) { page.Rows[1].UpdatedAt = baseTime.Add(-time.Second) },
		"wrong next":      func(page *AtomicReadSnapshotPage) { page.Next.ID = "gc-nudge-a" },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			page := valid
			page.Rows = append([]Bead(nil), valid.Rows...)
			mutate(&page)
			if err := validateAtomicReadSnapshotPage(query, page); !errors.Is(err, ErrAtomicReadSnapshotQuery) {
				t.Fatalf("error = %v, want ErrAtomicReadSnapshotQuery", err)
			}
		})
	}
}

func TestAtomicReadSnapshotForDoesNotFabricateCapability(t *testing.T) {
	t.Parallel()

	for _, store := range []Store{NewMemStore(), &FileStore{}, NewBdStore(t.TempDir(), nil)} {
		if capability, ok := AtomicReadSnapshotFor(store); ok || capability != nil {
			t.Fatalf("AtomicReadSnapshotFor(%T) = (%T, %v), want typed absence", store, capability, ok)
		}
	}
}

func TestNativeDoltAtomicReadSnapshotFailsClosedWithoutRawDatabase(t *testing.T) {
	t.Parallel()

	store := newNativeDoltStoreForTest(newAtomicNativeDoltStorageForTest())
	capability, ok := AtomicReadSnapshotFor(store)
	if !ok {
		t.Fatal("AtomicReadSnapshotFor(NativeDoltStore) = false, want production capability")
	}
	called := false
	err := capability.AtomicReadSnapshot(t.Context(), func(AtomicReadSnapshotTx) error {
		called = true
		return nil
	})
	if !errors.Is(err, ErrAtomicReadSnapshotUnsupported) {
		t.Fatalf("AtomicReadSnapshot error = %v, want ErrAtomicReadSnapshotUnsupported", err)
	}
	if called {
		t.Fatal("AtomicReadSnapshot called callback without a raw transaction-consistent database")
	}
}
