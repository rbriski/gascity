package beads

import (
	"context"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// The interfaces below surface the graph substrate's journal primitives —
// append-only event log, expected-version CAS, and writer lease — as optional
// beads.Store capabilities, following the same probe idiom as GraphApplyStore.
// They exist for the P4 claim-as-append path; this slice wires only the
// interface, the JournalStore forwarding impl, the CachingStore forwarding, and
// the capability probes. Nothing here is wired into the dispatcher yet.
//
// SEC-1/SEC-2: the append surface is controller-only. Exposing it as an
// optional capability (probed, never part of the base Store interface) keeps it
// off any generic CLI/API projection — a caller must reach for it explicitly.

// AppendLogStore is the optional beads.Store capability exposing the journal's
// append-only event log. AppendEvent is the expected-version compare-and-swap
// writer: it commits events atomically at seq expectedVersion+1... in one
// transaction and returns graphstore.ErrWrongExpectedVersion when
// expectedVersion does not match the stream head. The version to condition an
// append on is read via ConditionalVersionStore.StreamHead.
//
// Note the deliberate method rename across the package boundary: this
// beads-layer method is AppendEvent, forwarding to graphstore.Store.Append
// (the like-named graphstore.AppendLogStore.Append). The distinct name keeps
// the beads capability unambiguous when both packages are imported together.
type AppendLogStore interface {
	// AppendEvent commits events to streamID under engine's registered
	// vocabulary, conditioned on expectedVersion (the CAS) and fenced by
	// leaseEpoch. See graphstore.Store.Append for the full contract.
	AppendEvent(ctx context.Context, streamID, engine string, expectedVersion, leaseEpoch uint64, events []graphstore.JournalEvent) (graphstore.AppendResult, error)
	// ReadStream returns committed events for streamID with fromSeq <= seq <=
	// toSeq, ordered by seq. A toSeq of 0 means "up to the current head".
	ReadStream(ctx context.Context, streamID string, fromSeq, toSeq uint64) ([]graphstore.StoredEvent, error)
}

// ConditionalVersionStore is the read half of the expected-version CAS: it
// returns the current head so a caller can learn the version an AppendEvent
// must be conditioned on. The compare-and-swap itself happens inside
// AppendLogStore.AppendEvent (which returns graphstore.ErrWrongExpectedVersion
// on a lost race); this surface only reads the version.
type ConditionalVersionStore interface {
	// StreamHead returns the current head (MAX(seq)) for streamID; 0 means the
	// stream is absent.
	StreamHead(ctx context.Context, streamID string) (uint64, error)
}

// WriterLeaseStore is the optional beads.Store capability exposing the journal's
// writer-lease liveness surface. Leases coordinate which controller writes; they
// do not provide safety (that is expectedVersion). Mirrors
// graphstore.WriterLeaseStore.
type WriterLeaseStore interface {
	AcquireWriterLease(ctx context.Context, streamID, holder string, ttl time.Duration) (graphstore.WriterLease, error)
	RenewWriterLease(ctx context.Context, lease graphstore.WriterLease, ttl time.Duration) (graphstore.WriterLease, error)
	ReleaseWriterLease(ctx context.Context, lease graphstore.WriterLease) error
}

// Handle-provider interfaces let a wrapper (CachingStore) expose a delegated
// journal capability without claiming the interface globally, mirroring
// GraphApplyHandleProvider.

// AppendLogHandleProvider exposes an append-log handle for stores whose
// capability depends on wrapped runtime state.
type AppendLogHandleProvider interface {
	AppendLogHandle() (AppendLogStore, bool)
}

// ConditionalVersionHandleProvider exposes a conditional-version handle for
// wrapper stores.
type ConditionalVersionHandleProvider interface {
	ConditionalVersionHandle() (ConditionalVersionStore, bool)
}

// WriterLeaseHandleProvider exposes a writer-lease handle for wrapper stores.
type WriterLeaseHandleProvider interface {
	WriterLeaseHandle() (WriterLeaseStore, bool)
}

// AppendLogStoreFor returns the append-log capability for store when available.
// It preserves ordinary AppendLogStore implementations and lets wrappers expose
// a delegated handle. A store that does not support the capability returns
// (nil, false) — the honest "absent" signal, never a silently degraded stub.
func AppendLogStoreFor(store Store) (AppendLogStore, bool) {
	if store == nil {
		return nil, false
	}
	if s, ok := store.(AppendLogStore); ok {
		return s, true
	}
	if p, ok := store.(AppendLogHandleProvider); ok {
		return p.AppendLogHandle()
	}
	return nil, false
}

// ConditionalVersionStoreFor returns the expected-version CAS read capability
// for store when available.
func ConditionalVersionStoreFor(store Store) (ConditionalVersionStore, bool) {
	if store == nil {
		return nil, false
	}
	if s, ok := store.(ConditionalVersionStore); ok {
		return s, true
	}
	if p, ok := store.(ConditionalVersionHandleProvider); ok {
		return p.ConditionalVersionHandle()
	}
	return nil, false
}

// WriterLeaseStoreFor returns the writer-lease capability for store when
// available.
func WriterLeaseStoreFor(store Store) (WriterLeaseStore, bool) {
	if store == nil {
		return nil, false
	}
	if s, ok := store.(WriterLeaseStore); ok {
		return s, true
	}
	if p, ok := store.(WriterLeaseHandleProvider); ok {
		return p.WriterLeaseHandle()
	}
	return nil, false
}

// --- JournalStore forwarding impls -----------------------------------------

// Compile-time assertions that JournalStore surfaces all three journal
// capabilities directly (it owns a *graphstore.Store).
var (
	_ AppendLogStore          = (*JournalStore)(nil)
	_ ConditionalVersionStore = (*JournalStore)(nil)
	_ WriterLeaseStore        = (*JournalStore)(nil)
)

// AppendEvent forwards to the underlying journal engine's Append (the
// expected-version CAS writer).
func (s *JournalStore) AppendEvent(ctx context.Context, streamID, engine string, expectedVersion, leaseEpoch uint64, events []graphstore.JournalEvent) (graphstore.AppendResult, error) {
	return s.gs.Append(ctx, streamID, engine, expectedVersion, leaseEpoch, events)
}

// ReadStream forwards to the underlying journal engine's ReadStream.
func (s *JournalStore) ReadStream(ctx context.Context, streamID string, fromSeq, toSeq uint64) ([]graphstore.StoredEvent, error) {
	return s.gs.ReadStream(ctx, streamID, fromSeq, toSeq)
}

// StreamHead forwards to the underlying journal engine's Head — the version an
// AppendEvent CAS must be conditioned on.
func (s *JournalStore) StreamHead(ctx context.Context, streamID string) (uint64, error) {
	return s.gs.Head(ctx, streamID)
}

// AcquireWriterLease forwards to the underlying journal engine.
func (s *JournalStore) AcquireWriterLease(ctx context.Context, streamID, holder string, ttl time.Duration) (graphstore.WriterLease, error) {
	return s.gs.AcquireWriterLease(ctx, streamID, holder, ttl)
}

// RenewWriterLease forwards to the underlying journal engine.
func (s *JournalStore) RenewWriterLease(ctx context.Context, lease graphstore.WriterLease, ttl time.Duration) (graphstore.WriterLease, error) {
	return s.gs.RenewWriterLease(ctx, lease, ttl)
}

// ReleaseWriterLease forwards to the underlying journal engine.
func (s *JournalStore) ReleaseWriterLease(ctx context.Context, lease graphstore.WriterLease) error {
	return s.gs.ReleaseWriterLease(ctx, lease)
}
