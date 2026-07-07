package engine

import (
	"encoding/json"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
)

// Engine is the journal engine tag for every Lumen event. It satisfies the
// journal `engine IN ('lumen','v2','v1')` CHECK.
const Engine = "lumen"

// The frozen 18-type Lumen event vocabulary (blueprint §1, V-1/V-2 resolved).
// Every type is registered against the store via RegisterVocabulary; Append
// rejects anything else (I-5). Each carries a typed payload struct below — no
// map[string]any, no json.RawMessage on the wire.
//
// The reducer is TOTAL over this set (R-TOTAL): every type has a defined
// transition even where the P4.2 executor does not yet emit it. The arms this
// slice emits are run lifecycle, node.activated, node.decision (gather
// head-of-line), outcome.settled, and the effect pair; the remainder
// (channels, cancel, owned handles, attempt.minted, snapshot.anchored) are
// registered day one so the fold is total and the vocabulary never churns when
// their executor arms land (blueprint §1, R-7).
const (
	// EventRunStarted (1) opens the run: it materializes the root node and
	// seeds the frontier with it.
	EventRunStarted = "lumen.run.started"
	// EventNodeActivated (2) mints one activation, carrying its resolved
	// dependency edges (as activation keys) so the fold builds the DAG purely
	// from the journal (D-P4-1). Written once per activation key.
	EventNodeActivated = "lumen.node.activated"
	// EventNodeDecision (3) records a pure control choice (dispatch arm, guard,
	// loop, or a gather fold checkpoint) BEFORE the chosen work is minted, so
	// crash-resume re-reads the decision instead of re-deciding (I-7).
	EventNodeDecision = "lumen.node.decision"
	// EventOutcomeSettled (4) records one activation's terminal outcome. It is
	// the P4.2 rename of P1's node.settled (upcast transparently); the root's
	// outcome.settled is the run's outcome-freeze fact (V-1).
	EventOutcomeSettled = "lumen.outcome.settled"
	// EventEffectScheduled (5) records that a side-effecting step (agent `do`,
	// exec, sub-run, Tier-B attach) is about to run: appended BEFORE the effect
	// acts, carrying the idem token, the policy, and the effect spec hash.
	EventEffectScheduled = "lumen.effect.scheduled"
	// EventEffectSettled (6) pairs with EventEffectScheduled: the effect's
	// observed result (ok/failed/interrupted), captured output, and session ref.
	EventEffectSettled = "lumen.effect.settled"
	// EventAttemptMinted (7) records one retry attempt (idem-keyed on the
	// activation and attempt number). Registered; emitted when retry lands.
	EventAttemptMinted = "lumen.attempt.minted"
	// EventChannelOpened (8) opens a channel owned by an activation.
	EventChannelOpened = "lumen.channel.opened"
	// EventChannelEmit (9) writes one value into a channel (substream keyed).
	EventChannelEmit = "lumen.channel.emit"
	// EventChannelCursorPlanted (10) plants a reader cursor at a position.
	EventChannelCursorPlanted = "lumen.channel.cursor.planted"
	// EventChannelCursorAdvanced (11) advances a reader cursor by one entry.
	EventChannelCursorAdvanced = "lumen.channel.cursor.advanced"
	// EventChannelSealed (12) is the single terminal fact per channel: closed
	// or failed (V-2 merge of channel.closed / channel.failed).
	EventChannelSealed = "lumen.channel.sealed"
	// EventCancelRequested (13) requests cancellation of an activation or handle.
	EventCancelRequested = "lumen.cancel.requested"
	// EventCancelSwept (14) records the completion of a cancellation sweep.
	EventCancelSwept = "lumen.cancel.swept"
	// EventOwnedAdmitted (15) admits an in-flight async / detached-run handle.
	EventOwnedAdmitted = "lumen.owned.admitted"
	// EventOwnedSettled (16) settles an admitted handle at the await boundary.
	EventOwnedSettled = "lumen.owned.settled"
	// EventSnapshotAnchored (17) commits a fold-state snapshot anchor (§4, P4.3).
	EventSnapshotAnchored = "lumen.snapshot.anchored"
	// EventRunClosed (18) closes the run with the aggregated outcome and clears
	// the frontier. It SEALS the stream (terminal bookkeeping, V-1).
	EventRunClosed = "lumen.run.closed"
)

// eventNodeSettledLegacy is P1's provisional coarse settlement type. The
// executor no longer emits it; an upcaster (registered in init below) rewrites
// it to EventOutcomeSettled so a persistent P1-store journal folds identically
// under the v2 reducer. It is not part of the frozen vocabulary and is not
// registered for new appends.
const eventNodeSettledLegacy = "lumen.node.settled"

// EventTypes is the frozen closed vocabulary in a stable order, for
// registration against the journal store.
var EventTypes = []string{
	EventRunStarted,
	EventNodeActivated,
	EventNodeDecision,
	EventOutcomeSettled,
	EventEffectScheduled,
	EventEffectSettled,
	EventAttemptMinted,
	EventChannelOpened,
	EventChannelEmit,
	EventChannelCursorPlanted,
	EventChannelCursorAdvanced,
	EventChannelSealed,
	EventCancelRequested,
	EventCancelSwept,
	EventOwnedAdmitted,
	EventOwnedSettled,
	EventSnapshotAnchored,
	EventRunClosed,
}

// Effect policy vocabulary.
const (
	// PolicyAtMostOnce settles a crash-interrupted effect as failed rather than
	// re-acting it (the only policy P4.2 emits).
	PolicyAtMostOnce = "at_most_once"
	// PolicyAtLeastOnce re-acts a crash-interrupted effect under the SAME idem
	// token. Registered as vocabulary; opt-in per node when retry lands.
	PolicyAtLeastOnce = "at_least_once"
)

// Effect result vocabulary for EventEffectSettled.
const (
	EffectResultOK          = "ok"
	EffectResultFailed      = "failed"
	EffectResultInterrupted = "interrupted"
)

// Outcome vocabulary. pass and degraded are non-blocking (a dependent may run);
// failed, canceled, and skipped are blocking (a dependent skip-cascades).
const (
	OutcomePass     = "pass"
	OutcomeFailed   = "failed"
	OutcomeDegraded = "degraded"
	OutcomeSkipped  = "skipped"
	OutcomeCanceled = "canceled"
)

// node.decision decision kinds.
const (
	DecisionArm      = "arm"
	DecisionGuard    = "guard"
	DecisionLoop     = "loop"
	DecisionFoldCkpt = "fold_ckpt"
)

const (
	// reducerVersion is bumped on any semantic change to the fold or upcasters.
	// v2 replaces the P1 minimal fold with the DAG fold (blueprint §2).
	reducerVersion = 2
	// snapshotFormatVersion pins the on-disk lumenState layout. Bumped with the
	// v2 state shape; no v1 snapshot ever persisted (blueprint §2), so there is
	// no v1 fixture debt.
	snapshotFormatVersion = 2
)

// ---- Typed event payloads (R-CANON) ----

// runStartedPayload is the body of EventRunStarted. ir_hash pins the IR
// provenance (resume refuses a differing IR); formula_ref / input_hash are
// provenance fields the P1 upcast leaves empty.
type runStartedPayload struct {
	RootID     string `json:"root_id"`
	Name       string `json:"name"`
	IRHash     string `json:"ir_hash,omitempty"`
	FormulaRef string `json:"formula_ref,omitempty"`
	InputHash  string `json:"input_hash,omitempty"`
	CreatedAt  string `json:"created_at"`
}

// nodeActivatedPayload is the body of EventNodeActivated. After carries the
// resolved blocking dependency edges (a failed one skip-cascades this node);
// Members carries drain dependencies (a scatter aggregate / gather waits for
// them to settle with any outcome). Together they are THE DAG, in the journal.
type nodeActivatedPayload struct {
	NodeID           string   `json:"node_id"`
	Activation       string   `json:"activation"`
	ParentActivation string   `json:"parent_activation,omitempty"`
	MemberIndex      *int     `json:"member_index,omitempty"`
	After            []string `json:"after,omitempty"`
	Members          []string `json:"members,omitempty"`
	Kind             string   `json:"kind"`
}

// nodeDecisionPayload is the body of EventNodeDecision.
type nodeDecisionPayload struct {
	Activation     string `json:"activation"`
	Decision       string `json:"decision"`
	Chosen         string `json:"chosen,omitempty"`
	Guard          string `json:"guard,omitempty"`
	NextMember     string `json:"next_member,omitempty"`
	AccumulatorRef string `json:"accumulator_ref,omitempty"`
}

// outcomeSettledPayload is the body of EventOutcomeSettled.
type outcomeSettledPayload struct {
	Activation       string `json:"activation"`
	Outcome          string `json:"outcome"`
	Output           string `json:"output,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Detail           string `json:"detail,omitempty"`
	RetriesRemaining *int   `json:"retries_remaining,omitempty"`
}

// effectSpec captures the effect's inputs for provenance and hashing.
type effectSpec struct {
	Prompt   string `json:"prompt"`
	AgentRef string `json:"agent_ref,omitempty"`
}

// effectScheduledPayload is the body of EventEffectScheduled.
type effectScheduledPayload struct {
	Activation string     `json:"activation"`
	Effect     string     `json:"effect"`
	IdemToken  string     `json:"idem_token"`
	Policy     string     `json:"policy"`
	SpecHash   string     `json:"spec_hash"`
	Spec       effectSpec `json:"spec"`
}

// effectSettledPayload is the body of EventEffectSettled.
type effectSettledPayload struct {
	Activation string `json:"activation"`
	IdemToken  string `json:"idem_token"`
	Result     string `json:"result"`
	Output     string `json:"output,omitempty"`
	Session    string `json:"session,omitempty"`
	Detail     string `json:"detail,omitempty"`
}

// attemptMintedPayload is the body of EventAttemptMinted.
type attemptMintedPayload struct {
	Activation string `json:"activation"`
	Attempt    int    `json:"attempt"`
	Remaining  int    `json:"remaining"`
}

// channelOpenedPayload is the body of EventChannelOpened.
type channelOpenedPayload struct {
	ChannelID       string   `json:"channel_id"`
	OwnerActivation string   `json:"owner_activation"`
	Capabilities    []string `json:"capabilities,omitempty"`
}

// channelEmitPayload is the body of EventChannelEmit.
type channelEmitPayload struct {
	ChannelID  string `json:"channel_id"`
	ChannelPos int64  `json:"channel_pos"`
	Value      string `json:"value,omitempty"`
	ValueRef   string `json:"value_ref,omitempty"`
}

// channelCursorPayload is the shared body of the cursor plant/advance events.
type channelCursorPayload struct {
	ChannelID string `json:"channel_id"`
	ReaderKey string `json:"reader_key"`
	Position  int64  `json:"position"`
}

// channelSealedPayload is the body of EventChannelSealed (V-2).
type channelSealedPayload struct {
	ChannelID string `json:"channel_id"`
	Status    string `json:"status"`
	Reason    string `json:"reason,omitempty"`
}

// cancelRequestedPayload is the body of EventCancelRequested.
type cancelRequestedPayload struct {
	Target string `json:"target"`
	Source string `json:"source"`
}

// cancelSweptPayload is the body of EventCancelSwept.
type cancelSweptPayload struct {
	Target          string `json:"target"`
	OutcomeFrozen   bool   `json:"outcome_frozen"`
	BeadsClosed     int    `json:"beads_closed"`
	SessionsStopped int    `json:"sessions_stopped"`
}

// ownedAdmittedPayload is the body of EventOwnedAdmitted.
type ownedAdmittedPayload struct {
	Handle     string `json:"handle"`
	Activation string `json:"activation"`
	Kind       string `json:"kind"`
}

// ownedSettledPayload is the body of EventOwnedSettled.
type ownedSettledPayload struct {
	Handle  string `json:"handle"`
	Outcome string `json:"outcome"`
}

// snapshotAnchoredPayload is the body of EventSnapshotAnchored.
type snapshotAnchoredPayload struct {
	CoveredSeq  uint64 `json:"covered_seq"`
	StateHash   string `json:"state_hash"`
	SnapshotRef string `json:"snapshot_ref,omitempty"`
}

// runClosedPayload is the body of EventRunClosed.
type runClosedPayload struct {
	Outcome       string   `json:"outcome"`
	OwnedFailures []string `json:"owned_failures,omitempty"`
}

// legacyNodeSettledPayload is P1's node.settled body, the upcaster's input.
type legacyNodeSettledPayload struct {
	ID      string `json:"id"`
	Outcome string `json:"outcome"`
	Output  string `json:"output"`
}

// EventPayloadSamples maps every frozen event type to a zero-value instance of
// its typed payload struct. It is the enforceable proof that each of the 18
// types has a typed payload (no map[string]any / json.RawMessage on the wire) —
// the lumen-vocabulary peer of the events.RegisterPayload invariant. The
// executor arms this slice does not yet emit still carry their payload here, so
// a reviewer can see the full frozen contract in one place and a test can assert
// total coverage (TestEveryEventTypeHasTypedPayload).
func EventPayloadSamples() map[string]any {
	return map[string]any{
		EventRunStarted:            runStartedPayload{},
		EventNodeActivated:         nodeActivatedPayload{},
		EventNodeDecision:          nodeDecisionPayload{},
		EventOutcomeSettled:        outcomeSettledPayload{},
		EventEffectScheduled:       effectScheduledPayload{},
		EventEffectSettled:         effectSettledPayload{},
		EventAttemptMinted:         attemptMintedPayload{},
		EventChannelOpened:         channelOpenedPayload{},
		EventChannelEmit:           channelEmitPayload{},
		EventChannelCursorPlanted:  channelCursorPayload{},
		EventChannelCursorAdvanced: channelCursorPayload{},
		EventChannelSealed:         channelSealedPayload{},
		EventCancelRequested:       cancelRequestedPayload{},
		EventCancelSwept:           cancelSweptPayload{},
		EventOwnedAdmitted:         ownedAdmittedPayload{},
		EventOwnedSettled:          ownedSettledPayload{},
		EventSnapshotAnchored:      snapshotAnchoredPayload{},
		EventRunClosed:             runClosedPayload{},
	}
}

func init() {
	// Upcast P1's provisional node.settled → outcome.settled so a persistent
	// P1-store journal folds identically under the v2 reducer for the P1-emitted
	// outcomes (pass/failed); an authored skipped/canceled settle adopts the v2
	// status under the reducer-version bump (no live P1 journal contains one — the
	// P1 skeleton was exec-only). The rewrite maps
	// {id, outcome, output} → {activation: id+":0", outcome, output}; run.started
	// and run.closed keep their type names (v2 unmarshal tolerates the P1
	// payload's absent fields), so only this one type needs a rewrite (blueprint
	// §1). The type name changes, so the fold's non-advancing-rewrite guard is
	// satisfied.
	fold.RegisterUpcaster(Engine, eventNodeSettledLegacy, ir025,
		func(_ /*typ*/, _ /*irVersion*/ string, payload []byte) (string, string, []byte, error) {
			var p legacyNodeSettledPayload
			if err := json.Unmarshal(payload, &p); err != nil {
				return "", "", nil, err
			}
			out, err := canonPayload(outcomeSettledPayload{
				Activation: p.ID + ":0",
				Outcome:    p.Outcome,
				Output:     p.Output,
			})
			if err != nil {
				return "", "", nil, err
			}
			return EventOutcomeSettled, ir025, out, nil
		})
}

// ir025 is the lumen.ir contract version the P1 skeleton stamped on its events.
const ir025 = "0.2.5"
