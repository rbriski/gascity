package graphstore

import (
	"context"
	"errors"
	"time"
)

// Journal-path sentinels (01-architecture §3.3). These are fork-owned and live
// with the engine; the beads.Store adapter that surfaces them as capabilities is
// a later slice. Callers compare with errors.Is.
var (
	// ErrWrongExpectedVersion is returned when an Append supplies an
	// expectedVersion that does not equal the stream head. This is the loud,
	// typed conflict that kills S0.4: a concurrent second writer that lost the
	// CAS never silently overwrites — it fails here (I-1).
	ErrWrongExpectedVersion = errors.New("graphstore: wrong expected version")

	// ErrStreamSealed is returned when appending to a stream that has been
	// sealed by its terminal event (I-3). Seal detection is engine/fold
	// semantics; the journal-core slice defines the sentinel but does not yet
	// enforce sealing.
	ErrStreamSealed = errors.New("graphstore: stream sealed")

	// ErrUnknownEventType is returned when an append carries an (engine, type)
	// pair that is not in the registered closed vocabulary (I-5).
	ErrUnknownEventType = errors.New("graphstore: unknown event type")

	// ErrLeaseFenced is returned when an append presents a leaseEpoch lower than
	// the stream's current writer-lease epoch — the belt on top of
	// expectedVersion (02-determinism §4.1). It is never the safety mechanism.
	ErrLeaseFenced = errors.New("graphstore: lease fenced")

	// ErrReducerVersionSkew is returned when a resume cannot be certified across
	// a reducer-version boundary (R-VERSION-GATE). Defined here; enforced by the
	// fold slice.
	ErrReducerVersionSkew = errors.New("graphstore: reducer version skew")

	// ErrLeaseHeld is returned by AcquireWriterLease when the lease is currently
	// held by a different holder and has not expired.
	ErrLeaseHeld = errors.New("graphstore: writer lease held by another holder")

	// ErrChainBroken is returned by Verify when a stored chain_hash or
	// payload_hash does not match its recomputation, i.e. journal tampering.
	ErrChainBroken = errors.New("graphstore: journal hash chain broken")

	// ErrIdemTokenReuse is returned when an Append reuses an idem token that is
	// already bound to a committed event whose canonical payload (or type,
	// substream, or ir_contract_version) differs. Idempotent replay carries
	// byte-identical R-CANON payloads, so a divergent reuse is never an honest
	// replay — silently discarding it would lose data, so it fails loudly
	// (R-IDEM). Distinct from a clean duplicate, which is acknowledged.
	ErrIdemTokenReuse = errors.New("graphstore: idem token reused with a different payload")

	// ErrCityMismatch is returned when opening an existing store with a CityID
	// that differs from the stored graph_meta.city_id. city_id is the immutable
	// chain-genesis input (D-SEC-1); a cross-city open must never proceed
	// silently against another city's ledger.
	ErrCityMismatch = errors.New("graphstore: city id mismatch")

	// ErrRebuildRaced is returned by RebuildTierA when a concurrent Append commits
	// between the from-genesis stream read and the rebuild's write transaction:
	// the folded prefix is stale, so re-applying it would project a torn view.
	// The rebuild aborts (its transaction rolls back untouched) and the caller
	// retries against the new head.
	ErrRebuildRaced = errors.New("graphstore: tier-A rebuild raced a concurrent append")

	// ErrBusy is a retryable sentinel wrapping SQLite SQLITE_BUSY / "database is
	// locked": another writer holds the single write lock. Callers may retry;
	// it does not mean the store is broken. The store is single-writer per
	// process (Store.db has one write connection), so within a process writes
	// serialize on the pool; ErrBusy surfaces cross-connection or cross-process
	// contention.
	ErrBusy = errors.New("graphstore: database busy (retryable)")
)

// JournalEvent is one event a caller asks Append to commit. Payload holds
// R-CANON bytes produced by the canon package; the store stores them verbatim
// and never re-encodes (I-11).
type JournalEvent struct {
	// Substream is the channel discriminator under the root's single seq space;
	// "" is the main stream (D-1).
	Substream string
	// Type is the event type, validated at append against the registered closed
	// vocabulary for the stream's engine (I-5).
	Type string
	// IRContractVersion pins the content-hashed IR contract (upcaster key); ""
	// only for the coarse v1/v2 events.
	IRContractVersion string
	// IdemToken is the idempotency token; "" means the event is not
	// effect-bearing and cannot be deduplicated (I-6 / R-IDEM).
	IdemToken string
	// Payload is the R-CANON-encoded event body, stored verbatim.
	Payload []byte
}

// StoredEvent is a committed journal row as read back by ReadStream.
type StoredEvent struct {
	StreamID string
	Seq      uint64
	Engine   string
	JournalEvent
	PayloadHash [32]byte
	ChainHash   [32]byte
	LeaseEpoch  uint64
}

// AppendResult reports the outcome of an Append.
type AppendResult struct {
	// FirstSeq is the seq assigned to the first freshly committed event. When an
	// append is a pure idempotent replay (every event a duplicate) FirstSeq is
	// the existing seq of the first input event.
	FirstSeq uint64
	// Duplicates maps an input event index to the existing seq for events whose
	// idem token was already present — those events are acknowledged and no row
	// is written (R-IDEM).
	Duplicates map[int]uint64
}

// WriterLease is a held writer lease over one root stream. The epoch is the
// monotonic fencing token stamped into journal.lease_epoch; safety comes from
// expectedVersion, not from the lease (02-determinism §4.1).
type WriterLease struct {
	StreamID  string
	Holder    string
	Epoch     uint64
	ExpiresAt time.Time
}

// AppendLogStore is the journal write surface. It is controller-only: no CLI or
// API endpoint may expose it generically (SEC-1/SEC-2).
type AppendLogStore interface {
	// Append commits events atomically at seq expectedVersion+1... in one
	// transaction. An expectedVersion that does not match the head yields
	// ErrWrongExpectedVersion; an unregistered (engine, type) yields
	// ErrUnknownEventType; a leaseEpoch below the current lease epoch yields
	// ErrLeaseFenced; events whose idem token already exists are reported in
	// AppendResult.Duplicates and not rewritten.
	Append(ctx context.Context, streamID, engine string, expectedVersion, leaseEpoch uint64, events []JournalEvent) (AppendResult, error)
	// ReadStream returns committed events for streamID with fromSeq <= seq <=
	// toSeq, ordered by seq. A toSeq of 0 means "up to the current head".
	ReadStream(ctx context.Context, streamID string, fromSeq, toSeq uint64) ([]StoredEvent, error)
	// Head returns the current head (MAX(seq)) for streamID; 0 means the stream
	// is absent.
	Head(ctx context.Context, streamID string) (uint64, error)
}

// WriterLeaseStore is the writer-lease liveness surface. Leases coordinate which
// controller writes; they do not provide safety.
type WriterLeaseStore interface {
	AcquireWriterLease(ctx context.Context, streamID, holder string, ttl time.Duration) (WriterLease, error)
	RenewWriterLease(ctx context.Context, lease WriterLease, ttl time.Duration) (WriterLease, error)
	ReleaseWriterLease(ctx context.Context, lease WriterLease) error
}

// Compile-time assertions that the concrete engine implements both surfaces.
var (
	_ AppendLogStore   = (*Store)(nil)
	_ WriterLeaseStore = (*Store)(nil)
)
