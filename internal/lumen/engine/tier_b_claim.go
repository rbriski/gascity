package engine

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// Tier-B claim-as-append (P4.5). A pool-mode node materializes as a
// worker-claimable Tier-B work bead on the run's journal root — GraphClass,
// journal-native underneath, bead-compatible on the surface (B1/08). A worker
// claims and closes it exactly as it claims any bead (bd ready → claim → do →
// close); this layer TRANSLATES those two writes into journal appends:
//
//   - claim  → a CAS owned.admitted (kind=tier_b, assignee) at the stream head;
//   - close  → an owned.settled (outcome, output).
//
// The Tier-A projection (assignee/status) is a PURE FOLD of those events — the
// reducer's applyOwnedAdmitted/applyOwnedSettled arms — re-derived by folding the
// journal, never a raw column UPDATE. So the projection stays write-closed
// (fold_owned=1) for Tier-B beads too, and a drop+refold reproduces it exactly.
//
// The CAS is loud (the S0.4 kill for claims): two workers racing to claim one
// bead → exactly one wins the append, the loser gets a typed ErrTierBClaimConflict
// (wrapping graphstore.ErrWrongExpectedVersion / ErrIdemTokenReuse), NEVER a
// silent overwrite. ZERO hardcoded roles: worker-class is keyed by
// DispatchModePool, not a role name.
//
// This file is the store-facing translation and deliberately lives beside the
// reducer in package engine: the fold semantics are engine-owned, and
// internal/beads (the persistence substrate) may not depend upward on the engine
// (enginehost → beads would cycle). The JournalStore wraps the same
// *graphstore.Store these functions operate on, so a claim/close reflects through
// its beads.Store journal capabilities (ReadStream / StreamHead) unchanged. The
// remaining full-integration wiring — routing the worker's beads.Store
// claim/close through here and surfacing Tier-B fold-owned beads in Ready() so
// real `bd ready` / `gc hook --claim` see them — lives at the composition root
// (cmd/gc), which may import both packages; see TestWorkerClaimsLumenWorkBead*.

var (
	// ErrTierBNotClaimable reports that a bead is not a claimable pool-mode Tier-B
	// work bead (absent, engine-driven, or already settled).
	ErrTierBNotClaimable = errors.New("lumen tier-b: bead is not a claimable pool-mode work bead")

	// ErrTierBAlreadyClaimed reports that a bead is already held by a DIFFERENT
	// worker — the loud, no-overwrite outcome of a late claim.
	ErrTierBAlreadyClaimed = errors.New("lumen tier-b: work bead already claimed by another worker")

	// ErrTierBClaimConflict reports that a claim lost the append CAS race: a
	// concurrent claimant committed first. It wraps the underlying journal
	// conflict (ErrWrongExpectedVersion / ErrIdemTokenReuse) so callers can retry
	// (re-read, re-claim) without ever silently clobbering the winner.
	ErrTierBClaimConflict = errors.New("lumen tier-b: claim lost the append CAS race")
)

// TierBWorkSpec describes a claimable Tier-B work bead to materialize on a run
// root. It is engine-driven configuration, never a role: a pool-mode `do` node
// lowers to one claimable work bead. CreatedAt is a payload timestamp — the fold
// is clock-free, so the caller supplies it (RFC3339Nano).
type TierBWorkSpec struct {
	RunName   string // run title, applied only when the stream is fresh (head 0)
	CreatedAt string // RFC3339Nano payload timestamp threaded onto the projected rows
	NodeID    string // the do-node id (becomes the work bead id in the projection)
	Kind      string // ir kind (e.g. "do"); free-form, never a role name
}

// tierBClaimToken is the write-once idempotency token for a handle's claim. It
// deliberately excludes the assignee: a second claim of the same handle by a
// DIFFERENT worker rebinds this token to a divergent payload and is rejected
// loudly (ErrIdemTokenReuse); an honest re-claim by the SAME worker is a
// byte-identical replay and dedupes to success. Together with the head CAS this
// makes a claim write-once per handle with no silent overwrite.
func tierBClaimToken(activation string) string { return "tier-b-claim:" + activation }

// tierBSettleToken is the write-once idempotency token for a handle's settle: a
// re-settle with a different outcome is rejected loudly, an identical one dedupes.
func tierBSettleToken(activation string) string { return "tier-b-settle:" + activation }

// MaterializeTierBWork mints a worker-claimable Tier-B work bead on the run root
// stream and returns its activation key (its id in the projection). It seeds
// run.started when the stream is fresh, then appends a pool-mode node.activated
// whose fold projects an OPEN, claimable, fold-owned node — the write-closed
// Tier-B work bead. This is the "represented as a claimable work bead on the
// journal root" half of P4.5; lowering it from a pool-mode `do` node inside the
// executor is the remaining engine wiring (see the file header).
func MaterializeTierBWork(ctx context.Context, store *graphstore.Store, streamID string, spec TierBWorkSpec) (string, error) {
	if store == nil {
		return "", fmt.Errorf("lumen tier-b: nil store")
	}
	if streamID == "" || spec.NodeID == "" {
		return "", fmt.Errorf("lumen tier-b: materialize needs a stream id and node id")
	}
	RegisterVocabulary(store)

	head, err := store.Head(ctx, streamID)
	if err != nil {
		return "", fmt.Errorf("lumen tier-b: reading stream head: %w", err)
	}
	if head == 0 {
		started, err := canonPayload(runStartedPayload{RootID: streamID, Name: spec.RunName, CreatedAt: spec.CreatedAt})
		if err != nil {
			return "", err
		}
		res, err := appendAndProject(ctx, store, streamID, 0, graphstore.JournalEvent{
			Type:              EventRunStarted,
			IRContractVersion: ir025,
			IdemToken:         streamID + ":run.started",
			Payload:           started,
		})
		if err != nil {
			return "", fmt.Errorf("lumen tier-b: seeding run.started: %w", err)
		}
		head = res.FirstSeq
	}

	activation := spec.NodeID + ":0"
	activated, err := canonPayload(nodeActivatedPayload{
		NodeID:       spec.NodeID,
		Activation:   activation,
		Kind:         spec.Kind,
		DispatchMode: DispatchModePool,
	})
	if err != nil {
		return "", err
	}
	if _, err := appendAndProject(ctx, store, streamID, head, graphstore.JournalEvent{
		Type:              EventNodeActivated,
		IRContractVersion: ir025,
		IdemToken:         streamID + ":activated:" + activation,
		Payload:           activated,
	}); err != nil {
		return "", fmt.Errorf("lumen tier-b: materializing work bead %q: %w", spec.NodeID, err)
	}
	return activation, nil
}

// ClaimTierBWork translates a worker's claim of a Tier-B work bead into a CAS
// owned.admitted append + fold. The projection's assignee/status is a PURE FOLD
// of the appended event, never a direct column write. The CAS is loud: a
// concurrent claimant that loses the append gets ErrTierBClaimConflict (never a
// silent overwrite); a late claimant whose target is already owned by another
// worker gets ErrTierBAlreadyClaimed; an honest same-worker re-claim is
// idempotent success.
func ClaimTierBWork(ctx context.Context, store *graphstore.Store, streamID, activation, assignee string) error {
	if store == nil {
		return fmt.Errorf("lumen tier-b: nil store")
	}
	if streamID == "" || activation == "" {
		return fmt.Errorf("lumen tier-b: claim needs a stream id and activation")
	}
	if assignee == "" {
		return fmt.Errorf("lumen tier-b: claim needs an assignee")
	}
	RegisterVocabulary(store)

	node, ok, err := readTierBNode(ctx, store, streamID, activation)
	if err != nil {
		return err
	}
	if !ok || node.dispatchMode != DispatchModePool {
		return fmt.Errorf("lumen tier-b: %q: %w", activationNodeID(activation), ErrTierBNotClaimable)
	}
	if node.settled {
		return fmt.Errorf("lumen tier-b: %q is already settled: %w", node.id, ErrTierBNotClaimable)
	}
	if node.assignee != "" && node.assignee != assignee {
		return fmt.Errorf("lumen tier-b: %q is held by %q: %w", node.id, node.assignee, ErrTierBAlreadyClaimed)
	}

	head, err := store.Head(ctx, streamID)
	if err != nil {
		return fmt.Errorf("lumen tier-b: reading stream head: %w", err)
	}
	payload, err := canonPayload(ownedAdmittedPayload{
		Handle:     activation,
		Activation: activation,
		Kind:       OwnedKindTierB,
		Assignee:   assignee,
	})
	if err != nil {
		return err
	}
	if _, err := appendAndProject(ctx, store, streamID, head, graphstore.JournalEvent{
		Type:              EventOwnedAdmitted,
		IRContractVersion: ir025,
		IdemToken:         tierBClaimToken(activation),
		Payload:           payload,
	}); err != nil {
		if errors.Is(err, graphstore.ErrWrongExpectedVersion) || errors.Is(err, graphstore.ErrIdemTokenReuse) {
			return fmt.Errorf("lumen tier-b: claim of %q by %q lost the race: %w", node.id, assignee, ErrTierBClaimConflict)
		}
		// L2: a raw graphstore.ErrLeaseFenced propagates here UNWRAPPED. It means the
		// driver (Advance) re-acquired the writer lease and bumped the epoch between
		// this claim's CurrentLeaseEpoch read and its append (a re-acquire race), so
		// this cooperative append was fenced. It is RETRYABLE — the controller loop
		// must re-read the head/epoch and re-claim, exactly as it retries
		// ErrTierBClaimConflict — NOT surface it as a terminal claim failure. Mapping
		// ErrLeaseFenced onto a typed retryable claim error is L2 controller-loop work.
		return err
	}
	return nil
}

// SettleTierBWork translates a worker's close of a Tier-B work bead into an
// owned.settled append + fold: the projection folds the bead to its terminal
// outcome status (assignee retained for provenance) and drives the rest of the
// run's DAG. outcome is a Lumen outcome (pass / failed / degraded / skipped /
// canceled); the dispatch-firewall mapping of a raw gc.outcome onto it is the
// caller's (the settlement-observer's) job. A byte-identical re-settle is
// idempotent; a divergent one is rejected loudly at the append.
func SettleTierBWork(ctx context.Context, store *graphstore.Store, streamID, activation, outcome, output string) error {
	if store == nil {
		return fmt.Errorf("lumen tier-b: nil store")
	}
	if streamID == "" || activation == "" {
		return fmt.Errorf("lumen tier-b: settle needs a stream id and activation")
	}
	if !isSettleableOutcome(outcome) {
		return fmt.Errorf("lumen tier-b: settle outcome %q is not a Lumen outcome", outcome)
	}
	RegisterVocabulary(store)

	node, ok, err := readTierBNode(ctx, store, streamID, activation)
	if err != nil {
		return err
	}
	if !ok || node.dispatchMode != DispatchModePool {
		return fmt.Errorf("lumen tier-b: %q: %w", activationNodeID(activation), ErrTierBNotClaimable)
	}

	head, err := store.Head(ctx, streamID)
	if err != nil {
		return fmt.Errorf("lumen tier-b: reading stream head: %w", err)
	}
	payload, err := canonPayload(ownedSettledPayload{Handle: activation, Kind: OwnedKindTierB, Outcome: outcome, Output: output})
	if err != nil {
		return err
	}
	if _, err := appendAndProject(ctx, store, streamID, head, graphstore.JournalEvent{
		Type:              EventOwnedSettled,
		IRContractVersion: ir025,
		IdemToken:         tierBSettleToken(activation),
		Payload:           payload,
	}); err != nil {
		if errors.Is(err, graphstore.ErrWrongExpectedVersion) || errors.Is(err, graphstore.ErrIdemTokenReuse) {
			return fmt.Errorf("lumen tier-b: settle of %q lost the race: %w", node.id, ErrTierBClaimConflict)
		}
		return err
	}
	return nil
}

// isSettleableOutcome reports whether o is a Lumen outcome an owned.settled may
// carry.
func isSettleableOutcome(o string) bool {
	switch o {
	case OutcomePass, OutcomeFailed, OutcomeDegraded, OutcomeSkipped, OutcomeCanceled:
		return true
	default:
		return false
	}
}

// appendAndProject commits one lumen event at expectedVersion (the loud CAS),
// then re-derives the Tier-A projection by folding the journal — a drop+refold,
// never a direct column write. This is what keeps the projection a pure fold and
// write-closed for Tier-B beads, and is why a claim/settle is byte-identical to a
// from-genesis rebuild (DET-T-17).
//
// The append carries the stream's CURRENT lease epoch (blueprint correction #1),
// NOT a hardcoded 0. A claim/settle is a cross-process cooperative append onto a
// stream the driver (engine.Advance) leases per-Advance: a stale epoch 0 would be
// PERMANENTLY fenced on any stream the driver has ever driven (its lease row's
// epoch is >= 1, preserved across release), wedging every future claim. Reading
// the live epoch makes the append a same-generation cooperative write; a driver
// re-acquire that bumps the epoch fences a racing claim loudly (ErrLeaseFenced),
// which the caller re-reads and retries. An unfenced stream (no lease row) reads
// epoch 0 and appends exactly as before.
func appendAndProject(ctx context.Context, store *graphstore.Store, streamID string, expectedVersion uint64, ev graphstore.JournalEvent) (graphstore.AppendResult, error) {
	epoch, err := store.CurrentLeaseEpoch(ctx, streamID)
	if err != nil {
		return graphstore.AppendResult{}, err
	}
	res, err := store.Append(ctx, streamID, Engine, expectedVersion, epoch, []graphstore.JournalEvent{ev})
	if err != nil {
		return res, err
	}
	if err := store.RebuildTierA(ctx, Reducer(), streamID); err != nil {
		return res, fmt.Errorf("lumen tier-b: projecting stream %q: %w", streamID, err)
	}
	return res, nil
}

// tierBNodeView is the projected state a claim/settle guard reads: the fold's
// output (nodes + node_metadata), never the journal. A worker's `bd ready` reads
// the same projection.
type tierBNodeView struct {
	id           string
	status       string
	assignee     string
	dispatchMode string
	settled      bool
}

// readTierBNode reads the projected (fold-owned) row for an activation IN THE
// GIVEN STREAM. absent is (view{}, false, nil). settled is derived from a terminal
// status. The stream_id filter is load-bearing: nodes.id is a global PK but
// executor node ids are IR-local (not run-namespaced), so a claim naming a pool
// node of another stream would otherwise pass the pre-check and poison the target
// stream (MED-2). Scoping the read to streamID rejects the cross-stream claim.
func readTierBNode(ctx context.Context, store *graphstore.Store, streamID, activation string) (tierBNodeView, bool, error) {
	id := activationNodeID(activation)
	v := tierBNodeView{id: id}
	err := store.ReadDB().QueryRowContext(ctx,
		`SELECT status, COALESCE(assignee, '') FROM nodes WHERE id = ? AND stream_id = ? AND fold_owned = 1`, id, streamID,
	).Scan(&v.status, &v.assignee)
	if errors.Is(err, sql.ErrNoRows) {
		return tierBNodeView{}, false, nil
	}
	if err != nil {
		return tierBNodeView{}, false, fmt.Errorf("lumen tier-b: reading projected node %q: %w", id, err)
	}
	var dm sql.NullString
	err = store.ReadDB().QueryRowContext(ctx,
		`SELECT value FROM node_metadata WHERE node_id = ? AND key = ?`, id, DispatchModeMetaKey,
	).Scan(&dm)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// no dispatch_mode marker: engine-driven node
	case err != nil:
		return tierBNodeView{}, false, fmt.Errorf("lumen tier-b: reading dispatch mode of %q: %w", id, err)
	default:
		v.dispatchMode = dm.String
	}
	switch v.status {
	case "open", StatusClaimed:
		v.settled = false
	default:
		v.settled = true
	}
	return v, true, nil
}
