package beads

var _ AtomicReadSnapshotHandleProvider = (*CachingStore)(nil)

// AtomicReadSnapshotHandle returns the backing store's stable read snapshot
// capability without routing any read through the cache. Unsupported backings
// remain a typed absence.
func (c *CachingStore) AtomicReadSnapshotHandle() (AtomicReadSnapshotStore, bool) {
	if c == nil {
		return nil, false
	}
	return AtomicReadSnapshotFor(c.backing)
}
