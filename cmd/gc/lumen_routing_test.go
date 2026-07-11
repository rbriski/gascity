package main

import "testing"

// TestLumenPoolRouterRoutesByAgentRef proves the production Advance PoolRouter does
// agent-binding routing: a do that names an agent (its `with <agent>` binding →
// agentRef) routes to that agent's pool TEMPLATE — the agentRef IS the route, the same
// [agent] config-block name findAgentByTemplate resolves — and only a do with no
// agentRef falls back to the run default. This is what makes a multi-lane pack fan to
// distinct pools rather than one default.
func TestLumenPoolRouterRoutesByAgentRef(t *testing.T) {
	router := lumenPoolRouter("workers")

	if route, ok := router("codeReviewer"); !ok || route != "codeReviewer" {
		t.Errorf("router(codeReviewer) = (%q, %v), want (codeReviewer, true) — the named [agent] template", route, ok)
	}
	if route, ok := router("claudeReviewer"); !ok || route != "claudeReviewer" {
		t.Errorf("router(claudeReviewer) = (%q, %v), want (claudeReviewer, true) — a distinct lane", route, ok)
	}
	if route, ok := router(""); !ok || route != "workers" {
		t.Errorf("router(\"\") = (%q, %v), want (workers, true) — the run default", route, ok)
	}

	// An unbound do with an empty run default is a loud no-route (a pool-mode do with
	// nowhere to run) — ErrNoPoolRoute at materialize, never a silent misroute.
	if route, ok := lumenPoolRouter("")(""); ok || route != "" {
		t.Errorf("router(\"\") with empty default = (%q, %v), want (\"\", false)", route, ok)
	}
}
