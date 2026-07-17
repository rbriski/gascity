package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	convoycore "github.com/gastownhall/gascity/internal/convoy"
)

// setupInputConvoyWorkflowCity writes a minimal file-backed city, creates a
// source work bead, a synthetic input convoy tracking it, a graph.v2 workflow
// root linked ONLY via gc.input_convoy_id (no gc.source_bead_id, as
// pool-routed roots are), and an open child of the root. It returns the store
// and the source, convoy, root, and child ids.
func setupInputConvoyWorkflowCity(t *testing.T) (cityDir, sourceID, convoyID, rootID, childID string) {
	t.Helper()
	cityDir = t.TempDir()
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace]\nname = \"test-city\"\n\n[daemon]\nformula_v2 = true\n"+testControlDispatcherAgentTOML("")), 0o644); err != nil {
		t.Fatalf("write city.toml: %v", err)
	}
	t.Setenv("GC_CITY", cityDir)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	prevCityFlag := cityFlag
	cityFlag = ""
	t.Cleanup(func() { cityFlag = prevCityFlag })

	store, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity: %v", err)
	}
	source, err := store.Create(beads.Bead{Title: "Source", Type: "task", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(source): %v", err)
	}
	convoy, err := store.Create(beads.Bead{Title: "input convoy", Type: "convoy", Status: "in_progress"})
	if err != nil {
		t.Fatalf("Create(convoy): %v", err)
	}
	if err := convoycore.TrackItem(store, convoy.ID, source.ID); err != nil {
		t.Fatalf("TrackItem: %v", err)
	}
	// Pool-routed graph.v2 root: linked to the work only through
	// gc.input_convoy_id; gc.source_bead_id is deliberately absent.
	root, err := store.Create(beads.Bead{
		Title:  "mol-polecat-work",
		Type:   "task",
		Status: "in_progress",
		Metadata: map[string]string{
			"gc.kind":             "workflow",
			"gc.formula_contract": "graph.v2",
			"gc.input_convoy_id":  convoy.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(root): %v", err)
	}
	child, err := store.Create(beads.Bead{
		Title:  "submit-and-exit",
		Type:   "task",
		Status: "open",
		Metadata: map[string]string{
			"gc.root_bead_id": root.ID,
		},
	})
	if err != nil {
		t.Fatalf("Create(child): %v", err)
	}
	return cityDir, source.ID, convoy.ID, root.ID, child.ID
}

func assertDeleteSourceClosedRootAndChild(t *testing.T, cityDir, rootID, childID string) {
	t.Helper()
	reloaded, err := openStoreAtForCity(cityDir, cityDir)
	if err != nil {
		t.Fatalf("openStoreAtForCity(reload): %v", err)
	}
	root, err := reloaded.Get(rootID)
	if err != nil {
		t.Fatalf("Get(root): %v", err)
	}
	if root.Status != "closed" {
		t.Fatalf("root status = %q, want closed (graph.v2 root must be found via gc.input_convoy_id)", root.Status)
	}
	child, err := reloaded.Get(childID)
	if err != nil {
		t.Fatalf("Get(child): %v", err)
	}
	if child.Status != "closed" {
		t.Fatalf("child status = %q, want closed", child.Status)
	}
}

// TestCmdWorkflowDeleteSourceFindsGraphV2RootByWorkBead regresses the ga-tum
// recovery gap: `gc workflow delete-source <work-bead>` must finalize a
// graph.v2 molecule reached only through its input convoy. Before the fix the
// source-bead-keyed discovery returned matched_roots=0, so a witness could not
// recover a stale pool-routed workflow at all.
func TestCmdWorkflowDeleteSourceFindsGraphV2RootByWorkBead(t *testing.T) {
	cityDir, sourceID, _, rootID, childID := setupInputConvoyWorkflowCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(sourceID, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Fatalf("stdout = %q, want cleaned result (root reached via input convoy)", stdout.String())
	}
	assertDeleteSourceClosedRootAndChild(t, cityDir, rootID, childID)
}

// TestCmdWorkflowDeleteSourceFindsGraphV2RootByInputConvoy regresses the other
// half: passing the input-convoy id directly must also finalize the molecule.
func TestCmdWorkflowDeleteSourceFindsGraphV2RootByInputConvoy(t *testing.T) {
	cityDir, _, convoyID, rootID, childID := setupInputConvoyWorkflowCity(t)

	var stdout, stderr bytes.Buffer
	if code := cmdWorkflowDeleteSource(convoyID, sourceWorkflowStoreSelector{}, true, false, &stdout, &stderr); code != 0 {
		t.Fatalf("cmdWorkflowDeleteSource = %d; stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "result=cleaned") {
		t.Fatalf("stdout = %q, want cleaned result (root reached via input convoy id)", stdout.String())
	}
	assertDeleteSourceClosedRootAndChild(t, cityDir, rootID, childID)
}
