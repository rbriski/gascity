package engine

import (
	"context"
	"fmt"

	"github.com/gastownhall/gascity/internal/graphstore"
)

// OpenRun is one discovered open run: its root node id and its journal stream id.
// For a Lumen run the two are equal (applyRunStarted projects the root with
// id == stream_id), but both are surfaced so a caller reads them without assuming
// the identity.
type OpenRun struct {
	RootID   string
	StreamID string
}

// ListOpenRuns discovers every open, fold-owned run root from live projected
// state — the controller loop's per-tick discovery query (no run-registry file).
// A run root is projected open (bead_type='run', status='open') from run.started
// until run.closed folds it to its terminal status, so a sealed run drops out of
// the result on its own. The fold_owned=1 filter excludes any v2-side façade
// `run` row (which is fold_owned=0), so the loop can never pick up a control-bead
// projection. It reads through ReadDB with placeholder-free static SQL, following
// the readTierBNode idiom so the Postgres dialect shim covers it unchanged.
func ListOpenRuns(ctx context.Context, store *graphstore.Store) ([]OpenRun, error) {
	if store == nil {
		return nil, fmt.Errorf("lumen: list open runs: nil store")
	}
	rows, err := store.ReadDB().QueryContext(ctx,
		`SELECT id, stream_id FROM nodes
		  WHERE bead_type = 'run' AND status = 'open' AND fold_owned = 1
		  ORDER BY id`)
	if err != nil {
		return nil, fmt.Errorf("lumen: list open runs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []OpenRun
	for rows.Next() {
		var r OpenRun
		if err := rows.Scan(&r.RootID, &r.StreamID); err != nil {
			return nil, fmt.Errorf("lumen: list open runs: scan: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("lumen: list open runs: %w", err)
	}
	return out, nil
}
