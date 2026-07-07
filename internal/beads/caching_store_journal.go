package beads

// CachingStore forwards the journal capabilities (append-only log,
// expected-version CAS read, writer lease) straight through to the backing
// store. These surfaces operate on the journal event streams — a data domain
// disjoint from the bead-row projection this cache holds — so there is nothing
// for the cache to mask: an append never mutates a cached façade bead row, and
// no cached read can shadow a stream read. Forwarding is therefore total, not
// partial.
//
// The one thing that would be a silent drop is returning a degraded handle that
// swallows appends or serves stale stream reads. We never do: when the backing
// store lacks the capability the handle returns (nil, false) — the honest
// "absent" signal — and when it has it, the handle is the backing capability
// itself, unmediated. If a future slice folds journal appends into bead
// projections, the coupling must be handled here explicitly (invalidate the
// affected cached rows), never by silently serving stale reads.

// AppendLogHandle returns the backing store's append-log capability when it has
// one. See AppendLogStoreFor.
func (c *CachingStore) AppendLogHandle() (AppendLogStore, bool) {
	return AppendLogStoreFor(c.backing)
}

// ConditionalVersionHandle returns the backing store's expected-version CAS read
// capability when it has one.
func (c *CachingStore) ConditionalVersionHandle() (ConditionalVersionStore, bool) {
	return ConditionalVersionStoreFor(c.backing)
}

// WriterLeaseHandle returns the backing store's writer-lease capability when it
// has one.
func (c *CachingStore) WriterLeaseHandle() (WriterLeaseStore, bool) {
	return WriterLeaseStoreFor(c.backing)
}
