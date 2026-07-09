package engine_test

import (
	"context"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestListOpenRunsExcludesSealedAndFacadeRows (T-A4) pins the discovery contract:
// ListOpenRuns returns exactly the OPEN, fold-owned run roots — a sealed run
// (root left 'open' on run.closed) drops out, and a planted fold_owned=0 façade
// `bead_type='run'` row (the v2 control-frontier shape) is never returned.
func TestListOpenRunsExcludesSealedAndFacadeRows(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)

	// An OPEN run (do-only, parked).
	docJSON, _ := doOnlyDoc()
	openStream, err := engine.EnqueueRun(ctx, store, decodeIR(t, docJSON), nil, "ref", "workers")
	if err != nil {
		t.Fatalf("enqueue open run: %v", err)
	}

	// A SEALED run (engine-only, advanced to run.closed).
	sealDoc := decodeIR(t, blockDoc("seal", execNode("x", "echo x", nil)))
	sealStream, err := engine.EnqueueRun(ctx, store, sealDoc, nil, "ref", "")
	if err != nil {
		t.Fatalf("enqueue seal run: %v", err)
	}
	if res, err := engine.Advance(ctx, store, sealDoc, sealStream, nil, engine.Options{}); err != nil || !res.Sealed {
		t.Fatalf("advance seal run = %+v, err %v; want Sealed", res, err)
	}

	// A planted fold_owned=0 façade run row (allowed: the fold-owned guard fires
	// only on fold_owned=1). It must NOT be discovered.
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO nodes (id, bead_type, status, created_at, fold_owned, stream_id)
		 VALUES ('facade-run', 'run', 'open', '2026-07-08T00:00:00Z', 0, 'facade-stream')`); err != nil {
		t.Fatalf("plant façade row: %v", err)
	}

	runs, err := engine.ListOpenRuns(ctx, store)
	if err != nil {
		t.Fatalf("list open runs: %v", err)
	}
	byStream := map[string]string{} // stream_id -> root_id
	for _, r := range runs {
		byStream[r.StreamID] = r.RootID
	}

	if root, ok := byStream[openStream]; !ok {
		t.Fatalf("open run %q not discovered; got %+v", openStream, runs)
	} else if root != openStream {
		t.Fatalf("open run root_id %q != stream_id %q (a lumen run root is its stream)", root, openStream)
	}
	if _, ok := byStream[sealStream]; ok {
		t.Fatalf("sealed run %q was discovered as open", sealStream)
	}
	if _, ok := byStream["facade-stream"]; ok {
		t.Fatal("fold_owned=0 façade run row was discovered (should be excluded)")
	}
}
