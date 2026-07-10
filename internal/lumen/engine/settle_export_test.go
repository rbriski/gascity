package engine

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// This file exposes a test-only settle helper to the reducer/DAG/DET harness
// (package engine_test). It is a _test.go file, so it compiles ONLY into the test
// binary and adds nothing to the production API.

// SettleWorkForTest appends an outcome.settled for a pool-mode do activation and
// rebuilds the Tier-A projection — the synchronous analog of the controller's
// observe arm, which appends exactly this event (same idem token, same
// failed⇒retryable stamp) when a dispatched work bead closes (REDESIGN §1.4). The
// reducer/DAG/DET tests use it to drive a pool do to settlement without a real work
// store; a re-settle with the same outcome dedupes on the write-once idem token,
// matching the driver. It appends at the current head under the stream's live lease
// epoch, the cooperative-append discipline every lumen append follows.
func SettleWorkForTest(ctx context.Context, store *graphstore.Store, streamID, activation, outcome, output string) error {
	if store == nil {
		return fmt.Errorf("lumen: settle for test: nil store")
	}
	RegisterVocabulary(store)
	payload, err := canonPayload(outcomeSettledPayload{
		Activation: activation,
		Outcome:    outcome,
		Output:     output,
		Retryable:  outcome == OutcomeFailed,
	})
	if err != nil {
		return err
	}
	head, err := store.Head(ctx, streamID)
	if err != nil {
		return err
	}
	epoch, err := store.CurrentLeaseEpoch(ctx, streamID)
	if err != nil {
		return err
	}
	if _, err := store.Append(ctx, streamID, Engine, head, epoch, []graphstore.JournalEvent{{
		Type:              EventOutcomeSettled,
		IRContractVersion: ir025,
		IdemToken:         streamID + ":" + activation + ":settled",
		Payload:           payload,
	}}); err != nil {
		return err
	}
	return store.RebuildTierA(ctx, Reducer(), streamID)
}
