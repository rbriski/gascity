package sling

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
)

// TestInstantiateSlingFormulaRoutesMoleculeByClass proves a sling routes a
// formula's molecule to the store its wholesale coordination class owns: a plain
// v1 formula (work-class molecule/step scaffolding) to the work/domain store, and
// a graph.v2 formula (infra-class) to the graph/infra store. On a split city
// (Store != GraphStore) this keeps work-class beads out of the infra store; before
// the fix, sling materialized EVERY molecule to GraphStore, stranding v1
// molecule/step beads in the infra store and violating the domain/infra boundary.
func TestInstantiateSlingFormulaRoutesMoleculeByClass(t *testing.T) {
	formulaDir := t.TempDir()
	writeSplitRoutingFile(t, filepath.Join(formulaDir, "v1work.toml"),
		"formula = \"v1work\"\nversion = 1\n\n[[steps]]\nid = \"work\"\ntitle = \"Work\"\n")
	writeSplitRoutingFile(t, filepath.Join(formulaDir, "v2work.toml"),
		"formula = \"v2work\"\nversion = 2\ncontract = \"graph.v2\"\n\n[[steps]]\nid = \"step\"\ntitle = \"Do work\"\n")
	cfg := graphV2SlingTestConfig(t, formulaDir)
	a := config.Agent{Name: "mayor", MaxActiveSessions: intPtr(1)}

	// Fresh stores per case: MemStore mints ids per-store (gc-1, gc-2, ...), so a
	// shared pair would collide the two molecules' ids across stores.
	run := func(t *testing.T, formulaName string, wantInfra bool) {
		work := beads.NewMemStore()
		infra := beads.NewMemStore()
		deps := testDeps(cfg, runtime.NewFake(), newFakeRunner().run)
		deps.Store = work
		deps.GraphStore = infra
		res, err := InstantiateSlingFormula(context.Background(), formulaName, []string{formulaDir}, molecule.Options{}, "", "default", "", a, deps)
		if err != nil {
			t.Fatalf("%s InstantiateSlingFormula: %v", formulaName, err)
		}
		want, other := work, infra
		if wantInfra {
			want, other = infra, work
		}
		assertMoleculeInStore(t, formulaName, res, want, other)
	}

	t.Run("v1 plain formula routes to the work store", func(t *testing.T) { run(t, "v1work", false) })
	t.Run("graph.v2 formula routes to the infra store", func(t *testing.T) { run(t, "v2work", true) })
}

func writeSplitRoutingFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// assertMoleculeInStore checks every bead the molecule created resolves in want
// and is absent from other, proving the whole molecule landed in one store.
func assertMoleculeInStore(t *testing.T, label string, res *molecule.Result, want, other beads.Store) {
	t.Helper()
	ids := map[string]bool{res.RootID: true}
	for _, id := range res.IDMapping {
		ids[id] = true
	}
	if len(ids) == 0 {
		t.Fatalf("%s: molecule created no beads", label)
	}
	for id := range ids {
		if id == "" {
			continue
		}
		if _, err := want.Get(id); err != nil {
			t.Errorf("%s: bead %q not in the expected store: %v", label, id, err)
		}
		if _, err := other.Get(id); err == nil {
			t.Errorf("%s: bead %q leaked into the other store", label, id)
		}
	}
}
