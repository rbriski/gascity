package effectinventory

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverProfileUsesLexicalOrdinalsOriginsAndCanonicalOrder(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/advanced",
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	want := []observedKey{
		fixtureCall("boundary.emit", "advanced", "advanced.go", "DirectEmitTwice", nil, OperationCall, 1),
		fixtureCall("boundary.emit", "advanced", "advanced.go", "DirectEmitTwice", nil, OperationCall, 2),
		fixtureCall("boundary.emit", "advanced", "advanced.go", "GenericEmit", nil, OperationCall, 1),
		fixtureCall("boundary.emit", "advanced", "advanced.go", "NestedClosure", []int{1}, OperationCall, 1),
		fixtureCall("boundary.emit", "advanced", "advanced.go", "SourceWrapper", nil, OperationCall, 1),
	}

	boundaryOrders := [][]BoundaryDefinition{
		fixtureBoundaries(),
		reversedBoundaries(fixtureBoundaries()),
	}
	var baseline []ObservedSite
	for orderIndex, boundaries := range boundaryOrders {
		for repetition := 0; repetition < 3; repetition++ {
			observed, err := discoverProfile(context.Background(), config, profile, boundaries)
			if err != nil {
				t.Fatalf("discoverProfile(order=%d, repetition=%d) error: %v", orderIndex, repetition, err)
			}
			assertCanonicalObservedSites(t, observed, want)
			if baseline == nil {
				baseline = append([]ObservedSite(nil), observed...)
				continue
			}
			if !reflect.DeepEqual(observed, baseline) {
				t.Fatalf("discoverProfile(order=%d, repetition=%d) output changed\n got: %#v\nwant: %#v", orderIndex, repetition, observed, baseline)
			}
		}
	}
}

func TestDiscoverProfileDiagnosticsAreCanonicalAcrossBoundaryPermutations(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/advanced",
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}
	invalid := []BoundaryDefinition{
		{
			ID:     "missing-z",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: fixtureModulePath + "/boundary", Name: "MissingZ"},
			Match:  ObjectMatchExact,
		},
		{
			ID:     "missing-a",
			Kind:   KindProviderMutation,
			Object: ObjectRef{Package: fixtureModulePath + "/boundary", Name: "MissingA"},
			Match:  ObjectMatchExact,
		},
	}

	var baseline string
	for orderIndex, boundaries := range [][]BoundaryDefinition{invalid, reversedBoundaries(invalid)} {
		for repetition := 0; repetition < 2; repetition++ {
			_, err := discoverProfile(context.Background(), config, profile, boundaries)
			if err == nil {
				t.Fatalf("discoverProfile(order=%d, repetition=%d) error = nil, want unresolved-boundary diagnostics", orderIndex, repetition)
			}
			for _, name := range []string{"MissingA", "MissingZ"} {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("discoverProfile(order=%d, repetition=%d) error = %q, want %q", orderIndex, repetition, err, name)
				}
			}
			if baseline == "" {
				baseline = err.Error()
				continue
			}
			if err.Error() != baseline {
				t.Fatalf("discoverProfile(order=%d, repetition=%d) diagnostics changed\n got:\n%s\nwant:\n%s", orderIndex, repetition, err, baseline)
			}
		}
	}
}

func assertCanonicalObservedSites(t *testing.T, observed []ObservedSite, want []observedKey) {
	t.Helper()
	if len(observed) != len(want) {
		t.Fatalf("observed site count = %d, want %d\n got: %#v", len(observed), len(want), observed)
	}
	for index, site := range observed {
		if site.Profile != BuildLinuxDefault {
			t.Errorf("observed[%d] profile = %q, want %q: %+v", index, site.Profile, BuildLinuxDefault, site)
		}
		gotKey := observedKey{BoundaryID: site.BoundaryID, Matcher: site.Matcher}.key()
		wantKey := want[index].key()
		if gotKey != wantKey {
			t.Errorf("observed[%d] key = %q, want %q", index, gotKey, wantKey)
		}
	}
	for _, site := range observed {
		if site.Matcher.Enclosing.Object.Name == "CallsSourceWrapper" || site.Matcher.Enclosing.Object.Name == "InstantiateGeneric" {
			t.Errorf("source wrapper call was collapsed into boundary site: %+v", site)
		}
	}
}

func reversedBoundaries(input []BoundaryDefinition) []BoundaryDefinition {
	result := append([]BoundaryDefinition(nil), input...)
	for left, right := 0, len(result)-1; left < right; left, right = left+1, right-1 {
		result[left], result[right] = result[right], result[left]
	}
	return result
}
