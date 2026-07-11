package sling

import (
	"testing"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/formula"
)

func TestRecipeMaterializesInfraClass(t *testing.T) {
	cases := []struct {
		name   string
		recipe *formula.Recipe
		want   bool
	}{
		{"nil recipe", nil, false},
		{
			name: "v1 plain molecule is work-class",
			recipe: &formula.Recipe{Name: "m", Steps: []formula.RecipeStep{
				{ID: "m", Title: "root", Type: "task", IsRoot: true},
				{ID: "m.step", Title: "step", Type: "task"},
			}},
			want: false,
		},
		{
			name: "graph.v2 workflow root is infra-class",
			recipe: &formula.Recipe{Name: "wf", Steps: []formula.RecipeStep{
				{ID: "wf", Title: "wf", Type: "task", IsRoot: true, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWorkflow}},
				{ID: "wf.step", Title: "step", Type: "task"},
			}},
			want: true,
		},
		{
			name: "wisp root is infra-class",
			recipe: &formula.Recipe{Name: "w", Steps: []formula.RecipeStep{
				{ID: "w", Title: "wisp", Type: "task", IsRoot: true, Metadata: map[string]string{beadmeta.KindMetadataKey: beadmeta.KindWisp}},
			}},
			want: true,
		},
		{
			name: "convergence root is infra-class",
			recipe: &formula.Recipe{Name: "c", Steps: []formula.RecipeStep{
				{ID: "c", Title: "conv", Type: "convergence", IsRoot: true},
			}},
			want: true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := recipeMaterializesInfraClass(tc.recipe); got != tc.want {
				t.Fatalf("recipeMaterializesInfraClass(%s) = %v, want %v", tc.name, got, tc.want)
			}
		})
	}
}
