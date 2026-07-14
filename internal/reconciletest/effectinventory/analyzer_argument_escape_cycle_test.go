package effectinventory

import (
	"context"
	"strings"
	"testing"
)

func TestDiscoverProfileFailsClosedForCyclicSliceProvenance(t *testing.T) {
	const packageName = "callescape/phicycle"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundaries := []BoundaryDefinition{
		{
			ID:     "callescape.phicycle.callback",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Effect"},
			Match:  ObjectMatchExact,
		},
		{
			ID:     "callescape.phicycle.possible-callback",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "PossibleEffect"},
			Match:  ObjectMatchExact,
		},
		{
			ID:     "callescape.phicycle.channel",
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Approved"},
			Match:  ObjectMatchChannel,
		},
		{
			ID:     "callescape.phicycle.ready",
			Kind:   KindWakeSource,
			Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Ready"},
			Match:  ObjectMatchChannel,
		},
	}

	reversed := append([]BoundaryDefinition(nil), boundaries...)
	for left, right := 0, len(reversed)-1; left < right; left, right = left+1, right-1 {
		reversed[left], reversed[right] = reversed[right], reversed[left]
	}
	var baseline string
	for repetition, definitions := range [][]BoundaryDefinition{boundaries, reversed, boundaries} {
		observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), definitions)
		if err == nil {
			t.Fatalf("discoverProfile(repetition=%d) error = nil, want escaped-boundary diagnostics", repetition)
		}
		if observed != nil {
			t.Fatalf("discoverProfile(repetition=%d) observed = %#v, want nil", repetition, observed)
		}
		for _, owner := range []string{"EscapedValues", "EscapedBoxedValues"} {
			diagnostic := diagnosticLine(err.Error(), owner)
			for _, boundaryID := range []string{
				"callescape.phicycle.callback",
				"callescape.phicycle.possible-callback",
				"callescape.phicycle.channel",
				"callescape.phicycle.ready",
			} {
				if !strings.Contains(diagnostic, boundaryID) {
					t.Errorf("discoverProfile(repetition=%d) diagnostic for %s = %q, want boundary %q", repetition, owner, diagnostic, boundaryID)
				}
			}
		}
		if baseline == "" {
			baseline = err.Error()
		} else if err.Error() != baseline {
			t.Fatalf("discoverProfile(repetition=%d) diagnostics changed\n got:\n%s\nwant:\n%s", repetition, err, baseline)
		}
	}
}
