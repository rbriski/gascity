package sourceworkflow

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// MemStore assigns its own ids and forces Status="open" on Create, so these
// helpers thread the returned ids and close beads explicitly.

func mustCreate(t *testing.T, store beads.Store, b beads.Bead) beads.Bead {
	t.Helper()
	created, err := store.Create(b)
	if err != nil {
		t.Fatalf("Create(%s): %v", b.Title, err)
	}
	return created
}

func mustClose(t *testing.T, store beads.Store, id string) {
	t.Helper()
	if err := store.Close(id); err != nil {
		t.Fatalf("Close(%s): %v", id, err)
	}
}

// newInputConvoy creates a synthetic one-item input convoy (as
// graphv2.CreateSingleItemInputConvoy does) tracking each sourceID, and returns
// the convoy id. The convoy, the tracks edges, and the source beads are
// co-resident in store, matching the default single-store deployment.
func newInputConvoy(t *testing.T, store beads.Store, sourceIDs ...string) string {
	t.Helper()
	convoy := mustCreate(t, store, beads.Bead{
		Title:    "input convoy",
		Type:     "convoy",
		Metadata: map[string]string{beadmeta.SyntheticMetadataKey: "true"},
	})
	for _, sourceID := range sourceIDs {
		if err := convoycore.TrackItem(store, convoy.ID, sourceID); err != nil {
			t.Fatalf("TrackItem(%s -> %s): %v", convoy.ID, sourceID, err)
		}
	}
	return convoy.ID
}

// newGraphRoot creates an open graph.v2 workflow root linked to convoyID.
func newGraphRoot(t *testing.T, store beads.Store, convoyID string) beads.Bead {
	t.Helper()
	return mustCreate(t, store, beads.Bead{
		Title: "workflow",
		Type:  "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:            beadmeta.KindWorkflow,
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
			beadmeta.InputConvoyIDMetadataKey:   convoyID,
		},
	})
}

func TestWorkflowSourceTerminalViaInputConvoyAllMembersClosed(t *testing.T) {
	store := beads.NewMemStore()
	src := mustCreate(t, store, beads.Bead{Title: "the work", Type: "task"})
	convoyID := newInputConvoy(t, store, src.ID)
	root := newGraphRoot(t, store, convoyID)

	// Source still open -> not terminal.
	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || terminal {
		t.Fatalf("WorkflowSourceTerminal(open source) = %v, %v; want false, nil", terminal, err)
	}

	// Close the source (as the refinery does after merge) -> terminal.
	mustClose(t, store, src.ID)
	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || !terminal {
		t.Fatalf("WorkflowSourceTerminal(closed source) = %v, %v; want true, nil", terminal, err)
	}
}

func TestWorkflowSourceTerminalMultiMemberRequiresAllClosed(t *testing.T) {
	store := beads.NewMemStore()
	srcA := mustCreate(t, store, beads.Bead{Title: "a", Type: "task"})
	srcB := mustCreate(t, store, beads.Bead{Title: "b", Type: "task"})
	convoyID := newInputConvoy(t, store, srcA.ID, srcB.ID)
	root := newGraphRoot(t, store, convoyID)

	mustClose(t, store, srcA.ID)
	// One member still open -> not terminal.
	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || terminal {
		t.Fatalf("WorkflowSourceTerminal(one open) = %v, %v; want false, nil", terminal, err)
	}
	mustClose(t, store, srcB.ID)
	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || !terminal {
		t.Fatalf("WorkflowSourceTerminal(all closed) = %v, %v; want true, nil", terminal, err)
	}
}

func TestWorkflowSourceTerminalViaSourceBeadID(t *testing.T) {
	store := beads.NewMemStore()
	src := mustCreate(t, store, beads.Bead{Title: "the work", Type: "task"})
	root := mustCreate(t, store, beads.Bead{
		Title: "workflow", Type: "task",
		Metadata: map[string]string{
			beadmeta.KindMetadataKey:         beadmeta.KindWorkflow,
			beadmeta.SourceBeadIDMetadataKey: src.ID,
		},
	})

	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || terminal {
		t.Fatalf("WorkflowSourceTerminal(open source_bead_id) = %v, %v; want false, nil", terminal, err)
	}
	mustClose(t, store, src.ID)
	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || !terminal {
		t.Fatalf("WorkflowSourceTerminal(closed source_bead_id) = %v, %v; want true, nil", terminal, err)
	}
}

func TestWorkflowSourceTerminalNoSourceLinkIsFalse(t *testing.T) {
	store := beads.NewMemStore()
	root := mustCreate(t, store, beads.Bead{
		Title: "workflow", Type: "task",
		Metadata: map[string]string{
			beadmeta.FormulaContractMetadataKey: beadmeta.FormulaContractGraphV2,
		},
	})
	// No source_bead_id and no input_convoy_id: cannot confirm terminality, so
	// the workflow must never be force-finalized.
	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || terminal {
		t.Fatalf("WorkflowSourceTerminal(no link) = %v, %v; want false, nil", terminal, err)
	}
}

func TestWorkflowSourceTerminalEmptyConvoyIsFalse(t *testing.T) {
	store := beads.NewMemStore()
	convoy := mustCreate(t, store, beads.Bead{Title: "convoy", Type: "convoy"})
	root := newGraphRoot(t, store, convoy.ID)
	// An input convoy with zero resolvable members is not proof the source is
	// done — treat as not-terminal.
	if terminal, err := WorkflowSourceTerminal(store, root); err != nil || terminal {
		t.Fatalf("WorkflowSourceTerminal(empty convoy) = %v, %v; want false, nil", terminal, err)
	}
}

func TestListLiveRootsByInputConvoyFromConvoyID(t *testing.T) {
	store := beads.NewMemStore()
	src := mustCreate(t, store, beads.Bead{Title: "the work", Type: "task"})
	convoyID := newInputConvoy(t, store, src.ID)
	root := newGraphRoot(t, store, convoyID)
	mustClose(t, store, src.ID)

	roots, err := ListLiveRootsByInputConvoy(store, convoyID)
	if err != nil {
		t.Fatalf("ListLiveRootsByInputConvoy(convoy id): %v", err)
	}
	if len(roots) != 1 || roots[0].ID != root.ID {
		t.Fatalf("ListLiveRootsByInputConvoy(%s) = %#v, want [%s]", convoyID, roots, root.ID)
	}
}

func TestListLiveRootsByInputConvoyFromWorkBeadID(t *testing.T) {
	store := beads.NewMemStore()
	src := mustCreate(t, store, beads.Bead{Title: "the work", Type: "task"})
	convoyID := newInputConvoy(t, store, src.ID)
	root := newGraphRoot(t, store, convoyID)
	mustClose(t, store, src.ID)

	// Passing the work-bead id must reach the root through its tracking convoy.
	roots, err := ListLiveRootsByInputConvoy(store, src.ID)
	if err != nil {
		t.Fatalf("ListLiveRootsByInputConvoy(work bead id): %v", err)
	}
	if len(roots) != 1 || roots[0].ID != root.ID {
		t.Fatalf("ListLiveRootsByInputConvoy(%s) = %#v, want [%s]", src.ID, roots, root.ID)
	}
}

func TestListLiveRootsByInputConvoyExcludesTerminalAndNonGraphV2(t *testing.T) {
	store := beads.NewMemStore()
	src := mustCreate(t, store, beads.Bead{Title: "the work", Type: "task"})
	convoyID := newInputConvoy(t, store, src.ID)
	mustClose(t, store, src.ID)

	// A closed root must not be returned.
	closedRoot := newGraphRoot(t, store, convoyID)
	mustClose(t, store, closedRoot.ID)
	// A non-graph.v2 bead sharing the input convoy must not be returned: this
	// discovery path exists for graph.v2 roots that clear gc.source_bead_id.
	mustCreate(t, store, beads.Bead{
		Title: "not a graph root", Type: "task",
		Metadata: map[string]string{
			beadmeta.InputConvoyIDMetadataKey: convoyID,
		},
	})

	roots, err := ListLiveRootsByInputConvoy(store, convoyID)
	if err != nil {
		t.Fatalf("ListLiveRootsByInputConvoy: %v", err)
	}
	if len(roots) != 0 {
		t.Fatalf("ListLiveRootsByInputConvoy(%s) = %#v, want none (terminal + non-graph.v2 excluded)", convoyID, roots)
	}
}

func TestListLiveRootsByInputConvoyDedupesConvoyAndWorkBeadPaths(t *testing.T) {
	store := beads.NewMemStore()
	src := mustCreate(t, store, beads.Bead{Title: "the work", Type: "task"})
	convoyID := newInputConvoy(t, store, src.ID)
	root := newGraphRoot(t, store, convoyID)
	mustClose(t, store, src.ID)

	// The convoy id resolves the root directly; the tracked source id resolves
	// it via the tracking convoy. Passing the convoy id, the same root must
	// appear once.
	roots, err := ListLiveRootsByInputConvoy(store, convoyID)
	if err != nil {
		t.Fatalf("ListLiveRootsByInputConvoy: %v", err)
	}
	if len(roots) != 1 || roots[0].ID != root.ID {
		t.Fatalf("ListLiveRootsByInputConvoy(%s) = %#v, want single [%s]", convoyID, roots, root.ID)
	}
}
