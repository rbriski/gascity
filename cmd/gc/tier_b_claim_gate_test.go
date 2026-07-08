package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/graphstore"
	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// TestWorkerClaimsLumenWorkBeadViaGcHookAndGcBd is the B1/08 named exit gate,
// honestly re-scoped for L1: a worker claims, reads, and closes a Lumen-minted
// Tier-B pool work bead through the gc-native worker loop — `gc hook --claim` to
// claim and `gc bd` to close — end-to-end in one temp city, with the claim/settle
// events landing in the journal and the projection converging to a sealed run.
//
// Re-scope rationale (correction #5): the loop is `gc hook --claim` / `gc bd`, NOT
// raw bd. Item 4 of the retired TestWorkerClaimsLumenWorkBeadViaRealBd comment
// ("drive the loop with the real bd binary") is WRONG BY DESIGN: raw bd opens the
// city work store, which structurally cannot see `.gc/graph/journal.db`, so a raw
// `bd update --claim` / `bd close` could never claim or settle a journal bead. The
// journal-native worker loop routes claim through the Tier-B hook store (a journal
// owned.admitted append) and close through the gc bd Tier-B shim (an owned.settled
// append); the driver's Advance seals the run when it folds the settle.
//
// Driven by SCRIPTED settlement — the composition-root adapter under test — with no
// controller loop, no enqueue, and no pool demand (all L2).
func TestWorkerClaimsLumenWorkBeadViaGcHookAndGcBd(t *testing.T) {
	ctx := context.Background()
	cityPath := tbGateCity(t)

	// 1. Advance (PoolRouter) parks with a claimable pool row.
	{
		gs := tbHookOpenStore(t, cityPath)
		res, err := engine.Advance(ctx, gs, tbHookDoc(t), tbHookStream, nil, engine.Options{PoolRouter: tbHookRouter})
		if err != nil {
			t.Fatalf("advance 1: %v", err)
		}
		if !res.Parked {
			t.Fatalf("advance 1 = %+v, want Parked with a claimable row", res)
		}
		if err := gs.Close(); err != nil {
			t.Fatalf("close advance-1 store: %v", err)
		}
	}

	// 2. Claim through the gc hook --claim federation (in-process). The worker reads
	// the prompt FROM THE CLAIM JSON — no store read (no `bd show`) is needed.
	tbStore, ok := tierBHookStore(cityPath, []string{tbHookRoute}, []string{"worker-a"}, "worker-a")
	if !ok {
		t.Fatal("tier-b hook store not present for a graph-scoped city")
	}
	opts := hookClaimOptions{Assignee: "worker-a", RouteTargets: []string{tbHookRoute}, IdentityCandidates: []string{"worker-a"}, JSON: true}
	rawBdMustNotRun := func(string, string, []string) (string, error) {
		return "", fmt.Errorf("raw bd must not participate in the Lumen worker loop")
	}
	var claimOut, claimErr bytes.Buffer
	if code := claimHookWorkWithRunner("", cityPath, nil, []hookStore{tbStore}, opts, hookClaimOps{}, rawBdMustNotRun,
		func(string, error) {}, &claimOut, &claimErr); code != 0 {
		t.Fatalf("hook claim code = %d; stderr=%s", code, claimErr.String())
	}
	var claim hookClaimJSONResult
	if err := json.Unmarshal(bytes.TrimSpace(claimOut.Bytes()), &claim); err != nil {
		t.Fatalf("decode claim JSON: %v; raw=%s", err, claimOut.String())
	}
	if claim.Action != "work" || claim.BeadID != "hello" || claim.Route != tbHookRoute {
		t.Fatalf("claim JSON = %+v, want a work claim of hello routed to %s", claim, tbHookRoute)
	}
	if claim.Description != "Say hello." {
		t.Fatalf("claim JSON description = %q, want the rendered prompt readable from the JSON alone", claim.Description)
	}

	// 3. Close through `gc bd` (the Tier-B shim), NOT raw bd. doBd resolves the city,
	// then interceptTierBClose settles the pool bead before the (file-provider) bd
	// check is ever reached.
	var closeOut, closeErr bytes.Buffer
	if code := doBd([]string{"--city=" + cityPath, "update", claim.BeadID, "--set-metadata", "gc.outcome=pass", "--status", "closed"},
		&closeOut, &closeErr); code != 0 {
		t.Fatalf("gc bd close code = %d; stderr=%s", code, closeErr.String())
	}

	// 4. Re-Advance folds the settle and seals the run.
	gs := tbHookOpenStore(t, cityPath)
	defer func() { _ = gs.Close() }()
	sealed, err := engine.Advance(ctx, gs, tbHookDoc(t), tbHookStream, nil, engine.Options{PoolRouter: tbHookRouter})
	if err != nil {
		t.Fatalf("re-advance: %v", err)
	}
	if !sealed.Sealed || sealed.Parked {
		t.Fatalf("re-advance = %+v, want Sealed", sealed)
	}
	if sealed.Run.Outcome != engine.OutcomePass {
		t.Fatalf("run outcome = %q, want pass", sealed.Run.Outcome)
	}

	// 5. The journal is the full gc-native loop, in order.
	types := tbGateEventTypes(sealed.Run.Events)
	want := []string{
		engine.EventRunStarted, engine.EventNodeActivated,
		engine.EventOwnedAdmitted, engine.EventOwnedSettled, engine.EventRunClosed,
	}
	if !reflect.DeepEqual(types, want) {
		t.Fatalf("journal sequence = %v, want %v", types, want)
	}

	// 6. Drop+refold byte-identity (DET-T-17 extension over the claim/settle arms).
	before := tbGateProjectionSnapshot(t, gs)
	if err := gs.RebuildTierA(ctx, engine.Reducer(), tbHookStream); err != nil {
		t.Fatalf("RebuildTierA: %v", err)
	}
	after := tbGateProjectionSnapshot(t, gs)
	if before != after {
		t.Fatalf("drop+refold changed the projection:\n--- before ---\n%s\n--- after ---\n%s", before, after)
	}

	// 7. The hash chain verifies.
	if err := gs.Verify(ctx, tbHookStream); err != nil {
		t.Fatalf("Verify: %v", err)
	}
}

// tbGateCity builds a temp city that gc bd can load (city.toml + file store) AND
// that carries a journal graph scope, so the full gc-native loop runs against one
// city on disk.
func tbGateCity(t *testing.T) string {
	t.Helper()
	cityPath := t.TempDir()

	graphBeads := filepath.Join(cityPath, ".gc", "graph", ".beads")
	if err := os.MkdirAll(graphBeads, 0o755); err != nil {
		t.Fatalf("mkdir graph scope: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graphBeads, "config.yaml"), []byte("backend: sqlite\n"), 0o644); err != nil {
		t.Fatalf("write graph scope marker: %v", err)
	}

	cityToml := "[workspace]\nname = \"lumene2\"\n\n" +
		"[beads]\nprovider = \"file\"\n\n" +
		"[session]\nprovider = \"fake\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	if err := ensureScopedFileStoreLayout(cityPath); err != nil {
		t.Fatalf("ensure file store layout: %v", err)
	}
	if err := ensurePersistedScopeLocalFileStore(cityPath); err != nil {
		t.Fatalf("ensure city file store: %v", err)
	}
	return cityPath
}

func tbGateEventTypes(events []graphstore.StoredEvent) []string {
	out := make([]string, len(events))
	for i, e := range events {
		out[i] = e.Type
	}
	return out
}

// tbGateProjectionSnapshot renders a canonical dump of the stream's fold-owned
// Tier-A rows (nodes + metadata + frontier) for byte-equality across a refold.
func tbGateProjectionSnapshot(t *testing.T, gs *graphstore.Store) string {
	t.Helper()
	ctx := context.Background()
	var b strings.Builder
	nodeRows, err := gs.DB().QueryContext(ctx,
		`SELECT id, status, COALESCE(assignee,''), bead_type FROM nodes WHERE stream_id = ? AND fold_owned = 1 ORDER BY id`, tbHookStream)
	if err != nil {
		t.Fatalf("query nodes: %v", err)
	}
	defer func() { _ = nodeRows.Close() }()
	for nodeRows.Next() {
		var id, status, assignee, bt string
		if err := nodeRows.Scan(&id, &status, &assignee, &bt); err != nil {
			t.Fatalf("scan node: %v", err)
		}
		fmt.Fprintf(&b, "node %s status=%s assignee=%s type=%s\n", id, status, assignee, bt)
	}
	metaRows, err := gs.DB().QueryContext(ctx,
		`SELECT m.node_id, m.key, m.value FROM node_metadata m
		   JOIN nodes n ON n.id = m.node_id
		  WHERE n.stream_id = ? AND n.fold_owned = 1 ORDER BY m.node_id, m.key`, tbHookStream)
	if err != nil {
		t.Fatalf("query metadata: %v", err)
	}
	defer func() { _ = metaRows.Close() }()
	var metaLines []string
	for metaRows.Next() {
		var id, k, v string
		if err := metaRows.Scan(&id, &k, &v); err != nil {
			t.Fatalf("scan meta: %v", err)
		}
		metaLines = append(metaLines, fmt.Sprintf("meta %s %s=%s", id, k, v))
	}
	sort.Strings(metaLines)
	b.WriteString(strings.Join(metaLines, "\n"))
	b.WriteString("\n")
	frontierRows, err := gs.DB().QueryContext(ctx,
		`SELECT node_id FROM frontier WHERE root_id = ? ORDER BY node_id`, tbHookStream)
	if err != nil {
		t.Fatalf("query frontier: %v", err)
	}
	defer func() { _ = frontierRows.Close() }()
	for frontierRows.Next() {
		var id string
		if err := frontierRows.Scan(&id); err != nil {
			t.Fatalf("scan frontier: %v", err)
		}
		fmt.Fprintf(&b, "frontier %s\n", id)
	}
	return b.String()
}
