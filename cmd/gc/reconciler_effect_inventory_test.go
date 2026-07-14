package main

import (
	"context"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/reconciletest/effectinventory"
)

// TestReconcilerEffectInventoryOnBoundHead is P0.1's focused inventory test. It
// mechanically re-derives the reconciler effect surface from the exact bound
// execution head with the canonical type-aware analyzer, then asserts the
// generated counts and the load-bearing classifications. The counts are
// produced by this Go/types query, never hand-maintained prose; any new or
// removed production effect site changes them and fails the gate until the
// inventory is regenerated and re-reviewed.
//
// Scope is the plan's four reconciler packages (cmd/gc, internal/session,
// internal/worker, internal/runtime). The full five-profile golden drift gate
// lives in internal/reconciletest/effectinventory; this test pins the primary
// linux/default profile so the assertion stays fast inside the cmd/gc suite.
func TestReconcilerEffectInventoryOnBoundHead(t *testing.T) {
	if testing.Short() {
		t.Skip("effect inventory loads all reconciler packages; skipped in -short")
	}

	wd, err := os.Getwd()
	if err != nil {
		t.Fatalf("Getwd: %v", err)
	}
	repoRoot, modulePath, err := effectinventory.RepoRootFromDir(wd)
	if err != nil {
		t.Fatalf("locating repo root from %q: %v", wd, err)
	}

	linuxDefault := effectinventory.Profile{
		ID:     effectinventory.BuildLinuxDefault,
		GOOS:   "linux",
		GOARCH: "amd64",
	}
	sites, err := effectinventory.Discover(context.Background(), effectinventory.DiscoverConfig{
		RepoRoot:   repoRoot,
		ModulePath: modulePath,
		Profiles:   []effectinventory.Profile{linuxDefault},
		Boundaries: effectinventory.CanonicalBoundaries(),
	})
	if err != nil {
		t.Fatalf("Discover: %v", err)
	}

	// Generated counts. The type-aware analyzer resolves each call's receiver
	// through go/types, so unrelated Stop/Nudge/Close method names on other
	// types never become false effects. Update these only by regenerating the
	// golden and re-reviewing the diff.
	const (
		wantTotal    = 476
		wantStore    = 128
		wantProvider = 232
		wantEvent    = 84
		wantProcess  = 32
	)
	byKind := map[effectinventory.EffectKind]int{}
	for _, s := range sites {
		byKind[s.Kind]++
	}
	if len(sites) != wantTotal {
		t.Errorf("total effect sites = %d, want %d (regenerate the golden and review if the effect surface changed)", len(sites), wantTotal)
	}
	for kind, want := range map[effectinventory.EffectKind]int{
		effectinventory.KindStoreMutation:    wantStore,
		effectinventory.KindProviderMutation: wantProvider,
		effectinventory.KindEventEmission:    wantEvent,
		effectinventory.KindProcessMutation:  wantProcess,
	} {
		if got := byKind[kind]; got != want {
			t.Errorf("%s sites = %d, want %d", kind, got, want)
		}
	}

	// Load-bearing classifications the plan requires by name.

	// Route recovery's live-reread / non-CAS residual store write.
	if n := countSites(sites, func(s effectinventory.DiscoveredSite) bool {
		return s.BoundaryID == "store.SetMetadata" &&
			s.Matcher.Enclosing.Object.Name == "restoreCarriedWorkRoutes"
	}); n != 1 {
		t.Errorf("route-recovery store write sites = %d, want 1", n)
	}

	// Direct raw-store bypasses through the external bd store.
	if n := countSites(sites, func(s effectinventory.DiscoveredSite) bool {
		return s.Kind == effectinventory.KindStoreMutation && !s.ViaInterface &&
			s.ReceiverType == "*beads.BdStore"
	}); n != 11 {
		t.Errorf("raw *beads.BdStore bypass sites = %d, want 11", n)
	}

	// Provider-internal kill/signal sites must participate in the inventory
	// (their absence is a forbidden static-guard condition, ACCEPTANCE_MATRIX).
	if n := countSites(sites, func(s effectinventory.DiscoveredSite) bool {
		return s.Kind == effectinventory.KindProcessMutation &&
			strings.Contains(s.Package, "/internal/runtime/")
	}); n == 0 {
		t.Error("no provider-internal process (kill/signal) sites in the inventory")
	}

	// The curated ownership registry validates and reconciles with reality:
	// every classified site must resolve to an actually discovered site on the
	// head, so a stale classification cannot silently outlive its code.
	asOf := time.Date(2026, time.July, 14, 0, 0, 0, 0, time.UTC)
	registry := effectinventory.Inventory()
	if err := effectinventory.ValidateRegistry(registry, asOf); err != nil {
		t.Fatalf("ValidateRegistry(Inventory()): %v", err)
	}
	for _, site := range registry.Sites {
		if !containsSite(sites, site.BoundaryID, site.Matcher) {
			t.Errorf("classified site %q (%s in %s) is not present on the execution head",
				site.ID, site.BoundaryID, site.Matcher.Enclosing.Object.Name)
		}
	}
}

func countSites(sites []effectinventory.DiscoveredSite, pred func(effectinventory.DiscoveredSite) bool) int {
	n := 0
	for _, s := range sites {
		if pred(s) {
			n++
		}
	}
	return n
}

func containsSite(sites []effectinventory.DiscoveredSite, boundaryID string, matcher effectinventory.OperationSite) bool {
	for _, s := range sites {
		if s.BoundaryID == boundaryID && matcherEqual(s.Matcher, matcher) {
			return true
		}
	}
	return false
}

// matcherEqual compares two operation-site keys. OperationSite is not directly
// comparable because FunctionRef carries a ClosurePath slice.
func matcherEqual(a, b effectinventory.OperationSite) bool {
	return a.Operation == b.Operation &&
		a.Ordinal == b.Ordinal &&
		a.Enclosing.Object == b.Enclosing.Object &&
		a.Enclosing.File == b.Enclosing.File &&
		slices.Equal(a.Enclosing.ClosurePath, b.Enclosing.ClosurePath)
}
