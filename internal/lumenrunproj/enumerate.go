package lumenrunproj

import (
	"context"
	"fmt"
	"strconv"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// maxHistoricalRuns caps the closed Lumen runs surfaced per summary, mirroring
// runproj's maxHistoricalLanes so a long-lived city does not fold an unbounded
// history every request.
const maxHistoricalRuns = 50

// runStreamIDs returns the stream ids of the run roots to surface: every open
// run plus the most-recent closed runs (capped). Only run-ROOT rows are read
// from Tier-A — their ids are nonce stream ids that never collide across runs,
// unlike step rows (whose IR-local ids are last-writer-wins). Topology comes
// from FoldRunView, not these rows.
func runStreamIDs(ctx context.Context, store *graphstore.Store) ([]string, error) {
	open, err := engine.ListOpenRuns(ctx, store)
	if err != nil {
		return nil, fmt.Errorf("lumenrunproj: list open runs: %w", err)
	}
	seen := make(map[string]bool, len(open))
	out := make([]string, 0, len(open))
	for _, r := range open {
		if r.StreamID == "" || seen[r.StreamID] {
			continue
		}
		seen[r.StreamID] = true
		out = append(out, r.StreamID)
	}

	closed, err := historicalRunStreamIDs(ctx, store)
	if err != nil {
		return nil, err
	}
	for _, id := range closed {
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		out = append(out, id)
	}
	return out, nil
}

// historicalRunStreamIDs reads the most-recent closed run-root stream ids.
// Placeholder-free static SQL (the maxHistoricalRuns cap is a compile-time
// constant, not user input), matching engine.ListOpenRuns' Postgres-shim rule.
func historicalRunStreamIDs(ctx context.Context, store *graphstore.Store) ([]string, error) {
	rows, err := store.ReadDB().QueryContext(ctx,
		`SELECT stream_id FROM nodes
		  WHERE bead_type = 'run' AND status != 'open' AND fold_owned = 1
		  ORDER BY created_at DESC
		  LIMIT `+strconv.Itoa(maxHistoricalRuns))
	if err != nil {
		return nil, fmt.Errorf("lumenrunproj: list historical runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("lumenrunproj: list historical runs: scan: %w", err)
		}
		out = append(out, id)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lumenrunproj: list historical runs: %w", err)
	}
	return out, nil
}
