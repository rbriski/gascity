package engine

// This file exposes a test-only seam for planting a genesis-era (pre-INS) journal to the external
// resume/rebuild harness (ins_input_drive_test.go, package engine_test). Because it is a _test.go
// file it compiles ONLY into the test binary — it adds NOTHING to the production API.

import (
	"context"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// PlantPreINSJournalForTest writes a genesis-era run.started for doc on streamID WITHOUT the ⚑B2
// required-unbound refusal — the on-disk shape an old (pre-INS) binary left for an in-flight run
// whose input never bound a now-required field. It stamps the RAW inputHash exactly as genesis
// does (an empty raw map ⇒ the unpinned "" hash), then leaves the journal at head 1 (run.started
// only, no units), so a later Resume/Advance re-enters through the rebuild path rather than a fresh
// genesis. It lets the external harness prove rebuildDriver never refuses a required-unbound input.
func PlantPreINSJournalForTest(t *testing.T, store *graphstore.Store, doc *ir.IR, streamID string, raw map[string]any) {
	t.Helper()
	ctx := context.Background()
	RegisterVocabulary(store)
	lease, err := store.AcquireWriterLease(ctx, streamID, leaseHolder, leaseTTL)
	if err != nil {
		t.Fatalf("plant: acquire writer lease %q: %v", streamID, err)
	}
	defer func() { _ = store.ReleaseWriterLease(ctx, lease) }()
	reducer := lumenReducer{}
	d := &driver{
		ctx:      ctx,
		store:    store,
		streamID: streamID,
		irVer:    doc.Contract.Version,
		epoch:    lease.Epoch,
		reducer:  reducer,
		state:    reducer.Zero(streamID),
	}
	if err := d.append(EventRunStarted, streamID+":run:started", runStartedPayload{
		RootID:    streamID,
		Name:      doc.Name,
		IRHash:    irHash(doc),
		InputHash: inputHash(raw),
		CreatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	}); err != nil {
		t.Fatalf("plant: append run.started: %v", err)
	}
}
