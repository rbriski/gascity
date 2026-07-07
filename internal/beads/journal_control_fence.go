package beads

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// The control-epoch fence turns the control plane's epoch check-then-act (read
// gc.control_epoch, compare, SetMetadata) into a serialized decide-then-write
// for journal-resident control beads. S0.4 (ADR-0001) proved the check-then-act
// is a silent-lost-update class: two processors both read epoch E, both
// SetMetadata E+1, one write silently vanishes. The fence serializes those
// writers on a per-bead journal stream: a writer acquires the slot with a CAS
// append at the current head, and only THEN re-reads gc.control_epoch and
// applies its decision (dispatch's fenceControlWrite drives the loop). A
// concurrent writer that loses the CAS (graphstore.ErrWrongExpectedVersion)
// retries behind the winner rather than erroring, and re-evaluates its guard
// against the now-advanced epoch — so it no-ops instead of regressing the value.
// This kills the epoch lost-update AND the staggered regression at the fenced
// sites. The vocabulary below is the append half; the decide-after-acquire loop
// and its transient loser-handling live in dispatch/control_fence.go.
//
// Out of scope at P2 (deferred to P5): the append and the epoch SetMetadata are
// still two transactions (a crash between them is covered by idempotent redo,
// not atomicity), and the fence guards the epoch bump but not the Attach-time
// sub-DAG instantiation, so it does not by itself prevent a duplicate sub-DAG
// under a lost race — only a duplicate/regressed epoch. Full duplicate-DAG
// prevention needs an Attach-level fence over the instantiate region (P5).
//
// Stream identity (why synthetic at P2): a mutation-primary façade bead
// (fold_owned=0 — every control bead the dispatcher processes at P2) is written
// with an empty nodes.stream_id (journal_store.go applyCreateInTx), so there is
// no pre-existing per-bead journal stream to fence on. The fence therefore
// derives a dedicated per-bead stream from the bead id. Appends to it are pure
// journal-table writes (graphstore.Append is a UNIQUE(stream_id,seq) CAS that
// triggers no fold — the fold path is dormant at P2), so the stream is a
// serialization token only; gc.control_epoch metadata remains the epoch record.
//
// P5: when v1/v2 roots emit coarse settlement events into the journal, the fence
// migrates onto the per-root settlement stream and the epoch bump becomes a
// single-transaction append+fold. Until then the fence rides the v2 engine
// namespace with a dedicated event type on a dedicated per-bead stream that
// nothing folds.

// ControlFenceEngine is the journal engine the control-epoch fence appends under.
// The journal schema constrains engines to ('lumen','v2','v1') (ddl.go), so the
// fence rides the v2 (coarse-settlement) engine namespace — the "v2-engine
// control-settlement event" the blueprint names. At P2 nothing folds the fence
// streams, so the event is a pure serialization token, never projected.
const ControlFenceEngine = "v2"

// controlFenceEventType is the single (closed-vocabulary) event type appended to
// a control-epoch fence stream. Registered on every JournalStore's graph engine
// so AppendEvent accepts it (graphstore rejects unregistered types, I-5).
const controlFenceEventType = "control.epoch.fenced"

// registerControlFenceVocab registers the control-epoch fence event type on gs so
// a fence append is not rejected as an unknown (engine, type). Idempotent.
func registerControlFenceVocab(gs *graphstore.Store) {
	if gs == nil {
		return
	}
	gs.RegisterEventType(ControlFenceEngine, controlFenceEventType)
}

// IsJournalResidentID reports whether id carries the journal-residence marker
// (gcg-j<seq>) the JournalStore mints. The control-epoch fence activates only for
// journal-resident beads; a legacy bead (any other id shape) takes the unchanged
// check-then-act path even on a journal-capable store.
func IsJournalResidentID(id string) bool {
	return len(id) >= len(journalIDPrefix)+1+len(journalIDMarker) &&
		id[:len(journalIDPrefix)+1+len(journalIDMarker)] == journalIDPrefix+"-"+journalIDMarker
}

// ControlEpochFenceStreamID returns the dedicated journal stream a control bead's
// epoch writes are serialized on. Deterministic per bead, disjoint from any node
// stream_id (which is empty for façade beads at P2), so a fence append can never
// collide with fold-owned stream data.
func ControlEpochFenceStreamID(beadID string) string {
	return "control-epoch/" + beadID
}

// controlFencePayload is the fence event body. It carries only the fenced bead id
// for provenance; the fence's effect is the CAS on stream head, not the payload.
type controlFencePayload struct {
	Bead string `json:"bead"`
}

// ControlEpochFenceEvent builds the journal event a fence append commits. The
// event is not effect-bearing (empty IdemToken): it exists to advance the stream
// head so a concurrent second writer loses the CAS.
func ControlEpochFenceEvent(beadID string) graphstore.JournalEvent {
	payload, err := json.Marshal(controlFencePayload{Bead: beadID})
	if err != nil {
		// json.Marshal of a single-string struct cannot fail; fall back to a
		// minimal valid body so the event is never nil-payloaded.
		payload = []byte(`{}`)
	}
	return graphstore.JournalEvent{
		Type:    controlFenceEventType,
		Payload: payload,
	}
}
