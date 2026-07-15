package beads

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

const (
	// MaxAtomicReadWriteListLimit is the largest history-row set one atomic
	// callback may materialize. Callers that need a complete view reserve one
	// row for overflow detection and fail rather than consume a partial result.
	MaxAtomicReadWriteListLimit = 4096
)

var (
	// ErrAtomicReadWriteStorageClass reports an attempt to use an ignored
	// ephemeral or no-history record through the history-only atomic read/write
	// capability. Durable store metadata and history records must stay in the
	// same physical transaction; upstream NativeDolt commits ignored tables
	// separately.
	ErrAtomicReadWriteStorageClass = errors.New("atomic read/write requires history storage")
	// ErrAtomicReadWriteQuery reports an unsafe history query or a backing-store
	// result that violated its bounded, filtered, history-only contract.
	ErrAtomicReadWriteQuery = errors.New("invalid atomic read/write history query")
)

// AtomicReadWriteList is one bounded structured history-row query. A query is
// selective only when it supplies exact IDs, an ID prefix, or an issue type
// together with at least one metadata equality. Limit is always required;
// there is no zero-as-unbounded mode.
type AtomicReadWriteList struct {
	IssueType string
	Metadata  map[string]string
	IDs       []string
	IDPrefix  string
	Limit     int
}

// AtomicReadWriteTx is the narrow transaction surface used to commit a durable
// record and its history-tracked store metadata together. GetIssue is an exact
// ID lookup and sees writes made earlier in the same callback. GetMetadata and
// SetMetadata access the durable history-tracked metadata table; local/ignored
// metadata is deliberately not exposed. ListHistory is transaction-consistent,
// strictly bounded, and validates that every returned row is history-backed
// and matches the requested selectors.
type AtomicReadWriteTx interface {
	GetIssue(id string) (Bead, error)
	ListHistory(query AtomicReadWriteList) ([]Bead, error)
	Create(b Bead) (Bead, error)
	Update(id string, opts UpdateOpts) error
	GetMetadata(key string) (string, error)
	SetMetadata(key, value string) error
}

func validateAtomicReadWriteList(query AtomicReadWriteList) error {
	if query.Limit <= 0 || query.Limit > MaxAtomicReadWriteListLimit {
		return fmt.Errorf("history query limit %d is outside 1..%d: %w", query.Limit, MaxAtomicReadWriteListLimit, ErrAtomicReadWriteQuery)
	}
	query.IssueType = strings.TrimSpace(query.IssueType)
	query.IDPrefix = strings.TrimSpace(query.IDPrefix)
	for _, id := range query.IDs {
		if strings.TrimSpace(id) == "" {
			return fmt.Errorf("history query contains an empty id: %w", ErrAtomicReadWriteQuery)
		}
	}
	if len(query.IDs) > query.Limit {
		return fmt.Errorf("history query has %d exact ids but limit %d: %w", len(query.IDs), query.Limit, ErrAtomicReadWriteQuery)
	}
	for key, value := range query.Metadata {
		if strings.TrimSpace(key) == "" || strings.TrimSpace(value) == "" {
			return fmt.Errorf("history query metadata selectors must have non-empty keys and values: %w", ErrAtomicReadWriteQuery)
		}
	}
	if len(query.IDs) > 0 || query.IDPrefix != "" {
		return nil
	}
	if query.IssueType != "" && len(query.Metadata) > 0 {
		return nil
	}
	return fmt.Errorf("history query would scan without exact ids, id prefix, or type plus metadata: %w", ErrAtomicReadWriteQuery)
}

func validateAtomicReadWriteListResult(query AtomicReadWriteList, rows []Bead) error {
	if len(rows) > query.Limit {
		return fmt.Errorf("history query returned %d rows above limit %d: %w", len(rows), query.Limit, ErrAtomicReadWriteQuery)
	}
	ids := make(map[string]struct{}, len(query.IDs))
	for _, id := range query.IDs {
		ids[id] = struct{}{}
	}
	for _, row := range rows {
		if err := requireAtomicReadWriteHistory(row); err != nil {
			return fmt.Errorf("history query returned a non-history row: %w", errors.Join(ErrAtomicReadWriteQuery, err))
		}
		if len(ids) > 0 {
			if _, ok := ids[row.ID]; !ok {
				return fmt.Errorf("history query returned unrequested id %q: %w", row.ID, ErrAtomicReadWriteQuery)
			}
		}
		if query.IDPrefix != "" && !strings.HasPrefix(row.ID, query.IDPrefix) {
			return fmt.Errorf("history query returned id %q outside prefix %q: %w", row.ID, query.IDPrefix, ErrAtomicReadWriteQuery)
		}
		if query.IssueType != "" && row.Type != query.IssueType {
			return fmt.Errorf("history query returned id %q with type %q, want %q: %w", row.ID, row.Type, query.IssueType, ErrAtomicReadWriteQuery)
		}
		for key, value := range query.Metadata {
			if row.Metadata[key] != value {
				return fmt.Errorf("history query returned id %q without metadata selector %q: %w", row.ID, key, ErrAtomicReadWriteQuery)
			}
		}
	}
	return nil
}

// AtomicReadWriteStore is an optional Store capability for reading and writing
// history records and durable store metadata in one backing transaction. A
// callback error rolls the complete transaction back. Context cancellation is
// checked before callback entry and bounds the backing operation. Stores
// without that guarantee do not implement this interface.
type AtomicReadWriteStore interface {
	AtomicReadWrite(ctx context.Context, commitMsg string, fn func(AtomicReadWriteTx) error) error
}

// AtomicReadWriteHandleProvider exposes an atomic read/write handle for stores
// whose capability depends on their backing store at runtime.
type AtomicReadWriteHandleProvider interface {
	AtomicReadWriteHandle() (AtomicReadWriteStore, bool)
}

// AtomicReadWriteFor returns store's atomic read/write capability when it is
// available. Unsupported stores return nil, false; the helper never promotes a
// sequential or partially atomic Store.Tx implementation into this capability.
func AtomicReadWriteFor(store Store) (AtomicReadWriteStore, bool) {
	if store == nil {
		return nil, false
	}
	if atomicStore, ok := store.(AtomicReadWriteStore); ok {
		return atomicStore, true
	}
	if provider, ok := store.(AtomicReadWriteHandleProvider); ok {
		return provider.AtomicReadWriteHandle()
	}
	return nil, false
}
