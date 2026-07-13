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
	// EventEffectScheduled (5) records that a side-effecting step is about to run:
	// appended BEFORE the effect acts, carrying the idem token, the policy, and the
	// effect spec hash. In P4.3 ONLY agent `do` emits this pair, which is what makes
	// do at-most-once across a crash. `exec` (and, later, sub-run / detached-run attach)
	// run WITHOUT an effect record today, so they are at-least-once across resume;
	// extending the effect discipline to them is a deferred follow-up (M1).
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

// Pool-mode dispatch vocabulary. A pool-mode node's work is an ordinary
// fold_owned=0 work bead in the city work store (REDESIGN); the driver records
// its store-minted id in a write-once owned.admitted{kind:work_bead, bead_id}
// dispatch fact and observes the bead's ordinary close for settlement. ZERO
// hardcoded roles: worker-class is keyed by DispatchModePool, not a role name.
const (
	// DispatchModePool is the node.activated dispatch_mode marking a node
	// pool-dispatched (vs the default "" engine-driven path).
	DispatchModePool = "pool"
	// OwnedKindWorkBead is the owned.admitted kind for the real-bead do path
	// (REDESIGN §1.2): a ready pool-mode do's work is an ordinary fold_owned=0 work
	// bead in the city work store, and the driver records its store-minted id in a
	// write-once owned.admitted{kind:work_bead, bead_id} dispatch fact. Every other
	// owned kind (async / detached_run) folds as deferred no-op bookkeeping.
	OwnedKindWorkBead = "work_bead"
)

const (
	// reducerVersion is bumped on any semantic change to the fold or upcasters.
	// v2 replaces the P1 minimal fold with the DAG fold (blueprint §2).
	//
	// P4.3 stays at v2 despite applyRunStarted now folding a new run.started field
	// (input_hash): no snapshot predates P4.3 (snapshots are new in this slice), so
	// no persisted snapshot carries a stale reducer_version to gate, and an old
	// journal without input_hash folds identically (the omitempty field decodes to
	// "", the pre-P4.3 value). A bump would only strand snapshots that do not exist,
	// so it is unnecessary.
	//
	// L0 DECISION — stays at v2 despite a frontier RE-KEYING (MED-1). L0 changed the
	// leaf frontier row's node_id/id from the activation key to the BARE node id
	// (activationNodeID), so the frontier is a claim surface hydratable via
	// `nodes WHERE id IN (...)`. This changes the fold's emitted FRONTIER deltas for
	// a pre-L0 stream: a persisted pre-L0 stream that seeded an activation-keyed
	// frontier row (e.g. "x:0") and is resumed under L0 emits FrontierDelete "x"
	// (bare) on settle, which does NOT delete the stale "x:0" row — leaving a phantom
	// ready row until a RebuildTierA rewrites the whole frontier from state. A version
	// bump is NOT taken because (a) the pre-L0 lumen frontier was an observer-only
	// dead path — activation-keyed rows hydrated to nothing through Arm B's
	// `nodes WHERE id IN (...)` join, so nothing ever claimed off them — and (b) there
	// are NO persisted pre-L0 lumen streams in any real deployment (this branch is
	// unpushed local dev), so a bump would only force a resnapshot of snapshots that
	// do not exist. ANY pre-L0 stream that somehow survives MUST be given a one-time
	// RebuildTierA (which rewrites the frontier from state, dropping the stale
	// activation-keyed rows) before its L0 frontier is trusted.
	//
	// REDESIGN (real-bead do node) — v3. A ready pool-mode do's work is an ordinary
	// fold_owned=0 work bead in the city work store: the driver records its store-minted
	// id in a write-once owned.admitted{kind:work_bead, bead_id} dispatch fact, folds
	// nodeState.BeadID, projects the pool node as a PLAIN step (no claimable frontier
	// row, no dispatch_mode marker — the real bead is the only claim surface), and
	// settles the do from the bead's ordinary close through the existing outcome.settled.
	// The former Tier-B claim-as-append arms (owned.admitted/owned.settled kind=tier_b)
	// were REMOVED with the adapter: every owned kind other than work_bead now folds as
	// deferred no-op bookkeeping. That removal changes the fold of NO real-bead stream —
	// no real-bead stream ever carried a tier_b claim — so v3 is unchanged by it. The
	// bump to 3 was taken up front (against the never-persisted v2) so no persisted Lumen
	// stream or snapshot exists on any real deployment (this branch is unpushed local
	// dev); fold.Fold's version gate strands any (nonexistent) v2 snapshot LOUDLY
	// (ErrReducerVersionSkew, resume.go), so the bump costs nothing.
	//
	// RECOVER (error binding) — v4. applyOutcomeSettled now folds the settle's Detail
	// into a new nodeState.Detail field so a recover catch can bind {{ error.reason }}
	// from the guarded's failure. UNLIKE the BeadID field, this is NOT byte-identical:
	// Detail is ALREADY non-empty on many existing settles (skip-cascade, "no branch
	// taken", a failed inline do, invalid_input, interrupted effects), so folding it
	// changes the refolded state bytes / StateHash of essentially every stream. A
	// persisted v3 snapshot would then silently diverge from a drop+refold — so this
	// takes the honest bump. It is free: no persisted v3 Lumen snapshot exists on any
	// real deployment (this branch is unpushed local dev), and fold.Fold's version gate
	// strands any (nonexistent) v3 snapshot LOUDLY (ErrReducerVersionSkew, resume.go).
	reducerVersion = 4
	// snapshotFormatVersion pins the on-disk lumenState layout. Bumped 2 → 3 with
	// reducerVersion for the additive nodeState.BeadID field, then 3 → 4 with the
	// nodeState.Detail field (the recover error binding); no v3 snapshot ever persisted
	// (unpushed branch), so there is no fixture debt.
	snapshotFormatVersion = 4
)

// ---- Typed event payloads (R-CANON) ----

// runStartedPayload is the body of EventRunStarted. ir_hash pins the IR
// provenance (resume refuses a differing IR); formula_ref / input_hash are
// provenance fields the P1 upcast leaves empty.
//
// DefaultRoute is the L2 controller-loop provenance field: the run's default pool
// route for do nodes with no agentRef, stamped by EnqueueRun so a controller
// restart can recover which pool the run's do work targets (the journal pins
// ir_hash/input_hash/formula_ref but carried no route). It is additive and
// omitempty, and — like formula_ref — it is folded by NO reducer arm
// (applyRunStarted ignores it), so it changes no state or projection: an old
// journal without it folds identically (the field decodes to ""), and drop+refold
// stays byte-identical. That is why it needs no reducerVersion bump (the same
// additive-omitempty precedent as input_hash in P4.3).
type runStartedPayload struct {
	RootID       string `json:"root_id"`
	Name         string `json:"name"`
	IRHash       string `json:"ir_hash,omitempty"`
	FormulaRef   string `json:"formula_ref,omitempty"`
	InputHash    string `json:"input_hash,omitempty"`
	DefaultRoute string `json:"default_route,omitempty"`
	CreatedAt    string `json:"created_at"`
}

// nodeActivatedPayload is the body of EventNodeActivated. After carries the
// resolved blocking dependency edges (a failed one skip-cascades this node);
// Members carries drain dependencies (a scatter aggregate / gather waits for
// them to settle with any outcome). Together they are THE DAG, in the journal.
//
// DispatchMode is the pool-dispatch knob (default "" = engine-driven; "pool" =
// pool-dispatched). It is kind/label-keyed, never a role name: a ready pool node's
// work is dispatched as an ordinary work bead in the city work store (REDESIGN),
// not run inline. It is additive and omitempty, so a stream that never set it folds
// byte-identically to the pre-pool reducer.
//
// Route and Prompt are the pool-dispatch fields the driver (engine.Advance) stamps
// when it materializes a pool-mode do: Route is the pool the work is routed to (the
// dispatched bead's gc.routed_to), Prompt is the rendered agent prompt (the
// dispatched bead's Description). Both are carried in the fold so a re-Advance
// re-dispatches byte-identically. Both additive and omitempty, so an engine-driven
// node folds byte-identically.
//
// Duration is a timeout wrapper's raw duration literal (VERBATIM — "5m"), stamped ONLY on a
// timeout node's OWN node.activated (appendActivated, from the unit's timeoutSpec); every other
// activation omits it. It is ADVISORY journal metadata: enforcement is gc-side, off this
// journal field (never the bead), and the reducer does NOT fold it (applyNodeActivated omits it
// from nodeState), so it is snapshot/StateHash-transparent and additive/omitempty — a
// non-timeout node.activated folds byte-identically and reducerVersion STAYS 4.
//
// Metadata is a pool-mode do's static routing/affinity map (chiefly gc.continuation_group),
// stamped ONLY on a pool node's node.activated by appendPoolActivated (from u.leaf.metadata);
// the engine-mode appendActivated omits it, so a non-pool activation folds byte-identically.
// It follows the Duration precedent EXACTLY — payload-only observability parity, the reducer
// does NOT fold it (applyNodeActivated ignores it), so it is snapshot/StateHash-transparent and
// additive/omitempty, and reducerVersion STAYS 4. The authoritative copy rides onto the minted
// work bead via WorkDispatch.Metadata; both derive from the same static IR unit in one pass.
type nodeActivatedPayload struct {
	NodeID           string            `json:"node_id"`
	Activation       string            `json:"activation"`
	ParentActivation string            `json:"parent_activation,omitempty"`
	MemberIndex      *int              `json:"member_index,omitempty"`
	After            []string          `json:"after,omitempty"`
	Members          []string          `json:"members,omitempty"`
	Kind             string            `json:"kind"`
	DispatchMode     string            `json:"dispatch_mode,omitempty"`
	Route            string            `json:"route,omitempty"`
	Prompt           string            `json:"prompt,omitempty"`
	Duration         string            `json:"duration,omitempty"`
	Metadata         map[string]string `json:"metadata,omitempty"`
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
//
// Retryable is the L5 attempt-loop classification DATA for an engine-inline exec
// settle: it is true iff the exec's exit code is in exitMap.retryable, so the
// retry arm's re-attempt decision is a fold of journaled fact (nodeState.Retryable),
// never a Go inference. It is additive and omitempty — a non-retryable settle (every
// pre-L5 settle, and every non-exec settle) omits it and folds byte-identically.
type outcomeSettledPayload struct {
	Activation       string `json:"activation"`
	Outcome          string `json:"outcome"`
	Output           string `json:"output,omitempty"`
	Reason           string `json:"reason,omitempty"`
	Detail           string `json:"detail,omitempty"`
	RetriesRemaining *int   `json:"retries_remaining,omitempty"`
	Retryable        bool   `json:"retryable,omitempty"`
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

// effectSettledPayload is the body of EventEffectSettled. NodeOutcome memoizes
// the resolved node outcome (pass/degraded/failed) the effect produced, so a
// resume that finds effect.settled committed but outcome.settled missing can
// settle the node from this record WITHOUT re-invoking the host (B1). Result
// (ok/failed/interrupted) alone is lossy — ok maps to both pass and degraded —
// so the node outcome is recorded explicitly rather than re-derived.
type effectSettledPayload struct {
	Activation  string `json:"activation"`
	IdemToken   string `json:"idem_token"`
	Result      string `json:"result"`
	NodeOutcome string `json:"node_outcome,omitempty"`
	Output      string `json:"output,omitempty"`
	Session     string `json:"session,omitempty"`
	Detail      string `json:"detail,omitempty"`
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

// ownedAdmittedPayload is the body of EventOwnedAdmitted. For the real-bead do
// path (REDESIGN §1.2) it is the dispatch fact: kind=OwnedKindWorkBead and BeadID
// carries the store-minted id of the ordinary work bead the driver created for a
// pool-mode do. BeadID is env-specific but recorded ONCE at dispatch and replayed
// on every refold, never re-minted — the same determinism class as the lease epoch
// and effectSettledPayload.Session (DET holds, §1.5). Assignee/BeadID are additive
// and omitempty, so the deferred async/detached-run uses of this type (kind=async|
// detached_run) fold byte-identically without them.
type ownedAdmittedPayload struct {
	Handle     string `json:"handle"`
	Activation string `json:"activation"`
	Kind       string `json:"kind"`
	Assignee   string `json:"assignee,omitempty"`
	BeadID     string `json:"bead_id,omitempty"`
}

// ownedSettledPayload is the body of EventOwnedSettled. It is registered day one
// for the deferred async/detached-run await boundary (kind=async|detached_run,
// Outcome/Output). It carries NO real-bead do settlement: a do settles through the
// existing outcome.settled (applyOutcomeSettled), not here. Output is additive and
// omitempty.
type ownedSettledPayload struct {
	Handle  string `json:"handle"`
	Kind    string `json:"kind"`
	Outcome string `json:"outcome"`
	Output  string `json:"output,omitempty"`
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
