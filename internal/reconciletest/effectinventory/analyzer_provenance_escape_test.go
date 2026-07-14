package effectinventory

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

func TestDiscoverProfileRejectsAddressAndWholeStructEscapesDeterministically(t *testing.T) {
	tests := []struct {
		name        string
		packageName string
		functions   []string
	}{
		{
			name:        "addresses passed to unknown callbacks",
			packageName: "provescape/addresscallback",
			functions:   []string{"LocalChannelAddress", "StructFieldAddress"},
		},
		{
			name:        "whole struct stored through external pointer",
			packageName: "provescape/structstore",
			functions:   []string{"WholeStructExternalStore"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := fixtureAnalysisConfig(t, []string{
				"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + tt.packageName,
			})
			boundary := provenanceEscapeGlobalBoundary(tt.packageName)

			var baseline string
			for repetition := 0; repetition < 3; repetition++ {
				observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
				if err == nil {
					t.Fatalf("discoverProfile(repetition=%d) error = nil, want open-world escape diagnostic", repetition)
				}
				if observed != nil {
					t.Fatalf("discoverProfile(repetition=%d) observed = %#v, want nil", repetition, observed)
				}
				for _, fragment := range append(tt.functions, "open-world provenance") {
					if !strings.Contains(err.Error(), fragment) {
						t.Errorf("discoverProfile(repetition=%d) error = %q, want %q", repetition, err, fragment)
					}
				}
				if baseline == "" {
					baseline = err.Error()
				} else if err.Error() != baseline {
					t.Fatalf("discoverProfile(repetition=%d) diagnostics changed\n got:\n%s\nwant:\n%s", repetition, err, baseline)
				}
			}
		})
	}
}

func TestDiscoverProfileKeepsDynamicFactoryParametersOpenWorld(t *testing.T) {
	const packageName = "provescape/factory"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := provenanceEscapeGlobalBoundary(packageName)

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want open-world factory diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, fragment := range []string{"DynamicFactory", "open-world provenance"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("discoverProfile() error = %q, want %q", err, fragment)
		}
	}
}

func TestDiscoverProfileRejectsUnsafeDerivedExactBoundaryField(t *testing.T) {
	const packageName = "provescape/unsafehub"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:   "provescape.unsafehub.wake",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package:  fixtureModulePath + "/" + packageName,
			Receiver: "Hub",
			Name:     "Wake",
		},
		Match: ObjectMatchChannel,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err == nil {
		t.Fatal("discoverProfile() error = nil, want unsafe-provenance diagnostic")
	}
	if observed != nil {
		t.Fatalf("discoverProfile() observed = %#v, want nil", observed)
	}
	for _, fragment := range []string{"UnsafeDerivedField", "unsafe channel provenance"} {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("discoverProfile() error = %q, want %q", err, fragment)
		}
	}
}

func TestDiscoverProfileAcceptsClosedLexicalAndRecursiveProvenanceDeterministically(t *testing.T) {
	const packageName = "provescape/closed"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := provenanceEscapeGlobalBoundary(packageName)
	want := []observedKey{
		fixtureCall(boundary.ID, packageName, "closed.go", "LexicalClosure", []int{1}, OperationChannelSend, 1),
		fixtureCall(boundary.ID, packageName, "closed.go", "StaticRecursivePassthrough", nil, OperationChannelSend, 1),
	}

	var baseline []ObservedSite
	for repetition := 0; repetition < 3; repetition++ {
		observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
		if err != nil {
			t.Fatalf("discoverProfile(repetition=%d) error: %v", repetition, err)
		}
		assertObservedSites(t, observed, want)
		if baseline == nil {
			baseline = append([]ObservedSite(nil), observed...)
		} else if !reflect.DeepEqual(observed, baseline) {
			t.Fatalf("discoverProfile(repetition=%d) operation locators changed\n got: %#v\nwant: %#v", repetition, observed, baseline)
		}
	}
}

func provenanceEscapeGlobalBoundary(packageName string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:   "provescape." + strings.ReplaceAll(packageName, "/", ".") + ".approved",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package: fixtureModulePath + "/" + packageName,
			Name:    "Approved",
		},
		Match: ObjectMatchChannel,
	}
}
