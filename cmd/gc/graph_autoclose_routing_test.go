package main

import (
	"bytes"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/events"
)

// federatingReadStore is a minimal by-id read federation over [work, graph],
// mirroring the single behavior of coordrouter.Router that the autoclose paths
// rely on TODAY (graph_store=sqlite): a Get/List must find a graph-class bead in
// the graph leg even though the just-closed parent lives in the work leg. It
// embeds the work store for every other (write) op so attachment-pointer reads
// resolve while the close itself is still routed by the autoclose core to the
// owning store. It exists only to model the live Router read surface in a unit
// test; it is NOT the production store.
type federatingReadStore struct {
	beads.Store // work store: writes + the work leg of reads
	graph       beads.Store
}

func (f federatingReadStore) Get(id string) (beads.Bead, error) {
	if b, err := f.Store.Get(id); err == nil {
		return b, nil
	}
	return f.graph.Get(id)
}

// TestWispAutocloseRoutesGraphAttachmentCloseToGraphStore proves the Phase GD-b
// wisp-autoclose routing: under a graph-relocated city a wisp/workflow attachment
// (ClassGraph, gcg- prefix, living in the dedicated graph store) is closed ON the
// graph store, not on the just-closed work parent's store. The Router is gone from
// this unit (federatingReadStore models only its read surface), so the close lands
// correctly purely because doWispAutocloseWith routes by owning store.
func TestWispAutocloseRoutesGraphAttachmentCloseToGraphStore(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	graph, ok := openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}

	// Graph wisp subtree in the graph store: root (gcg-) + a terminal step.
	root, err := graph.Create(beads.Bead{Title: "wisp root", Type: "molecule"})
	if err != nil {
		t.Fatalf("create graph root: %v", err)
	}
	step, err := graph.Create(beads.Bead{Title: "step", Type: "task", ParentID: root.ID})
	if err != nil {
		t.Fatalf("create graph step: %v", err)
	}
	if err := graph.Close(step.ID); err != nil {
		t.Fatalf("close graph step: %v", err)
	}

	// Work parent (gc-) pointing at the graph wisp root via molecule_id, then closed.
	parent, err := work.Create(beads.Bead{
		Title:    "owner work bead",
		Metadata: map[string]string{"molecule_id": root.ID},
	})
	if err != nil {
		t.Fatalf("create work parent: %v", err)
	}
	if err := work.Close(parent.ID); err != nil {
		t.Fatalf("close work parent: %v", err)
	}

	store := federatingReadStore{Store: work, graph: graph}

	var stdout bytes.Buffer
	doWispAutocloseWith(store, parent.ID, &stdout, graph)

	// The graph root must be closed in the GRAPH store.
	got, err := graph.Get(root.ID)
	if err != nil {
		t.Fatalf("graph root Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("graph root status = %q, want closed (close routed to graph store)", got.Status)
	}
	// And it must never have been written to the work store.
	if _, err := work.Get(root.ID); err == nil {
		t.Fatalf("graph root %s leaked onto the work store", root.ID)
	}
}

// TestWispAutocloseByteIdenticalAtGraphBD proves the byte-identical floor: with no
// distinct graph store (graph=bd → graphStore == store, the single work store) the
// wisp-autoclose close lands on that one store exactly as before. The graph-store
// argument is the same store, so autocloseStoreSet collapses to a single store and
// autocloseOwningStore is the identity.
func TestWispAutocloseByteIdenticalAtGraphBD(t *testing.T) {
	store := beads.NewMemStore()
	_, _ = store.Create(beads.Bead{Title: "work item"})                                // gc-1
	_, _ = store.Create(beads.Bead{Title: "wisp", Type: "molecule", ParentID: "gc-1"}) // gc-2
	_ = store.Close("gc-1")

	var stdout bytes.Buffer
	// Passing store as the graph store == the graph=bd default (graph == work).
	doWispAutocloseWith(store, "gc-1", &stdout, store)

	b, err := store.Get("gc-2")
	if err != nil {
		t.Fatal(err)
	}
	if b.Status != "closed" {
		t.Fatalf("wisp status = %q, want closed (single-store byte-identical close)", b.Status)
	}
}

// TestMoleculeAutocloseRoutesGraphWorkflowRootToGraphStore proves the Phase GD-b
// molecule-autoclose source-bead routing: when a graph.v2 workflow's source work
// bead is closed, the reverse-resolved workflow root (ClassGraph, in the graph
// store) is closed ON the graph store. ListLiveRoots + the subtree-terminal close
// run against the graph store argument, so the stepless-wisp finalize-gap is
// drained even with no Router federating the mutate.
func TestMoleculeAutocloseRoutesGraphWorkflowRootToGraphStore(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	graph, ok := openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}

	// Source work bead in the work store.
	src, err := work.Create(beads.Bead{Title: "work issue", Type: "task"})
	if err != nil {
		t.Fatalf("create work source: %v", err)
	}

	// Graph.v2 workflow root in the graph store, sourced from the work bead, plus a
	// terminal step so the subtree is complete.
	root, err := graph.Create(beads.Bead{
		Title: "graph.v2 workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.SourceBeadIDMetadataKey:    src.ID,
		},
	})
	if err != nil {
		t.Fatalf("create graph root: %v", err)
	}
	step, err := graph.Create(beads.Bead{
		Title:    "graph step",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	if err != nil {
		t.Fatalf("create graph step: %v", err)
	}
	if err := graph.Close(step.ID); err != nil {
		t.Fatalf("close graph step: %v", err)
	}

	_ = work.Close(src.ID)

	var stdout bytes.Buffer
	doMoleculeAutocloseWith(work, "", events.Discard, src.ID, &stdout, graph)

	got, err := graph.Get(root.ID)
	if err != nil {
		t.Fatalf("graph root Get: %v", err)
	}
	if got.Status != "closed" {
		t.Fatalf("graph workflow root status = %q, want closed (routed to graph store)", got.Status)
	}
	if _, err := work.Get(root.ID); err == nil {
		t.Fatalf("graph workflow root %s leaked onto the work store", root.ID)
	}
}

// TestMoleculeAutocloseByteIdenticalAtGraphBD proves the byte-identical floor for
// the molecule source-bead path: with graph=bd (graphStore == the single work
// store) the workflow root is closed on that one store exactly as before.
func TestMoleculeAutocloseByteIdenticalAtGraphBD(t *testing.T) {
	store := beads.NewMemStore()
	src, _ := store.Create(beads.Bead{Title: "work issue", Type: "task"})
	root, _ := store.Create(beads.Bead{
		Title: "graph.v2 workflow root",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            "workflow",
			beadmeta.FormulaContractMetadataKey: "graph.v2",
			beadmeta.SourceBeadIDMetadataKey:    src.ID,
		},
	})
	step, _ := store.Create(beads.Bead{
		Title:    "step",
		Type:     "task",
		Metadata: map[string]string{beadmeta.RootBeadIDMetadataKey: root.ID},
	})
	_ = store.Close(step.ID)
	_ = store.Close(src.ID)

	var stdout bytes.Buffer
	doMoleculeAutocloseWith(store, "", events.Discard, src.ID, &stdout, store)

	got, err := store.Get(root.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "closed" {
		t.Fatalf("workflow root status = %q, want closed (single-store byte-identical)", got.Status)
	}
}

// TestAutocloseOwningStoreRoutesByPrefixAndProbe pins the by-id routing helper the
// autoclose cores use: a gcg- graph bead resolves to the graph store (by id
// prefix), a gc- work bead resolves to the work store (by probe), and a
// single-store set (graph=bd) always returns the fallback — byte-identical.
func TestAutocloseOwningStoreRoutesByPrefixAndProbe(t *testing.T) {
	cityPath := t.TempDir()
	work := beads.NewMemStore()
	graph, ok := openGraphSQLiteStore(cityPath)
	if !ok {
		t.Fatal("openGraphSQLiteStore failed")
	}
	wb, _ := work.Create(beads.Bead{Title: "work"})
	gb, _ := graph.Create(beads.Bead{Title: "graph", Type: "molecule"})

	set := autocloseStoreSet(work, graph)
	if got := autocloseOwningStore(gb.ID, set, work); got != graph {
		t.Fatalf("graph bead %s routed to %v, want graph store", gb.ID, got)
	}
	if got := autocloseOwningStore(wb.ID, set, work); any(got) != any(work) {
		t.Fatalf("work bead %s routed to %v, want work store", wb.ID, got)
	}

	// graph=bd: the set is the single work store, so every id returns the fallback.
	single := autocloseStoreSet(work, work)
	if len(single) != 1 {
		t.Fatalf("graph=bd store set len = %d, want 1", len(single))
	}
	if got := autocloseOwningStore(wb.ID, single, work); any(got) != any(work) {
		t.Fatalf("graph=bd routing not identity: got %v", got)
	}
}

// TestWispGCRunsRootUniverseOnGraphAndMailOnMessagingStore proves the Phase GD-b
// wisp-GC split: the ClassGraph root universe (closed-root purge) collects from the
// store passed to runGC (the controller hands it the graph store), while the
// ClassMessaging read-message retention sweep targets the messaging store set via
// setMessagingStore — never the graph store. Without the split, mail retention
// would query the graph store and silently stop purging once graph relocates.
func TestWispGCRunsRootUniverseOnGraphAndMailOnMessagingStore(t *testing.T) {
	now := time.Now()
	// Graph store: a closed graph root past TTL (the root universe).
	graph := newGCStore([]beads.Bead{
		makeGCBead("mol-graph", now.Add(-2*time.Hour), "closed", "molecule"),
	})
	// Messaging store: an expired read message (the mail-retention universe).
	messaging := newGCStore([]beads.Bead{
		makeGCMessageWisp("read-old", now.Add(-2*time.Hour), map[string]string{mailReadMetadataKey: "true"}),
	})

	wg := newWispGC(5*time.Minute, time.Hour, time.Hour)
	m, ok := wg.(*memoryWispGC)
	if !ok {
		t.Fatal("newWispGC did not return *memoryWispGC")
	}
	m.setMessagingStore(messaging)

	purged, err := wg.runGC(graph, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged = %d, want 2 (1 graph root + 1 read message)", purged)
	}
	// The graph root was purged from the graph store, NOT the messaging store.
	assertDeletedIDs(t, graph.deletedIDs, "mol-graph")
	// The read message was purged from the messaging store, NOT the graph store.
	assertDeletedIDs(t, messaging.deletedIDs, "read-old")
}

// TestWispGCByteIdenticalWhenMessagingStoreUnset proves the byte-identical floor:
// with no messaging store set (the CLI/test path, and graph=bd where the resolved
// messaging store equals the passed store), both the root universe and the mail
// leg run on the single store passed to runGC — exactly the pre-split behavior.
func TestWispGCByteIdenticalWhenMessagingStoreUnset(t *testing.T) {
	now := time.Now()
	store := newGCStore([]beads.Bead{
		makeGCBead("mol-1", now.Add(-2*time.Hour), "closed", "molecule"),
		makeGCMessageWisp("read-old", now.Add(-2*time.Hour), map[string]string{mailReadMetadataKey: "true"}),
	})

	wg := newWispGC(5*time.Minute, time.Hour, time.Hour)
	purged, err := wg.runGC(store, now)
	if err != nil {
		t.Fatalf("runGC: %v", err)
	}
	if purged != 2 {
		t.Fatalf("purged = %d, want 2", purged)
	}
	assertDeletedIDs(t, store.deletedIDs, "mol-1", "read-old")
}
