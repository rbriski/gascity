package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
)

const (
	// MaxAtomicReadSnapshotPageSize is the hard upper bound for one page in a
	// transaction-consistent history traversal. Traversals have no total-row
	// ceiling and advance only with the returned keyset cursor.
	MaxAtomicReadSnapshotPageSize = 256
)

var (
	// ErrAtomicReadSnapshotUnsupported reports that a store cannot hold one
	// read-only, transaction-consistent snapshot across bounded keyset pages.
	ErrAtomicReadSnapshotUnsupported = errors.New("atomic read snapshot is unsupported")
	// ErrAtomicReadSnapshotQuery reports an unindexed/unbounded query or a page
	// whose selectors, order, or continuation were violated by the backend.
	ErrAtomicReadSnapshotQuery = errors.New("invalid atomic read snapshot query")
)

// AtomicReadSnapshotCursor is the exclusive `(updated_at,id)` keyset for the
// next page. UpdatedAt is always UTC and ID is the deterministic tie-breaker.
// Both fields are zero for the first page and after the final page.
type AtomicReadSnapshotCursor struct {
	UpdatedAt time.Time
	ID        string
}

// AtomicReadSnapshotOrder selects one verified backing index and its matching
// exclusive keyset. Callers must choose explicitly so a backend cannot silently
// substitute an unbounded sort or scan.
type AtomicReadSnapshotOrder uint8

const (
	// AtomicReadSnapshotOrderID traverses `(status,id)`. It is the bounded path
	// for current-state reads and partition-prefix selection.
	AtomicReadSnapshotOrderID AtomicReadSnapshotOrder = iota + 1
	// AtomicReadSnapshotOrderUpdatedAtID traverses `(status,updated_at,id)`.
	// It is the monotonic path for discovering records that enter a status.
	AtomicReadSnapshotOrderUpdatedAtID
)

// AtomicReadSnapshotPageQuery selects one bounded page through standard,
// indexed issue columns. IDPrefix and Status are both mandatory. Metadata is
// deliberately absent because JSON metadata equality is not an indexed scale
// contract in the supported NativeDolt schema.
type AtomicReadSnapshotPageQuery struct {
	IDPrefix string
	Status   string
	Order    AtomicReadSnapshotOrder
	After    AtomicReadSnapshotCursor
	Limit    int
}

// AtomicReadSnapshotPage is one owned page and its exclusive continuation.
// Next is zero when fewer than Limit rows remain.
type AtomicReadSnapshotPage struct {
	Rows []Bead
	Next AtomicReadSnapshotCursor
}

// AtomicReadSnapshotTx is a read-only transaction surface. GetIssue and
// GetMetadata observe the same snapshot as every ListHistoryPage call.
type AtomicReadSnapshotTx interface {
	GetIssue(id string) (Bead, error)
	ListHistoryPage(query AtomicReadSnapshotPageQuery) (AtomicReadSnapshotPage, error)
	GetMetadata(key string) (string, error)
}

// AtomicReadSnapshotStore runs a complete bounded-page traversal inside one
// stable backing snapshot. A callback cannot mutate records or metadata.
type AtomicReadSnapshotStore interface {
	AtomicReadSnapshot(ctx context.Context, fn func(AtomicReadSnapshotTx) error) error
}

// AtomicReadSnapshotPreparer explicitly installs any provider-owned companion
// index required by AtomicReadSnapshot. Preparation is a writer operation;
// read paths never invoke it implicitly.
type AtomicReadSnapshotPreparer interface {
	PrepareAtomicReadSnapshot(ctx context.Context) error
}

// AtomicReadSnapshotHandleProvider exposes the capability for wrappers whose
// support depends on their backing store.
type AtomicReadSnapshotHandleProvider interface {
	AtomicReadSnapshotHandle() (AtomicReadSnapshotStore, bool)
}

// AtomicReadSnapshotFor returns store's real snapshot capability. It never
// promotes a sequence of independent reads into a fabricated snapshot.
func AtomicReadSnapshotFor(store Store) (AtomicReadSnapshotStore, bool) {
	if store == nil {
		return nil, false
	}
	if snapshotStore, ok := store.(AtomicReadSnapshotStore); ok {
		return snapshotStore, true
	}
	if provider, ok := store.(AtomicReadSnapshotHandleProvider); ok {
		return provider.AtomicReadSnapshotHandle()
	}
	return nil, false
}

func validateAtomicReadSnapshotPageQuery(query AtomicReadSnapshotPageQuery) error {
	if query.Limit <= 0 || query.Limit > MaxAtomicReadSnapshotPageSize {
		return fmt.Errorf("snapshot page limit %d is outside 1..%d: %w", query.Limit, MaxAtomicReadSnapshotPageSize, ErrAtomicReadSnapshotQuery)
	}
	if err := validateAtomicReadSnapshotSelector("id prefix", query.IDPrefix); err != nil {
		return err
	}
	if strings.ContainsAny(query.IDPrefix, "%_\\") {
		return fmt.Errorf("snapshot id prefix contains a SQL pattern character: %w", ErrAtomicReadSnapshotQuery)
	}
	if err := validateAtomicReadSnapshotSelector("status", query.Status); err != nil {
		return err
	}
	if query.Order != AtomicReadSnapshotOrderID && query.Order != AtomicReadSnapshotOrderUpdatedAtID {
		return fmt.Errorf("snapshot order %d is unsupported: %w", query.Order, ErrAtomicReadSnapshotQuery)
	}
	if query.After == (AtomicReadSnapshotCursor{}) {
		return nil
	}
	if !strings.HasPrefix(query.After.ID, query.IDPrefix) {
		return fmt.Errorf("snapshot continuation id %q is outside prefix %q: %w", query.After.ID, query.IDPrefix, ErrAtomicReadSnapshotQuery)
	}
	switch query.Order {
	case AtomicReadSnapshotOrderID:
		if query.After.ID == "" || !query.After.UpdatedAt.IsZero() {
			return fmt.Errorf("status/id snapshot continuation requires only id: %w", ErrAtomicReadSnapshotQuery)
		}
	case AtomicReadSnapshotOrderUpdatedAtID:
		if query.After.UpdatedAt.IsZero() || query.After.ID == "" {
			return fmt.Errorf("status/updated_at/id snapshot continuation requires updated_at and id: %w", ErrAtomicReadSnapshotQuery)
		}
		if query.After.UpdatedAt.Location() != time.UTC {
			return fmt.Errorf("snapshot continuation updated_at is not UTC: %w", ErrAtomicReadSnapshotQuery)
		}
	}
	return nil
}

func validateAtomicReadSnapshotSelector(name, value string) error {
	if value == "" || strings.TrimSpace(value) != value {
		return fmt.Errorf("snapshot %s is empty or non-canonical: %w", name, ErrAtomicReadSnapshotQuery)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return fmt.Errorf("snapshot %s contains a control character: %w", name, ErrAtomicReadSnapshotQuery)
		}
	}
	return nil
}

func validateAtomicReadSnapshotPage(query AtomicReadSnapshotPageQuery, page AtomicReadSnapshotPage) error {
	if err := validateAtomicReadSnapshotPageQuery(query); err != nil {
		return err
	}
	if len(page.Rows) > query.Limit {
		return fmt.Errorf("snapshot page returned %d rows above limit %d: %w", len(page.Rows), query.Limit, ErrAtomicReadSnapshotQuery)
	}
	prior := query.After
	for _, row := range page.Rows {
		if err := requireAtomicReadWriteHistory(row); err != nil {
			return fmt.Errorf("snapshot page returned a non-history row: %w", errors.Join(ErrAtomicReadSnapshotQuery, err))
		}
		if row.Status != query.Status || !strings.HasPrefix(row.ID, query.IDPrefix) {
			return fmt.Errorf("snapshot page row %q violates status/prefix selectors: %w", row.ID, ErrAtomicReadSnapshotQuery)
		}
		if row.UpdatedAt.IsZero() || row.UpdatedAt.Location() != time.UTC {
			return fmt.Errorf("snapshot page row %q has zero or non-UTC updated_at: %w", row.ID, ErrAtomicReadSnapshotQuery)
		}
		cursor := atomicReadSnapshotCursorForRow(query.Order, row)
		if !atomicReadSnapshotCursorAfter(query.Order, cursor, prior) {
			return fmt.Errorf("snapshot page row %q does not strictly advance the keyset: %w", row.ID, ErrAtomicReadSnapshotQuery)
		}
		prior = cursor
	}
	wantNext := AtomicReadSnapshotCursor{}
	if len(page.Rows) == query.Limit {
		wantNext = atomicReadSnapshotCursorForRow(query.Order, page.Rows[len(page.Rows)-1])
	}
	if page.Next != wantNext {
		return fmt.Errorf("snapshot page continuation %#v does not match %#v: %w", page.Next, wantNext, ErrAtomicReadSnapshotQuery)
	}
	return nil
}

func atomicReadSnapshotCursorForRow(order AtomicReadSnapshotOrder, row Bead) AtomicReadSnapshotCursor {
	if order == AtomicReadSnapshotOrderID {
		return AtomicReadSnapshotCursor{ID: row.ID}
	}
	return AtomicReadSnapshotCursor{UpdatedAt: row.UpdatedAt, ID: row.ID}
}

func atomicReadSnapshotCursorAfter(order AtomicReadSnapshotOrder, candidate, prior AtomicReadSnapshotCursor) bool {
	if prior == (AtomicReadSnapshotCursor{}) {
		return true
	}
	if order == AtomicReadSnapshotOrderID {
		return candidate.ID > prior.ID
	}
	if candidate.UpdatedAt.After(prior.UpdatedAt) {
		return true
	}
	return candidate.UpdatedAt.Equal(prior.UpdatedAt) && candidate.ID > prior.ID
}
