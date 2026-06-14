package main

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/coordrouter"
)

// TestWrapWithCachingStoreInsertsRouter proves B1b's wiring: the controller's
// store construction now layers policy(Router(caching(backend))) — the Router is
// present between the policy wrapper and the cache.
func TestWrapWithCachingStoreInsertsRouter(t *testing.T) {
	policy := wrapStoreWithBeadPolicies(beads.NewMemStore(), nil) // policy(mem)
	wrapped := wrapWithCachingStore(nil, policy, nil, false)      // policy(Router(caching(mem)))
	if wrapped == nil {
		t.Fatal("wrapWithCachingStore returned nil")
	}
	base, _, ok := unwrapBeadPolicyStore(wrapped)
	if !ok {
		t.Fatal("expected the result to be policy-wrapped")
	}
	if _, isRouter := base.(*coordrouter.Router); !isRouter {
		t.Fatalf("expected a *coordrouter.Router inside the policy wrapper, got %T", base)
	}
}

// TestCloseBeadStoreHandlePeelsRouter proves closeBeadStoreHandle reaches the
// underlying CachingStore through the Router (so StopReconciler/CloseStore fire
// and no reconciler goroutine leaks).
func TestCloseBeadStoreHandlePeelsRouter(t *testing.T) {
	cs := beads.NewCachingStore(beads.NewMemStore(), nil)
	wrapped := wrapStoreWithBeadPolicies(coordrouter.New(cs), nil) // policy(Router(caching(mem)))
	if err := closeBeadStoreHandle(wrapped); err != nil {
		t.Fatalf("closeBeadStoreHandle(policy(Router(caching))): %v", err)
	}
}
