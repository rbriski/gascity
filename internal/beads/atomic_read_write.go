package beads

import (
	"context"
	"errors"
)

// ErrAtomicReadWriteStorageClass reports an attempt to use an ignored
// ephemeral or no-history record through the history-only atomic read/write
// capability. Durable store metadata and history records must stay in the same
// physical transaction; upstream NativeDolt commits ignored tables separately.
var ErrAtomicReadWriteStorageClass = errors.New("atomic read/write requires history storage")

// AtomicReadWriteTx is the narrow transaction surface used to commit a durable
// record and its history-tracked store metadata together. GetIssue is an exact
// ID lookup and sees writes made earlier in the same callback. GetMetadata and
// SetMetadata access the durable history-tracked metadata table; local/ignored
// metadata is deliberately not exposed.
type AtomicReadWriteTx interface {
	GetIssue(id string) (Bead, error)
	Create(b Bead) (Bead, error)
	Update(id string, opts UpdateOpts) error
	GetMetadata(key string) (string, error)
	SetMetadata(key, value string) error
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
