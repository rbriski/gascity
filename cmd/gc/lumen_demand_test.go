package main

import (
	"context"
	"os"
	"path/filepath"
	"sort"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/lumen/engine"
	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// lumenTwoDoDoc is a formula with two independent (both ready) pool-mode do nodes.
func lumenTwoDoDoc(t *testing.T) *ir.IR {
	t.Helper()
	doc := `{
      "contract": {"name": "lumen.ir", "version": "0.2.5", "producer": "test"},
      "name": "fanout",
      "input": {"name": "main.input", "fields": [], "origin": {"uri": "t", "line": 0, "col": 0}},
      "origin": {"uri": "t", "line": 0, "col": 0},
      "nodes": [
        {"kind": "block", "id": "block_1", "after": [], "origin": {"uri": "t", "line": 1, "col": 0},
         "members": [
           {"kind": "do", "id": "a", "name": "a", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "source": {"kind": "prompt"},
            "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "Do A.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}},
           {"kind": "do", "id": "b", "name": "b", "after": [],
            "origin": {"uri": "t", "line": 1, "col": 0},
            "source": {"kind": "prompt"},
            "interpreter": {"kind": "agent", "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
            "body": {"raw": "Do B.", "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}}
         ]}
      ]
    }`
	d, err := ir.Decode([]byte(doc))
	if err != nil {
		t.Fatalf("decode two-do IR: %v", err)
	}
	return d
}

// lumenSeedTwoDoParked enqueues + ticks a two-do run to Parked (two claimable pool
// rows routed to tbHookRoute), returning its stream id.
func lumenSeedTwoDoParked(t *testing.T, cityPath string) string {
	t.Helper()
	streamID := lumenSeedRun(t, cityPath, lumenTwoDoDoc(t), nil, tbHookRoute)
	gs := tbHookOpenStore(t, cityPath)
	res, err := engine.Advance(context.Background(), gs, lumenTwoDoDoc(t), streamID, nil, engine.Options{PoolRouter: tbHookRouter})
	_ = gs.Close()
	if err != nil || !res.Parked || len(res.InFlight) != 2 {
		t.Fatalf("advance two-do = %+v, err %v; want Parked with 2 in flight", res, err)
	}
	return streamID
}

// TestLumenFrontierDemandCounts (T-D1) proves the native probe merge point: two
// ready pool rows routed to tbHookRoute produce count 2 with the projected bead ids
// and StoreRefs[id]=="graph-journal", and that demand yields a Tier:"new" pool
// request through ComputePoolDesiredStates.
func TestLumenFrontierDemandCounts(t *testing.T) {
	cityPath := tbHookGraphCity(t)
	lumenSeedTwoDoParked(t, cityPath)

	counts, demand, partials, err := lumenScaleCheckDemand(cityPath, []string{tbHookRoute})
	if err != nil {
		t.Fatalf("lumenScaleCheckDemand: %v", err)
	}
	if len(partials) != 0 {
		t.Fatalf("partials = %v, want none (healthy journal)", partials)
	}
	if counts[tbHookRoute] != 2 {
		t.Fatalf("count[%s] = %d, want 2", tbHookRoute, counts[tbHookRoute])
	}
	d := demand[tbHookRoute]
	ids := append([]string(nil), d.WorkBeadIDs...)
	sort.Strings(ids)
	if len(ids) != 2 || ids[0] != "a" || ids[1] != "b" {
		t.Fatalf("demand WorkBeadIDs = %v, want [a b]", ids)
	}
	for _, id := range []string{"a", "b"} {
		if d.StoreRefs[id] != tierBHookStoreName {
			t.Fatalf("StoreRefs[%s] = %q, want %q", id, d.StoreRefs[id], tierBHookStoreName)
		}
	}

	// The demand yields a Tier:"new" pool request (a cold pool spawns a session).
	cfg := &config.City{Agents: []config.Agent{poolAgent("claude", "rig", intPtr(2), 0)}}
	result := ComputePoolDesiredStates(cfg, nil, nil, counts)
	if len(result) != 1 || len(result[0].Requests) == 0 {
		t.Fatalf("desired states = %+v, want a request for the journal demand", result)
	}
	sawNew := false
	for _, req := range result[0].Requests {
		if req.Tier == "new" {
			sawNew = true
		}
	}
	if !sawNew {
		t.Fatalf("no Tier:\"new\" request from the journal-frontier demand: %+v", result[0].Requests)
	}
}

// TestLumenDemandExcludesClaimedRows (T-D2) proves a claim removes its row from the
// demand count (the frontier delete is the fold's; this pins the demand contract on
// top): claiming one of the two do rows drops the count to 1.
func TestLumenDemandExcludesClaimedRows(t *testing.T) {
	ctx := context.Background()
	cityPath := tbHookGraphCity(t)
	streamID := lumenSeedTwoDoParked(t, cityPath)

	gs := tbHookOpenStore(t, cityPath)
	if err := engine.ClaimTierBWork(ctx, gs, streamID, "a:0", "worker-a"); err != nil {
		_ = gs.Close()
		t.Fatalf("claim a: %v", err)
	}
	_ = gs.Close()

	counts, demand, _, err := lumenScaleCheckDemand(cityPath, []string{tbHookRoute})
	if err != nil {
		t.Fatalf("lumenScaleCheckDemand: %v", err)
	}
	if counts[tbHookRoute] != 1 {
		t.Fatalf("count[%s] = %d after claiming one, want 1", tbHookRoute, counts[tbHookRoute])
	}
	if len(demand[tbHookRoute].WorkBeadIDs) != 1 || demand[tbHookRoute].WorkBeadIDs[0] != "b" {
		t.Fatalf("demand ids = %v, want [b] (a was claimed)", demand[tbHookRoute].WorkBeadIDs)
	}
}

// TestLumenDemandUnopenableJournalMarksPartial (T-D3) proves an opted-but-unopenable
// journal marks the templates partial and surfaces the error — the count is NOT
// silently a 0-authoritative, so drain decisions stay suppressed.
func TestLumenDemandUnopenableJournalMarksPartial(t *testing.T) {
	cityPath := t.TempDir()
	graphBeads := filepath.Join(cityPath, ".gc", "graph", ".beads")
	if err := os.MkdirAll(graphBeads, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(graphBeads, "config.yaml"), []byte("backend: bogus\n"), 0o644); err != nil {
		t.Fatalf("write marker: %v", err)
	}

	counts, _, partials, err := lumenScaleCheckDemand(cityPath, []string{tbHookRoute})
	if err == nil {
		t.Fatal("lumenScaleCheckDemand did not surface the unopenable-journal error")
	}
	if !partials[tbHookRoute] {
		t.Fatalf("template %s not marked partial: %v", tbHookRoute, partials)
	}
	if _, ok := counts[tbHookRoute]; ok {
		t.Fatalf("count for %s is present (%d) — an unopenable journal must not assert an authoritative 0", tbHookRoute, counts[tbHookRoute])
	}
}
