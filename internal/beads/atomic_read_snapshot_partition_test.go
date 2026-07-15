package beads

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

const atomicSnapshotPartitionRouteForTest = "gc:control-partition:v1:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestAtomicReadSnapshotPageQueryRequiresExactAssigneeSelectorWhenPresent(t *testing.T) {
	query := AtomicReadSnapshotPageQuery{
		IDPrefix: "gc-nudge-",
		Status:   "open",
		Order:    AtomicReadSnapshotOrderID,
		Limit:    1,
	}
	setAtomicSnapshotAssigneeForTest(t, &query, atomicSnapshotPartitionRouteForTest)
	if err := validateAtomicReadSnapshotPageQuery(query); err != nil {
		t.Fatalf("validate exact-assignee snapshot query: %v", err)
	}

	row := Bead{
		ID:        "gc-nudge-owned",
		Title:     "durable control command",
		Status:    "open",
		Type:      "task",
		Assignee:  atomicSnapshotPartitionRouteForTest,
		CreatedAt: time.Date(2026, 7, 15, 12, 0, 0, 0, time.UTC),
		UpdatedAt: time.Date(2026, 7, 15, 12, 0, 1, 0, time.UTC),
	}
	page := AtomicReadSnapshotPage{
		Rows: []Bead{row},
		Next: AtomicReadSnapshotCursor{ID: row.ID},
	}
	if err := validateAtomicReadSnapshotPage(query, page); err != nil {
		t.Fatalf("validate owned exact-assignee page: %v", err)
	}

	page.Rows[0].Assignee = "gc:control-partition:v1:ffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffffff"
	if err := validateAtomicReadSnapshotPage(query, page); !errors.Is(err, ErrAtomicReadSnapshotQuery) {
		t.Fatalf("foreign-assignee page error = %v, want ErrAtomicReadSnapshotQuery", err)
	}

	setAtomicSnapshotAssigneeForTest(t, &query, " partition-with-whitespace ")
	if err := validateAtomicReadSnapshotPageQuery(query); !errors.Is(err, ErrAtomicReadSnapshotQuery) {
		t.Fatalf("non-canonical assignee selector error = %v, want ErrAtomicReadSnapshotQuery", err)
	}

	setAtomicSnapshotAssigneeForTest(t, &query, atomicSnapshotPartitionRouteForTest)
	query.Order = AtomicReadSnapshotOrderUpdatedAtID
	if err := validateAtomicReadSnapshotPageQuery(query); !errors.Is(err, ErrAtomicReadSnapshotQuery) {
		t.Fatalf("exact-assignee updated-at query error = %v, want ErrAtomicReadSnapshotQuery", err)
	}
}

func setAtomicSnapshotAssigneeForTest(t *testing.T, query *AtomicReadSnapshotPageQuery, assignee string) {
	t.Helper()
	field := reflect.ValueOf(query).Elem().FieldByName("Assignee")
	if !field.IsValid() {
		t.Fatal("AtomicReadSnapshotPageQuery has no exact Assignee selector; partition reads cannot be pushed into an indexed query")
	}
	if field.Kind() != reflect.String || !field.CanSet() {
		t.Fatalf("AtomicReadSnapshotPageQuery.Assignee has type/settable = %s/%t, want settable string", field.Kind(), field.CanSet())
	}
	field.SetString(assignee)
}
