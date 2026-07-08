package beads_test

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// These tests exercise the Tier-B claim-surface read capability against a REAL
// journal seeded by engine.Advance(PoolRouter) — the same projection a live pool
// claim would read. The beads layer cannot import engine (layering), so the seed
// runs from this external test package, which may import both.

const (
	tbRoute  = "pool-reviewers"
	tbStream = "gcg-run-tierb-cap"
)

func tbRouter(string) (string, bool) { return tbRoute, true }

func tbNewStore(t *testing.T) *graphstore.Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "graph.db")
	store, err := graphstore.Open(context.Background(), path, graphstore.Options{CityID: "test-city"})
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

// tbDoOnlyDoc is a single pool-mode do node "hello" with prompt "Say hello.".
func tbDoOnlyDoc(t *testing.T) *ir.IR {
	t.Helper()
	const doc = `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "greet",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [
           {"kind": "do", "id": "hello", "name": "hello", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "source": {"kind": "prompt"},
            "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "Say hello.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}}
         ]}
      ]
    }`
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode IR: %v", err)
	}
	return d
}

// tbParkPoolRow seeds a parked pool-mode run and returns the wrapped store and
// its Tier-B claim surface.
func tbParkPoolRow(t *testing.T) (*graphstore.Store, beads.TierBClaimSurfaceStore) {
	t.Helper()
	ctx := context.Background()
	store := tbNewStore(t)
	res, err := engine.Advance(ctx, store, tbDoOnlyDoc(t), tbStream, nil, engine.Options{PoolRouter: tbRouter})
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if !res.Parked {
		t.Fatalf("advance = %+v, want Parked", res)
	}
	js := beads.NewJournalStore(store)
	surface, ok := beads.TierBClaimSurfaceStoreFor(js)
	if !ok {
		t.Fatal("journal store does not expose the Tier-B claim surface")
	}
	return store, surface
}

func TestTierBRoutedFrontierHydratesClaimContract(t *testing.T) {
	ctx := context.Background()
	store, surface := tbParkPoolRow(t)

	rows, err := surface.TierBRoutedFrontier(ctx, []string{tbRoute}, 0)
	if err != nil {
		t.Fatalf("TierBRoutedFrontier: %v", err)
	}
	if len(rows) != 1 {
		t.Fatalf("routed frontier rows = %d, want exactly 1 (the ready pool row)", len(rows))
	}
	b := rows[0]
	if b.ID != "hello" {
		t.Fatalf("row id = %q, want hello", b.ID)
	}
	if b.Type != "task" {
		t.Fatalf("type = %q, want task", b.Type)
	}
	if b.Assignee != "" {
		t.Fatalf("assignee = %q, want empty (unclaimed)", b.Assignee)
	}
	if b.Metadata[beadmeta.RoutedToMetadataKey] != tbRoute {
		t.Fatalf("gc.routed_to = %q, want %q", b.Metadata[beadmeta.RoutedToMetadataKey], tbRoute)
	}
	if b.Metadata["activation"] == "" {
		t.Fatalf("activation metadata missing on an unsettled pool row: %+v", b.Metadata)
	}
	if b.Description != "Say hello." {
		t.Fatalf("description = %q, want the rendered prompt", b.Description)
	}
	if b.IsBlocked != nil && *b.IsBlocked {
		t.Fatalf("IsBlocked = true, want false (fold after/member edges are not blocking)")
	}

	// The run root (route "") and any engine-driven nodes never surface: querying
	// only the pool route yields only the pool row.
	for _, r := range rows {
		if r.ID == tbStream {
			t.Fatalf("routed frontier surfaced the run root %q (route empty)", tbStream)
		}
	}

	// After a claim the frontier delete removes the row: it is no longer routed-ready.
	if err := engine.ClaimTierBWork(ctx, store, tbStream, "hello:0", "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}
	after, err := surface.TierBRoutedFrontier(ctx, []string{tbRoute}, 0)
	if err != nil {
		t.Fatalf("TierBRoutedFrontier after claim: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("routed frontier after claim = %d rows, want 0 (claimed row leaves the frontier)", len(after))
	}
}

func TestTierBAssignedReturnsClaimedRows(t *testing.T) {
	ctx := context.Background()
	store, surface := tbParkPoolRow(t)

	q := func(assignee string) beads.TierBAssignedQuery {
		return beads.TierBAssignedQuery{
			Assignees:   []string{assignee},
			MarkerKey:   engine.DispatchModeMetaKey,
			MarkerValue: engine.DispatchModePool,
		}
	}

	// Before a claim: the open row has no assignee, so an assignee query is empty.
	if rows, err := surface.TierBAssigned(ctx, q("worker-a")); err != nil || len(rows) != 0 {
		t.Fatalf("assigned before claim = %d rows, err %v; want 0", len(rows), err)
	}

	if err := engine.ClaimTierBWork(ctx, store, tbStream, "hello:0", "worker-a"); err != nil {
		t.Fatalf("claim: %v", err)
	}

	rows, err := surface.TierBAssigned(ctx, q("worker-a"))
	if err != nil {
		t.Fatalf("TierBAssigned: %v", err)
	}
	if len(rows) != 1 || rows[0].ID != "hello" {
		t.Fatalf("assigned = %v, want the in_progress hello row", idsOf(rows))
	}
	if rows[0].Status != engine.StatusClaimed {
		t.Fatalf("assigned row status = %q, want in_progress", rows[0].Status)
	}
	if rows[0].Metadata[beadmeta.RoutedToMetadataKey] != tbRoute {
		t.Fatalf("assigned row gc.routed_to = %q, want %q (retained on claim)", rows[0].Metadata[beadmeta.RoutedToMetadataKey], tbRoute)
	}

	// A different assignee sees nothing.
	if rows, err := surface.TierBAssigned(ctx, q("worker-b")); err != nil || len(rows) != 0 {
		t.Fatalf("assigned for other worker = %d rows, err %v; want 0", len(rows), err)
	}

	// After settle the row leaves the (open,in_progress) window.
	if err := engine.SettleTierBWork(ctx, store, tbStream, "hello:0", engine.OutcomePass, "hi"); err != nil {
		t.Fatalf("settle: %v", err)
	}
	if rows, err := surface.TierBAssigned(ctx, q("worker-a")); err != nil || len(rows) != 0 {
		t.Fatalf("assigned after settle = %d rows, err %v; want 0", len(rows), err)
	}
}

func TestFoldOwnedGetExactID(t *testing.T) {
	ctx := context.Background()
	store, surface := tbParkPoolRow(t)

	got, ok, err := surface.FoldOwnedGet(ctx, "hello")
	if err != nil {
		t.Fatalf("FoldOwnedGet(hello): %v", err)
	}
	if !ok || got.ID != "hello" {
		t.Fatalf("FoldOwnedGet(hello) = (%+v, %v), want the fold row", got, ok)
	}

	// A façade (fold_owned=0) row with a distinct id is not a fold-owned row.
	if _, err := store.DB().ExecContext(ctx,
		`INSERT INTO nodes (id, status, bead_type, created_at, fold_owned) VALUES ('facade-1', 'open', 'task', '2026-07-08T00:00:00Z', 0)`,
	); err != nil {
		t.Fatalf("insert façade row: %v", err)
	}
	if _, ok, err := surface.FoldOwnedGet(ctx, "facade-1"); err != nil || ok {
		t.Fatalf("FoldOwnedGet(facade-1) ok=%v err=%v, want not found (fold-owned only)", ok, err)
	}

	// An absent id.
	if _, ok, err := surface.FoldOwnedGet(ctx, "nope"); err != nil || ok {
		t.Fatalf("FoldOwnedGet(nope) ok=%v err=%v, want not found", ok, err)
	}
}

func idsOf(beadsIn []beads.Bead) []string {
	out := make([]string, len(beadsIn))
	for i, b := range beadsIn {
		out[i] = b.ID
	}
	return out
}
