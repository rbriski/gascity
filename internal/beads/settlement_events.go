package beads

import (
	"encoding/json"
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// Coarse settlement/lifecycle provenance (P5.3). The v1/v2 control engines stay
// mutation-primary: a root/attempt/workflow reaches its terminal state via the
// existing gc.outcome column write — that column write is the projection of
// record. AFTER that write, the dispatcher emits ONE coarse settlement event per
// root/attempt/workflow SETTLEMENT into the city's shared graph journal, so a
// single provenance stream spans all three engines (lumen fine-grained journal,
// v2/v1 coarse). These events are provenance ONLY — nothing folds them into
// Tier-A (no nodes/edges/frontier delta); the projection of record for v1/v2
// remains the mutation-primary columns. The provenance fold + read surface land
// in P5.4.
//
// Coarse means coarse (the cache-reconcile wisp-flood lesson): exactly one event
// per SETTLEMENT — never per tick, never per bead.updated. The emitter lives in
// internal/dispatch (NewJournalSettlementEmitter); this file is the append half,
// mirroring the control-epoch fence vocabulary in journal_control_fence.go: the
// engine tags, the closed event vocabulary, the per-root stream identity, the
// typed payload, and the outcome-scoped idempotency token.

// Settlement engine tags. The journal schema constrains engines to
// ('lumen','v2','v1') (graphstore/ddl.go); coarse settlements ride the v2 and v1
// engine namespaces. Dispatch (ProcessControl processes only graph.v2 control
// beads) emits under v2; v1 root settlements from the autoclose/wisp-GC anchors
// land in P5.4.
const (
	SettlementEngineV2 = "v2"
	SettlementEngineV1 = "v1"
)

// Coarse settlement event types (closed vocabulary, registered on every
// JournalStore so an emit is never rejected as an unknown (engine, type), I-5).
// The names deliberately mirror the lumen settlement concepts so the unified
// stream reads uniformly, but they are a SEPARATE vocabulary under the v2/v1
// tags — the lumen 18-type vocabulary is frozen, lumen-prefixed, and carries
// activation-key semantics these coarse facts do not have.
const (
	// SettlementRootType records that a workflow/molecule ROOT reached its
	// terminal outcome (emitted after the root's gc.outcome column write).
	SettlementRootType = "settlement.root"
	// SettlementAttemptType records that a logical retry/ralph step settled after
	// its attempt(s) — the logical settle, NOT every per-attempt eval/check close
	// (settling once per logical step is what makes it coarse).
	SettlementAttemptType = "settlement.attempt"
	// SettlementWorkflowFinalizedType records that the workflow-finalize control
	// bead completed (including the missing_root arm).
	SettlementWorkflowFinalizedType = "settlement.workflow.finalized"
)

// SettlementStreamID returns the per-root stream a root's coarse settlement
// events are appended to. Per-root total order is what the unified-provenance
// claim needs; the shape mirrors ControlEpochFenceStreamID's control-epoch/<id>.
func SettlementStreamID(rootID string) string {
	return "settlement/" + rootID
}

// SettlementPayload is the typed body of a coarse settlement event (no
// map[string]any / json.RawMessage on the wire). It is deterministic and
// CLOCK-FREE: time provenance is the journal's appended_at framing column, never
// a payload field, so an idempotent redo of the same settlement produces a
// byte-identical payload and dedupes cleanly (R-IDEM). Root is the molecule root
// (the per-root stream key); Bead is the settled bead (== Root for a root
// settlement). Kind and StoreRef are optional provenance pointers populated by
// the richer P5.4 anchors; Attempt is set only for settlement.attempt.
type SettlementPayload struct {
	Root     string `json:"root"`
	Bead     string `json:"bead"`
	Kind     string `json:"kind,omitempty"`
	Outcome  string `json:"outcome"`
	Attempt  int    `json:"attempt,omitempty"`
	StoreRef string `json:"store_ref,omitempty"`
}

// SettlementIdemToken builds the outcome-scoped idempotency token for a coarse
// settlement: <type>/<bead>/<outcome> (+ /<attempt> for attempt settles). A
// crash-redo of the SAME settlement mints the SAME token and dedupes (R-IDEM); a
// genuinely different re-settlement (reopen → re-close with a different outcome)
// mints a different token and is recorded as a second provenance fact, which is
// correct.
//
// LOW-3: because the token is outcome-scoped (not occurrence-scoped), a genuine
// same-outcome re-settlement — a bead reopened and re-closed with the identical
// outcome and attempt — coalesces to the first event's token and dedupes, so the
// second real settlement is invisible in the provenance stream. This is accepted:
// such a reopen→re-close-to-the-same-outcome is rare, and occurrence-scoping the
// token would forfeit the crash-redo idempotency that is the common case.
func SettlementIdemToken(typ, bead, outcome string, attempt int) string {
	token := typ + "/" + bead + "/" + outcome
	if attempt > 0 {
		token += "/" + strconv.Itoa(attempt)
	}
	return token
}

// SettlementEvent builds the journal event for a coarse settlement: the typed
// payload marshaled deterministically (json.Marshal of this map-free struct is
// stable, and the journal stores the bytes verbatim and R-IDEM-compares their
// hash), stamped with the closed-vocabulary type and the outcome-scoped idem
// token. IRContractVersion is left "" — coarse v1/v2 events carry no IR contract.
func SettlementEvent(typ string, p SettlementPayload) (graphstore.JournalEvent, error) {
	payload, err := json.Marshal(p)
	if err != nil {
		return graphstore.JournalEvent{}, fmt.Errorf("marshaling settlement payload: %w", err)
	}
	return graphstore.JournalEvent{
		Type:      typ,
		IdemToken: SettlementIdemToken(typ, p.Bead, p.Outcome, p.Attempt),
		Payload:   payload,
	}, nil
}

// registerSettlementVocab registers the coarse settlement event types on gs so an
// emit is never rejected as an unknown (engine, type). Additive and idempotent,
// registered at JournalStore construction alongside the control-fence vocab. v2
// emits all three coarse types; v1 emits only root settlements (wired in P5.4).
func registerSettlementVocab(gs *graphstore.Store) {
	if gs == nil {
		return
	}
	gs.RegisterEventType(SettlementEngineV2, SettlementRootType)
	gs.RegisterEventType(SettlementEngineV2, SettlementAttemptType)
	gs.RegisterEventType(SettlementEngineV2, SettlementWorkflowFinalizedType)
	gs.RegisterEventType(SettlementEngineV1, SettlementRootType)
}
