package beads

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore/fold"
	"github.com/gastownhall/gascity/internal/settlementfold"
)

// ProvenanceStream is one journal stream's contribution to a root's settlement
// provenance: the stream id plus that stream's own seq-ordered facts. A root can
// have two streams — settlement/<root> (the v1/v2 coarse settlements plus the v2
// control-epoch fence) and <root> (the lumen run stream) — and EACH carries an
// independent dense seq that starts at 1. The two seqs are therefore NOT
// cross-comparable, so provenance is returned as a list of per-stream groups
// rather than a single merged sequence.
type ProvenanceStream struct {
	// StreamID is the journal stream these facts were folded from.
	StreamID string
	// Facts are that stream's coarse settlement facts, in the stream's own seq
	// order (as ReadStream returns them).
	Facts []settlementfold.SettlementFact
}

// ProvenanceTimeline reads a root's unified settlement provenance (P5.4) as an
// ordered list of PER-STREAM groups — the single read surface where provenance
// across all three engines is observable. It is the reader half of the coarse
// events the dispatcher (v2) and the cmd-side v1 closers emit, plus the lumen
// run's own terminal facts.
//
// It folds two streams and returns each as its own group, deterministically
// ordered (settlement stream first, then the lumen run stream), omitting any
// absent/empty stream:
//
//   - settlement/<rootID> — the per-root settlement stream, which deliberately
//     carries interleaved v1/v2 (and the v2 control-epoch fence) rows; the fold
//     is mixed-engine (settlementfold.FoldEvents), so each fact keeps its own
//     event's engine tag. This is where a v1/v2 root's coarse settlements live.
//   - <rootID>            — for a lumen root, its journal run stream is keyed by
//     the run/root id (engine.go: RootID == StreamID). Folding it surfaces the
//     lumen terminal facts (outcome.settled / run.closed). For a v1/v2 root no
//     such stream exists, so it contributes no group.
//
// The two streams' seqs are NOT cross-comparable (each is an independent dense
// seq), and the store exposes no honest global order across them — the journal's
// appended_at is a wall clock and the retention gate makes rowid non-monotonic —
// so ProvenanceTimeline deliberately does NOT synthesize a merged global
// sequence. Callers render each group with its own per-stream sequence. store
// must expose the journal read capability (AppendLogStore.ReadStream) — a
// JournalStore does; a store without it (a non-journal city) is an error, since
// provenance only exists on the shared journal.
func ProvenanceTimeline(ctx context.Context, store Store, rootID string) ([]ProvenanceStream, error) {
	reader, ok := AppendLogStoreFor(store)
	if !ok {
		return nil, fmt.Errorf("provenance timeline for %s: store lacks journal read capability", rootID)
	}

	var streams []ProvenanceStream
	for _, streamID := range []string{SettlementStreamID(rootID), rootID} {
		facts, err := foldProvenanceStream(ctx, reader, streamID)
		if err != nil {
			return nil, err
		}
		if len(facts) > 0 {
			streams = append(streams, ProvenanceStream{StreamID: streamID, Facts: facts})
		}
	}
	return streams, nil
}

// foldProvenanceStream reads one journal stream and folds it into provenance
// facts. An absent stream reads as empty and folds to no facts.
func foldProvenanceStream(ctx context.Context, reader AppendLogStore, streamID string) ([]settlementfold.SettlementFact, error) {
	stored, err := reader.ReadStream(ctx, streamID, 1, 0)
	if err != nil {
		return nil, fmt.Errorf("provenance timeline: reading stream %s: %w", streamID, err)
	}
	if len(stored) == 0 {
		return nil, nil
	}
	events := make([]fold.Event, len(stored))
	for i, e := range stored {
		events[i] = fold.Event{
			StreamID:          e.StreamID,
			Seq:               e.Seq,
			Engine:            e.Engine,
			Substream:         e.Substream,
			Type:              e.Type,
			IRContractVersion: e.IRContractVersion,
			IdemToken:         e.IdemToken,
			Payload:           e.Payload,
		}
	}
	tl, err := settlementfold.FoldEvents(events)
	if err != nil {
		return nil, fmt.Errorf("provenance timeline: folding stream %s: %w", streamID, err)
	}
	return tl.Facts, nil
}
