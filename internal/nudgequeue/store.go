package nudgequeue

import "github.com/gastownhall/gascity/internal/beads"

// NudgeStore is the persistence seam for the nudge-queue durability mirror — the
// transient shadow beads (type=chore, gc:nudge) that record each queued nudge's
// terminalization state. It is the swap point for relocating nudges off bd: the
// bd-delegating first implementation is any [beads.Store] (a faithful subset,
// proven below), and a future SQLite-backed store satisfies the same interface.
//
// The live flock-guarded queue file remains the source of truth; these beads are
// its persistent shadow, so the seam carries only the operations the ensure /
// terminalize / sweep paths perform (cmd/gc/nudge_beads.go + waits.go).
type NudgeStore interface {
	Create(b beads.Bead) (beads.Bead, error)
	Get(id string) (beads.Bead, error)
	List(query beads.ListQuery) ([]beads.Bead, error)
	SetMetadata(id, key, value string) error
	SetMetadataBatch(id string, kvs map[string]string) error
	Close(id string) error
}

// Compile-time proof that the bd-delegating first implementation of NudgeStore is
// any beads.Store — introducing the seam is a no-op type narrowing, no wrapper.
var _ NudgeStore = beads.Store(nil)
