package effectinventory

import (
	"context"
	"reflect"
	"testing"
)

func TestDiscoverProfilePreservesClosedChannelProvenanceThroughCopies(t *testing.T) {
	const packageName = "channelprov/closed"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := fixtureChannelProvenanceBoundary(packageName)
	want := []observedKey{
		fixtureCall(boundary.ID, packageName, "closed.go", "StaticPassthroughSend", nil, OperationChannelSend, 1),
		fixtureCall(boundary.ID, packageName, "closed.go", "LocalStructCopySend", nil, OperationChannelSend, 1),
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
			continue
		}
		if !reflect.DeepEqual(observed, baseline) {
			t.Fatalf("discoverProfile(repetition=%d) operation locators changed\n got: %#v\nwant: %#v", repetition, observed, baseline)
		}
	}
}

func TestDiscoverProfileRejectsPhiSelectWithInjectedChannelDeterministically(t *testing.T) {
	const packageName = "channelprov/phi"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := fixtureChannelProvenanceBoundary(packageName)
	wantDiagnostic := fixtureOpenWorldChannelDiagnostic(packageName, "phi.go", "SelectInjected")

	for repetition := 0; repetition < 3; repetition++ {
		observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
		if err == nil {
			t.Fatalf("discoverProfile(repetition=%d) error = nil, want open-world provenance diagnostic", repetition)
		}
		if observed != nil {
			t.Fatalf("discoverProfile(repetition=%d) observed = %#v, want nil so the exact branch is not inventoried", repetition, observed)
		}
		if err.Error() != wantDiagnostic {
			t.Fatalf("discoverProfile(repetition=%d) diagnostic mismatch\n got:\n%s\nwant:\n%s", repetition, err, wantDiagnostic)
		}
	}
}

func TestDiscoverProfileRejectsInjectedChannelStoredInLocalFieldDeterministically(t *testing.T) {
	const packageName = "channelprov/injectedfield"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := fixtureChannelProvenanceBoundary(packageName)
	wantDiagnostic := fixtureOpenWorldChannelDiagnostic(packageName, "injectedfield.go", "InjectedLocalFieldSend")

	for repetition := 0; repetition < 3; repetition++ {
		observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
		if err == nil {
			t.Fatalf("discoverProfile(repetition=%d) error = nil, want open-world provenance diagnostic", repetition)
		}
		if observed != nil {
			t.Fatalf("discoverProfile(repetition=%d) observed = %#v, want nil", repetition, observed)
		}
		if err.Error() != wantDiagnostic {
			t.Fatalf("discoverProfile(repetition=%d) diagnostic mismatch\n got:\n%s\nwant:\n%s", repetition, err, wantDiagnostic)
		}
	}
}

func fixtureChannelProvenanceBoundary(packageName string) BoundaryDefinition {
	return BoundaryDefinition{
		ID:   "channelprov." + packageName + ".approved",
		Kind: KindWakeSource,
		Object: ObjectRef{
			Package: fixtureModulePath + "/" + packageName,
			Name:    "Approved",
		},
		Match: ObjectMatchChannel,
	}
}

func fixtureOpenWorldChannelDiagnostic(packageName, file, function string) string {
	locator := fixtureCall("", packageName, file, function, nil, OperationChannelSend, 1).Matcher.Enclosing.key()
	return "effect discovery failed for profile \"linux/default\":\n- " + locator +
		": unresolved channel operation has open-world provenance compatible with an inventoried boundary"
}
