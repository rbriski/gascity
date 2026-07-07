package beads

// EdgeKey identifies a single dependency edge by its (from, to, type) triple.
// It is the lookup key for edge metadata a plain DepList (which returns only
// from/to/type) does not carry, used by the strand migration to preserve
// waits-for gate metadata (`{"gate":"any-children"}`) into the journal copy.
type EdgeKey struct {
	FromID  string
	ToID    string
	DepType string
}

// EdgeMetadataReader optionally exposes the raw JSON metadata stored on a
// dependency edge. DepList intentionally returns only the (issue, depends-on,
// type) triple; the edge's type-specific metadata blob — e.g. a waits-for gate's
// `{"gate":"any-children"}` minted by the formula compiler — is dropped there. A
// source store that implements this surface lets the strand migration carry that
// metadata into the journal copy so post-migration gate evaluation still works.
// A store WITHOUT the capability migrates edges with empty metadata (the
// pre-P3.2 behavior): a documented, loud-in-comment degradation, never a silent
// wrong answer, because the fold-verify/re-verify hashes see the same empty
// metadata on both sides.
type EdgeMetadataReader interface {
	// EdgeMetadata returns the metadata blob on the edge fromID -> toID of the
	// given dependency type, or "" when the edge exists with no metadata or is
	// absent. A hard read failure is returned as an error, never flattened to "".
	EdgeMetadata(fromID, toID, depType string) (string, error)
}

// EdgeMetadataHandleProvider lets a wrapper (policy/caching) expose the backing
// store's edge-metadata capability without claiming the interface globally,
// mirroring ResidenceMigrationHandleProvider.
type EdgeMetadataHandleProvider interface {
	EdgeMetadataHandle() (EdgeMetadataReader, bool)
}

// EdgeMetadataReaderFor returns the edge-metadata capability for store when
// available, reaching through wrapper handle providers. A store without it
// returns (nil, false) — the honest "absent" signal that makes the caller fall
// back to empty edge metadata rather than guess.
func EdgeMetadataReaderFor(store Store) (EdgeMetadataReader, bool) {
	if store == nil {
		return nil, false
	}
	if r, ok := store.(EdgeMetadataReader); ok {
		return r, true
	}
	if p, ok := store.(EdgeMetadataHandleProvider); ok {
		return p.EdgeMetadataHandle()
	}
	return nil, false
}
