package effectinventory

import (
	"context"
	"reflect"
	"strings"
	"testing"
)

const rawProcessFixturePackage = fixtureModulePath + "/rawprocess"

func TestDiscoverRawProcessEffectsUsesTypedIdentityAndSignalSemantics(t *testing.T) {
	analysis := loadRawProcessFixture(t, "vehicle")

	got, err := discoverRawProcessEffects(analysis)
	if err != nil {
		t.Fatalf("discoverRawProcessEffects() error: %v", err)
	}
	want := []ObservedSite{
		rawProcessFixtureSite(rawProcessSignal, "vehicle", "Root"),
		rawProcessFixtureSite(rawProcessSyscallKill, "vehicle", "Root"),
		rawProcessFixtureSite(rawProcessSyscallKill, "vehicle", "helper"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("raw process effects mismatch\n got: %#v\nwant: %#v", got, want)
	}
	for _, site := range got {
		if site.Matcher.Enclosing.Object.Name == "Probe" || site.Matcher.Enclosing.Object.Receiver == "namedLookalike" {
			t.Fatalf("non-effect or same-name lookalike was inventoried: %+v", site)
		}
	}
}

func TestValidateRawProcessGuardRequiresExactReachableEvidence(t *testing.T) {
	analysis := loadRawProcessFixture(t, "vehicle")
	vehicle := rawProcessVehicle{
		ID:   "fixture.vehicle",
		Root: ObjectRef{Package: rawProcessFixturePackage + "/vehicle", Name: "Root"},
	}
	evidence := []rawProcessEvidence{
		fixtureRawProcessEvidence(rawProcessSyscallKill, "Root", "fixture.vehicle"),
		fixtureRawProcessEvidence(rawProcessSignal, "Root", "fixture.vehicle"),
		fixtureRawProcessEvidence(rawProcessSyscallKill, "helper", "fixture.vehicle"),
	}

	if err := validateRawProcessGuard(analysis, []rawProcessVehicle{vehicle}, evidence); err != nil {
		t.Fatalf("validateRawProcessGuard() rejected exact reachable evidence: %v", err)
	}

	t.Run("stale evidence", func(t *testing.T) {
		stale := append([]rawProcessEvidence(nil), evidence...)
		stale[0].Matcher.Ordinal = 2
		err := validateRawProcessGuard(analysis, []rawProcessVehicle{vehicle}, stale)
		assertRawProcessError(t, err, "has no evidence", "stale raw process evidence")
	})

	t.Run("duplicate evidence", func(t *testing.T) {
		duplicate := append(append([]rawProcessEvidence(nil), evidence...), evidence[0])
		err := validateRawProcessGuard(analysis, []rawProcessVehicle{vehicle}, duplicate)
		assertRawProcessError(t, err, "duplicate raw process evidence")
	})

	t.Run("evidence without profiles", func(t *testing.T) {
		extra := evidence[0]
		extra.Matcher.Ordinal = 2
		extra.Profiles = nil
		err := validateRawProcessGuard(analysis, []rawProcessVehicle{vehicle}, append(evidence, extra))
		assertRawProcessError(t, err, "has no build profiles")
	})
}

func TestValidateRawProcessGuardRejectsDirectAndInjectedBypasses(t *testing.T) {
	analysis := loadRawProcessFixture(t, "bypass")

	err := validateRawProcessGuard(analysis, nil, nil)
	assertRawProcessError(t, err,
		".Direct",
		".Injected",
		"raw syscall.Kill",
		"raw os.Process.Signal",
		"has no evidence",
	)
}

func TestValidateRawProcessGuardRejectsEvidenceOutsideItsTypedVehicle(t *testing.T) {
	analysis := loadRawProcessFixture(t, "bypass")
	vehicle := rawProcessVehicle{
		ID:   "fixture.unreachable",
		Root: ObjectRef{Package: rawProcessFixturePackage + "/bypass", Name: "SupplyRaw"},
	}
	evidence := []rawProcessEvidence{
		{
			Operation:  rawProcessSyscallKill,
			Matcher:    rawProcessFixtureSite(rawProcessSyscallKill, "bypass", "Direct").Matcher,
			VehicleIDs: []string{"fixture.unreachable"},
			Profiles:   []BuildProfileID{BuildLinuxDefault},
		},
	}

	err := validateRawProcessGuard(analysis, []rawProcessVehicle{vehicle}, evidence)
	assertRawProcessError(t, err, "is not reachable from typed vehicle", ".Direct")
}

func TestValidateRawProcessGuardRejectsSiblingCallerThatBypassesVehicle(t *testing.T) {
	analysis := loadRawProcessFixture(t, "callerbypass")
	packagePath := rawProcessFixturePackage + "/callerbypass"
	vehicle := rawProcessVehicle{
		ID:   "fixture.vehicle",
		Root: ObjectRef{Package: packagePath, Name: "Root"},
	}
	evidence := []rawProcessEvidence{{
		Operation: rawProcessSyscallKill,
		Matcher: OperationSite{
			Operation: OperationCall,
			Enclosing: FunctionRef{
				Object: ObjectRef{Package: packagePath, Name: "helper"},
				File:   "internal/reconciletest/effectinventory/testdata/analyzerfixture/rawprocess/callerbypass/callerbypass.go",
			},
			Ordinal: 1,
		},
		VehicleIDs: []string{"fixture.vehicle"},
		Profiles:   []BuildProfileID{BuildLinuxDefault},
	}}

	err := validateRawProcessGuard(analysis, []rawProcessVehicle{vehicle}, evidence)
	assertRawProcessError(t, err, "caller path bypasses typed vehicle", ".BypassVehicle")
}

func TestValidateRawProcessGuardRequiresEverySharedVehicleExplicitly(t *testing.T) {
	analysis := loadRawProcessFixture(t, "multivehicle")
	packagePath := rawProcessFixturePackage + "/multivehicle"
	vehicles := []rawProcessVehicle{
		{ID: "fixture.root-a", Root: ObjectRef{Package: packagePath, Name: "RootA"}},
		{ID: "fixture.root-b", Root: ObjectRef{Package: packagePath, Name: "RootB"}},
	}
	evidence := rawProcessEvidence{
		Operation: rawProcessSyscallKill,
		Matcher: OperationSite{
			Operation: OperationCall,
			Enclosing: FunctionRef{
				Object: ObjectRef{Package: packagePath, Name: "helper"},
				File:   "internal/reconciletest/effectinventory/testdata/analyzerfixture/rawprocess/multivehicle/multivehicle.go",
			},
			Ordinal: 1,
		},
		VehicleIDs: []string{"fixture.root-a", "fixture.root-b"},
		Profiles:   []BuildProfileID{BuildLinuxDefault},
	}

	if err := validateRawProcessGuard(analysis, vehicles, []rawProcessEvidence{evidence}); err != nil {
		t.Fatalf("validateRawProcessGuard() rejected explicit shared vehicles: %v", err)
	}
	evidence.VehicleIDs = []string{"fixture.root-b", "fixture.root-a"}
	if err := validateRawProcessGuard(analysis, vehicles, []rawProcessEvidence{evidence}); err != nil {
		t.Fatalf("validateRawProcessGuard() depended on shared vehicle order: %v", err)
	}

	evidence.VehicleIDs = []string{"fixture.root-a"}
	err := validateRawProcessGuard(analysis, vehicles, []rawProcessEvidence{evidence})
	assertRawProcessError(t, err, "caller path bypasses typed vehicle", ".RootB")
}

func TestCanonicalRawProcessGuardAcrossProductionProfiles(t *testing.T) {
	config := fixtureAnalysisConfig(t, canonicalProductionAnalysisPatterns())
	for _, profile := range canonicalAnalysisProfiles() {
		profile := profile
		t.Run(string(profile.ID), func(t *testing.T) {
			analysis, err := loadAnalysis(context.Background(), config, profile)
			if err != nil {
				t.Fatalf("loadAnalysis() error: %v", err)
			}
			if err := validateCanonicalRawProcessGuard(analysis); err != nil {
				t.Fatalf("validateCanonicalRawProcessGuard() error: %v", err)
			}
		})
	}
}

func loadRawProcessFixture(t *testing.T, packageName string) *loadedAnalysis {
	t.Helper()
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/rawprocess/" + packageName,
	})
	analysis, err := loadAnalysis(context.Background(), config, fixtureLinuxProfile())
	if err != nil {
		t.Fatalf("loadAnalysis() error: %v", err)
	}
	return analysis
}

func rawProcessFixtureSite(operation rawProcessOperation, packageName, function string) ObservedSite {
	return ObservedSite{
		BoundaryID: string(operation),
		Matcher: OperationSite{
			Operation: OperationCall,
			Enclosing: FunctionRef{
				Object:      ObjectRef{Package: rawProcessFixturePackage + "/" + packageName, Name: function},
				File:        "internal/reconciletest/effectinventory/testdata/analyzerfixture/rawprocess/" + packageName + "/" + packageName + ".go",
				ClosurePath: []int{},
			},
			Ordinal: 1,
		},
		Profile: BuildLinuxDefault,
	}
}

func fixtureRawProcessEvidence(operation rawProcessOperation, function, vehicleID string) rawProcessEvidence {
	return rawProcessEvidence{
		Operation:  operation,
		Matcher:    rawProcessFixtureSite(operation, "vehicle", function).Matcher,
		VehicleIDs: []string{vehicleID},
		Profiles:   []BuildProfileID{BuildLinuxDefault},
	}
}

func assertRawProcessError(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("error = nil, want raw process guard rejection")
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Errorf("error = %q, want fragment %q", err, fragment)
		}
	}
}
