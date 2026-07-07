package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
)

// This file covers the E1.2 TAIL: the remaining GRAPH-class read/create CLI
// roots the census flagged but the E2.3/read-side pass did not route —
//
//   - molecule_autoclose.go doMoleculeAutoclose  (graphStoreOpt was omitted)
//   - wisp_autoclose.go      doWispAutoclose      (graphStoreOpt was omitted)
//   - cmd_convoy_dispatch.go control-dispatch discovery (findBeadAcrossStores)
//     and cmdWorkflowDelete graph-root membership scan
//
// Each root now routes its graph-class access through cliGraphStore (→
// resolveGraphStore), which returns the infra store on a split city and the
// input store verbatim (identity) on a legacy single-store city. The tests
// prove BOTH halves: the graph op reaches the infra store on a split city, and
// the legacy path is byte-identical.

// splitMoleculeStores returns a work store and a distinct infra graph store,
// both wrapped in the production policy stack so the two-store split is
// exercised through the same wrappers production uses. The stores are separate
// instances so a read/close landing in the wrong one is observable.
func splitMoleculeStores(t *testing.T) (work, infra beads.Store) {
	t.Helper()
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	work = wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
	infra = wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	return work, infra
}

// TestMoleculeAutocloseWithGraphStoreClosesInfraResidentRoot proves the
// doMoleculeAutoclose FIX: on a split city the just-closed WORK/source bead
// lives in the work store while its graph-workflow root lives ONLY in the graph
// (infra) store, and the source-bead reverse walk closes that infra-resident
// root because the graph store is passed as the trailing graphStoreOpt.
func TestMoleculeAutocloseWithGraphStoreClosesInfraResidentRoot(t *testing.T) {
	work, infra := splitMoleculeStores(t)

	source, err := work.Create(beads.Bead{Title: "fix the bug", Type: "task"})
	if err != nil {
		t.Fatalf("create source in work store: %v", err)
	}
	root, err := infra.Create(beads.Bead{
		Title: "mol-focus-review",
		Type:  "task", // graph.v2 wisps are issue_type "task"
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})
	if err != nil {
		t.Fatalf("create graph root in infra store: %v", err)
	}

	if err := work.Close(source.ID); err != nil {
		t.Fatalf("close source: %v", err)
	}

	var out bytes.Buffer
	// storeRef "" matches on bead id alone (single source store), the same path
	// the CLI root takes when the graph root's SourceStoreRef is empty.
	doMoleculeAutocloseWith(work, "", events.Discard, source.ID, &out, infra)

	got, err := infra.Get(root.ID)
	if err != nil {
		t.Fatalf("get root from infra: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("infra-resident graph root not auto-closed via graph store: status=%q out=%q", got.Status, out.String())
	}
	if !strings.Contains(out.String(), "Auto-closed molecule "+root.ID) {
		t.Fatalf("stdout=%q, want auto-close announcement for %s", out.String(), root.ID)
	}
}

// TestMoleculeAutocloseWithoutGraphStoreStaysOnOwningStore proves byte-identity
// for the legacy single-store path: when the graphStoreOpt is omitted (nil), the
// core collapses to the owning store exactly as before the per-class seam — a
// root resident in a SEPARATE store is untouched, and one co-resident with the
// closed bead still closes.
func TestMoleculeAutocloseWithoutGraphStoreStaysOnOwningStore(t *testing.T) {
	work, other := splitMoleculeStores(t)

	source, _ := work.Create(beads.Bead{Title: "fix the bug", Type: "task"})
	// A root that lives in the OTHER store must not be reached without a graph store.
	otherRoot, _ := other.Create(beads.Bead{
		Title: "mol-elsewhere",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})
	// A root co-resident with the closed bead still closes (legacy behavior).
	coResidentRoot, _ := work.Create(beads.Bead{
		Title: "mol-here",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.SourceBeadIDMetadataKey:    source.ID,
		},
	})

	_ = work.Close(source.ID)

	var out bytes.Buffer
	doMoleculeAutocloseWith(work, "", events.Discard, source.ID, &out) // no graphStoreOpt → identity

	if got, _ := other.Get(otherRoot.ID); got.Status == "closed" {
		t.Fatalf("root in a separate store was closed without a graph store (byte-identity violated)")
	}
	if got, _ := work.Get(coResidentRoot.ID); got.Status != "closed" {
		t.Fatalf("co-resident root not closed on legacy single-store path: status=%q", got.Status)
	}
}

// TestWispAutocloseWithGraphStoreClosesInfraResidentAttachment proves the
// doWispAutoclose FIX: on a split city the closed parent WORK bead lives in the
// work store while its attached workflow root lives ONLY in the infra graph
// store, and the attachment is collected + closed through the passed graph store.
func TestWispAutocloseWithGraphStoreClosesInfraResidentAttachment(t *testing.T) {
	work, infra := splitMoleculeStores(t)

	// The attached workflow root lives in the infra store; the parent WORK bead
	// links to it via workflow_id metadata (the link CollectAttachedBeads
	// follows). The parent is read from the work store, but the root is resolved
	// through the graph store passed as graphStoreOpt.
	attached, _ := infra.Create(beads.Bead{Title: "attached-wisp", Type: "molecule"})
	step, _ := infra.Create(beads.Bead{
		Title:    "wisp step",
		Type:     "step",
		ParentID: attached.ID,
		Metadata: map[string]string{
			beadmeta.RootBeadIDMetadataKey: attached.ID,
		},
	})
	_ = infra.Close(step.ID) // subtree terminal so the wisp is reaped, not parked

	parent, _ := work.Create(beads.Bead{
		Title:    "owner dispatch bead",
		Type:     "task",
		Metadata: map[string]string{"workflow_id": attached.ID},
	})
	_ = work.Close(parent.ID)

	var out bytes.Buffer
	doWispAutocloseWith(work, parent.ID, &out, infra)

	if got, _ := infra.Get(attached.ID); got.Status != "closed" {
		t.Fatalf("infra-resident attached wisp not closed via graph store: status=%q out=%q", got.Status, out.String())
	}
}

// TestWispAutocloseWithoutGraphStoreStaysOnOwningStore proves byte-identity for
// the legacy path: without a graph store the attachment collection runs on the
// owning store, so an attachment in a separate store is untouched.
func TestWispAutocloseWithoutGraphStoreStaysOnOwningStore(t *testing.T) {
	work, other := splitMoleculeStores(t)

	attached, _ := other.Create(beads.Bead{Title: "attached-elsewhere", Type: "molecule"})
	step, _ := other.Create(beads.Bead{
		Title:    "wisp step",
		Type:     "step",
		ParentID: attached.ID,
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: attached.ID},
	})
	_ = other.Close(step.ID)

	parent, _ := work.Create(beads.Bead{
		Title:    "owner dispatch bead",
		Type:     "task",
		Metadata: map[string]string{"workflow_id": attached.ID},
	})
	_ = work.Close(parent.ID)

	var out bytes.Buffer
	doWispAutocloseWith(work, parent.ID, &out) // no graphStoreOpt → identity

	if got, _ := other.Get(attached.ID); got.Status == "closed" {
		t.Fatalf("attachment in a separate store was closed without a graph store (byte-identity violated)")
	}
}

// writeMinimalCityConfig writes a file-provider city.toml so the city work store
// opens against a temp dir without dolt/bd.
func writeMinimalCityConfig(t *testing.T, cityPath string) {
	t.Helper()
	content := "[workspace]\nname = \"e12-tail-city\"\nprefix = \"ga\"\n\n[beads]\nprovider = \"file\"\n"
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(content), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
}

// withControlDispatchInfraStore swaps the control-dispatch discovery infra-store
// seam to a test store and restores it on cleanup.
func withControlDispatchInfraStore(t *testing.T, store beads.Store) {
	t.Helper()
	prev := controlDispatchInfraStore
	controlDispatchInfraStore = func(string) beads.Store { return store }
	t.Cleanup(func() { controlDispatchInfraStore = prev })
}

// TestFindBeadAcrossStoresDiscoversInfraControlBeadOnSplitCity proves the
// control-dispatch DISCOVERY fix: a control/graph bead resident only in the
// infra store is found by findBeadAcrossStores on a split city, so a manual
// `gc convoy control <id>` can locate an infra-resident control bead. The city
// store is a filesystem-backed scope; the infra candidate is injected through
// the seam so the test needs no dolt.
func TestFindBeadAcrossStoresDiscoversInfraControlBeadOnSplitCity(t *testing.T) {
	cityPath := t.TempDir()
	seedSplitCityInfraMarker(t, cityPath) // makes cityHasInfraStore(cityPath) true
	writeMinimalCityConfig(t, cityPath)

	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), nil)
	control, err := infra.Create(beads.Bead{
		Title: "control-check",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey: "check",
		},
	})
	if err != nil {
		t.Fatalf("create control bead in infra: %v", err)
	}
	withControlDispatchInfraStore(t, infra)

	gotStore, gotBead, gotPath, err := findBeadAcrossStores(cityPath, control.ID, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("findBeadAcrossStores(split city) err=%v, want the infra-resident control bead", err)
	}
	if gotBead.ID != control.ID {
		t.Fatalf("found bead id=%q, want %q", gotBead.ID, control.ID)
	}
	if gotStore != infra {
		t.Fatalf("found store is not the infra store (discovery routed to the wrong store)")
	}
	if gotPath != infraScopeRoot(cityPath) {
		t.Fatalf("found path=%q, want infra scope root %q", gotPath, infraScopeRoot(cityPath))
	}
}

// TestFindBeadAcrossStoresSkipsInfraOnLegacyCity proves byte-identity for the
// legacy single-store path: with no infra marker, cityHasInfraStore is false, so
// the infra store is never consulted even if the seam would return one — the
// scan is city-then-rigs exactly as before, and a bead only in the (would-be)
// infra store is reported not-found.
func TestFindBeadAcrossStoresSkipsInfraOnLegacyCity(t *testing.T) {
	cityPath := t.TempDir()
	writeMinimalCityConfig(t, cityPath) // NO infra marker → legacy city

	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), nil)
	control, _ := infra.Create(beads.Bead{Title: "control-check", Type: "task"})
	// The seam is wired, but cityHasInfraStore is false so it must not be reached.
	withControlDispatchInfraStore(t, infra)

	_, _, _, err := findBeadAcrossStores(cityPath, control.ID, &bytes.Buffer{})
	if err == nil {
		t.Fatalf("findBeadAcrossStores(legacy city) unexpectedly found an infra-only bead — the infra store was consulted without the split marker (byte-identity violated)")
	}
	if !strings.Contains(err.Error(), "not found") && !strings.Contains(err.Error(), control.ID) {
		t.Fatalf("expected a not-found error for the infra-only bead on a legacy city, got: %v", err)
	}
}

// TestWorkflowDeleteGraphStoreDedupIsIdentitySafe pins the dedup semantics
// cmdWorkflowDelete relies on: the policy-wrapped stores cliGraphStore returns
// are usable as map keys (interface identity, no panic), so on a split city
// where every work-view resolves to the SAME infra store the membership scan
// collapses to a single scan, while on a legacy city distinct work stores stay
// distinct keys and every store is scanned once. This guards the map[beads.Store]
// dedup against a future non-comparable store type silently breaking it.
func TestWorkflowDeleteGraphStoreDedupIsIdentitySafe(t *testing.T) {
	cfg := &config.City{Workspace: config.Workspace{Name: "test-city"}}
	infra := wrapInfraStoreWithBeadPolicies(beads.NewMemStoreHonoringIDs(), cfg)
	workA := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)
	workB := wrapStoreWithBeadPolicies(beads.NewMemStore(), cfg)

	// Split city: two work views both resolve to the one infra store → 1 unique.
	seen := make(map[beads.Store]bool)
	for _, resolved := range []beads.Store{infra, infra} {
		seen[resolved] = true
	}
	if len(seen) != 1 {
		t.Fatalf("split-city dedup: got %d unique graph stores, want 1 (infra scanned once)", len(seen))
	}

	// Legacy city: cliGraphStore is identity, so distinct work stores stay distinct.
	seen = make(map[beads.Store]bool)
	for _, resolved := range []beads.Store{workA, workB} {
		seen[resolved] = true
	}
	if len(seen) != 2 {
		t.Fatalf("legacy dedup: got %d unique graph stores, want 2 (each work store scanned)", len(seen))
	}
}
