package engine

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/gastownhall/gascity/internal/lumen/ir"
)

// TestDispatchMetadataFixtureLowers guards the hand-authored ITEM B dolt-e2e bundle
// fixture: it decodes and lowers under BOTH pool flag pairs, and the greet do leaf
// carries its static gc.continuation_group=main metadata through lowering — so a fixture
// typo (or a regression that drops the metadata off the leaf) fails fast HERE, not 300s
// into the sealed dolt e2e.
func TestDispatchMetadataFixtureLowers(t *testing.T) {
	path := filepath.Join("..", "..", "..", "examples", "lumen", "dispatch-metadata.lumen.json")
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	doc, err := ir.Decode(data)
	if err != nil {
		t.Fatalf("decode fixture: %v", err)
	}
	for _, combineDo := range []bool{true, false} {
		units, err := buildUnits(doc, true, combineDo)
		if err != nil {
			t.Fatalf("buildUnits(allowCombineDo=%v) refused the dispatch-metadata fixture: %v", combineDo, err)
		}
		greet := unitByNode(units, "greet")
		if greet == nil || greet.kind != unitLeaf {
			t.Fatalf("greet = %+v, want a leaf do unit", greet)
		}
		if got := greet.leaf.metadata["gc.continuation_group"]; got != "main" {
			t.Fatalf("greet leaf metadata gc.continuation_group = %q, want main (static metadata must survive lowering)", got)
		}
	}
}
