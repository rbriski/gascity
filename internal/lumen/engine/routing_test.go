package engine_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/engine"
)

// doNodeWithAgent renders a do node carrying a `with <agent>` binding
// (interpreter.agent.name = the agent/config-template name), the source of a do's
// agentRef and thus its per-lane route.
func doNodeWithAgent(id, prompt, agent string, after []string) string {
	afterJSON, _ := json.Marshal(after)
	promptJSON, _ := json.Marshal(prompt)
	return `{
      "kind": "do", "id": "` + id + `", "name": "` + id + `", "after": ` + string(afterJSON) + `,
      "origin": {"uri": "t", "line": 1, "col": 0},
      "source": {"kind": "prompt"},
      "interpreter": {"kind": "agent", "agent": {"kind": "ref", "name": "` + agent + `"}, "mode": {"kind": "do"}, "origin": {"uri": "t", "line": 1, "col": 0}},
      "body": {"raw": ` + string(promptJSON) + `, "language": "markdown", "source": {"kind": "inline"}, "origin": {"uri": "t", "line": 1, "col": 0}}
    }`
}

// TestAdvanceRoutesEachDoByAgentRef proves per-lane (agent-binding) routing: each do
// routes to ITS OWN agent's pool via its agentRef (the do's `with <agent>` binding →
// interpreter.agent.name → PoolRouter(agentRef) → the work bead's route). A multi-lane
// pack (e.g. a codex reviewer + a claude reviewer) therefore fans to distinct pools,
// not one default. The router here mirrors cmd/gc's lumenPoolRouter: the agentRef IS
// the route (the [agent] config-template name that findAgentByTemplate resolves).
func TestAdvanceRoutesEachDoByAgentRef(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	router := func(agentRef string) (string, bool) {
		if agentRef == "" {
			return "default-pool", true
		}
		return "pool-" + agentRef, true
	}
	doc := decodeIR(t, blockDoc("route",
		scatterNode("lanes", nil, "continue",
			doNodeWithAgent("laneA", "review A", "reviewerA", nil),
			doNodeWithAgent("laneB", "review B", "reviewerB", nil)),
	))
	res, err := engine.Advance(ctx, store, doc, "gcg-run-route", nil, fake.optsWith(router))
	if err != nil {
		t.Fatalf("advance: %v", err)
	}
	if res.Sealed || len(res.InFlight) != 2 {
		t.Fatalf("advance = %+v, want 2 in-flight lanes", res)
	}
	routes := map[string]string{}
	for _, w := range fake.dispatches {
		routes[w.NodeID] = w.Route
	}
	if routes["laneA"] != "pool-reviewerA" {
		t.Errorf("laneA route = %q, want pool-reviewerA (routed by its own agentRef)", routes["laneA"])
	}
	if routes["laneB"] != "pool-reviewerB" {
		t.Errorf("laneB route = %q, want pool-reviewerB (routed by its own agentRef)", routes["laneB"])
	}
}

// TestAdvanceDoWithoutAgentRefUsesDefault proves a do with no `with <agent>` binding
// routes to the run default (agentRef ""), so a single-lane formula still works.
func TestAdvanceDoWithoutAgentRefUsesDefault(t *testing.T) {
	ctx := context.Background()
	store := newStore(t)
	fake := newFakeWorkStore()
	router := func(agentRef string) (string, bool) {
		if agentRef == "" {
			return "default-pool", true
		}
		return "pool-" + agentRef, true
	}
	doc := decodeIR(t, blockDoc("routed", doNode("plain", "just do it", nil)))
	if _, err := engine.Advance(ctx, store, doc, "gcg-run-plain", nil, fake.optsWith(router)); err != nil {
		t.Fatalf("advance: %v", err)
	}
	if len(fake.dispatches) != 1 || fake.dispatches[0].Route != "default-pool" {
		t.Fatalf("dispatch route = %+v, want default-pool (no agentRef)", fake.dispatches)
	}
}
