package beads

// RowOp is the physical mutation a store performed on a bead row.
type RowOp string

const (
	// RowCreated is emitted after a bead is created.
	RowCreated RowOp = "created"
	// RowUpdated is emitted after a bead's fields change (this includes status
	// transitions like close/reopen and metadata writes, which are physically
	// updates — a higher layer refines them into semantic events if needed).
	RowUpdated RowOp = "updated"
	// RowDeleted is emitted after a bead is removed.
	RowDeleted RowOp = "deleted"
)

// RowChange is the low-level, domain-agnostic notification a store emits after a
// committed mutation. It carries only primitive row data — no domain payloads —
// so it is safe to emit from the Layer-0 substrate; a higher layer (Layer 2/3)
// translates it into typed domain/bead events. Type is the bead's issue_type,
// included so a translator can route the change without re-reading the store
// (essential for deletes, where the row is already gone).
type RowChange struct {
	ID   string
	Type string
	Op   RowOp
}

// RowChangeEmitter receives a RowChange after each committed mutation. It is
// invoked synchronously, after the write transaction commits and the single
// write connection's lock is released, so the emitter may safely read the store
// and observes the committed state. A nil emitter is a no-op (the default), which
// keeps a store without a recorder byte-identical to before.
type RowChangeEmitter func(RowChange)
