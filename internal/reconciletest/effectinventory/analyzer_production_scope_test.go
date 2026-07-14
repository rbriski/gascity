package effectinventory

import (
	"context"
	"go/types"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"

	"golang.org/x/tools/go/ssa"
)

func TestCanonicalProductionAnalysisUsesOneCommandRoot(t *testing.T) {
	want := []string{"./cmd/gc"}
	got := canonicalProductionAnalysisPatterns()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalProductionAnalysisPatterns() = %#v, want %#v", got, want)
	}

	got[0] = "mutated"
	if again := canonicalProductionAnalysisPatterns(); !reflect.DeepEqual(again, want) {
		t.Fatalf("canonicalProductionAnalysisPatterns() retained caller mutation: %#v", again)
	}
}

func TestProductionDependencyClosureIncludesDirectAPIWorkspaceRestart(t *testing.T) {
	config := fixtureAnalysisConfig(t, canonicalProductionAnalysisPatterns())
	boundary := requireCanonicalBoundary(t, "workspacesvc.registry.Restart")

	analysis, err := loadAnalysis(context.Background(), config, fixtureLinuxProfile())
	if err != nil {
		t.Fatalf("loadAnalysis() error: %v", err)
	}
	observed := observeExactSourceFunction(t, analysis, []BoundaryDefinition{boundary}, "github.com/gastownhall/gascity/internal/api", "humaHandleServiceRestart")
	want := observedKey{
		BoundaryID: boundary.ID,
		Matcher: OperationSite{
			Operation: OperationCall,
			Enclosing: FunctionRef{
				Object: ObjectRef{
					Package:  "github.com/gastownhall/gascity/internal/api",
					Receiver: "Server",
					Name:     "humaHandleServiceRestart",
				},
				File: "internal/api/huma_handlers_services.go",
			},
			Ordinal: 1,
		},
	}
	assertObservedSites(t, observed, []observedKey{want})
}

func TestProductionSourceFunctionsAreModuleDependencyClosureWithoutRuntimeTests(t *testing.T) {
	config := fixtureAnalysisConfig(t, canonicalProductionAnalysisPatterns())
	analysis, err := loadAnalysis(context.Background(), config, fixtureLinuxProfile())
	if err != nil {
		t.Fatalf("loadAnalysis() error: %v", err)
	}

	packages := sourceFunctionPackagePaths(analysis.sourceFuncs)
	for _, want := range []string{
		"github.com/gastownhall/gascity/cmd/gc",
		"github.com/gastownhall/gascity/internal/api",
		"github.com/gastownhall/gascity/internal/workspacesvc",
	} {
		if !containsString(packages, want) {
			t.Errorf("production source package closure does not contain %q", want)
		}
	}
	if unwanted := "github.com/gastownhall/gascity/internal/runtime/runtimetest"; containsString(packages, unwanted) {
		t.Errorf("production source package closure contains test-only package %q", unwanted)
	}
	for _, packagePath := range packages {
		if packagePath != config.ModulePath && !strings.HasPrefix(packagePath, config.ModulePath+"/") {
			t.Errorf("production source function package %q escapes module %q", packagePath, config.ModulePath)
		}
	}
}

func TestDependencyClosureFindsEffectAuthoredOutsideRoot(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/productionscope",
	})
	boundary := BoundaryDefinition{
		ID:     "profiletags.affect",
		Kind:   KindProviderMutation,
		Object: ObjectRef{Package: fixtureModulePath + "/profiletags", Name: "Affect"},
		Match:  ObjectMatchExact,
	}

	observed, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), []BoundaryDefinition{boundary})
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	assertObservedSites(t, observed, []observedKey{
		fixtureCall("profiletags.affect", "profiletags", "default.go", "ProfileRoute", nil, OperationCall, 1),
	})
}

func TestDependencyClosureProfilesAreDeterministicAndCoverPlatformAndNativeVariants(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/productionscope",
	})
	want := map[BuildProfileID][]string{
		BuildDarwinDefault:  {"profiletags/default.go", "routes/platform_darwin.go"},
		BuildDarwinNative:   {"profiletags/native.go", "routes/platform_darwin.go"},
		BuildLinuxDefault:   {"profiletags/default.go", "routes/platform_linux.go"},
		BuildLinuxNative:    {"profiletags/native.go", "routes/platform_linux.go"},
		BuildWindowsCompile: {"profiletags/default.go", "routes/platform_windows.go"},
	}

	for _, profile := range canonicalAnalysisProfiles() {
		t.Run(string(profile.ID), func(t *testing.T) {
			var baseline []string
			for repetition := 0; repetition < 2; repetition++ {
				analysis, err := loadAnalysis(context.Background(), config, profile)
				if err != nil {
					t.Fatalf("loadAnalysis(repetition=%d) error: %v", repetition, err)
				}
				manifest := sourceFunctionManifest(analysis)
				if repetition == 0 {
					baseline = manifest
				} else if !reflect.DeepEqual(manifest, baseline) {
					t.Fatalf("source function manifest changed\n got: %#v\nwant: %#v", manifest, baseline)
				}
				for _, suffix := range want[profile.ID] {
					if !manifestContainsFile(manifest, suffix) {
						t.Errorf("source function manifest does not contain profile-selected file %q", suffix)
					}
				}
			}
		})
	}
}

func requireCanonicalBoundary(t *testing.T, id string) BoundaryDefinition {
	t.Helper()
	for _, boundary := range CanonicalBoundaries() {
		if boundary.ID == id {
			return boundary
		}
	}
	t.Fatalf("canonical boundary %q does not exist", id)
	return BoundaryDefinition{}
}

func observeExactSourceFunction(t *testing.T, analysis *loadedAnalysis, definitions []BoundaryDefinition, packagePath, name string) []ObservedSite {
	t.Helper()
	boundaries, err := resolveBoundaries(analysis.packages, definitions)
	if err != nil {
		t.Fatalf("resolveBoundaries() error: %v", err)
	}

	var matches []*ssa.Function
	for function := range analysis.sourceFuncs {
		object, ok := function.Object().(*types.Func)
		if ok && object.Pkg() != nil && object.Pkg().Path() == packagePath && object.Name() == name {
			matches = append(matches, function)
		}
	}
	if len(matches) != 1 {
		t.Fatalf("source function %s.%s matches = %d, want 1", packagePath, name, len(matches))
	}

	var observed []observedCall
	var problems []string
	for _, block := range matches[0].Blocks {
		for _, instruction := range block.Instrs {
			call, ok := instruction.(ssa.CallInstruction)
			if !ok {
				continue
			}
			site, callProblems := analysis.observeCallInstruction(matches[0], call, boundaries)
			problems = append(problems, callProblems...)
			if site != nil {
				observed = append(observed, *site)
			}
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		t.Fatalf("observing source function failed:\n- %s", strings.Join(compactStrings(problems), "\n- "))
	}
	return numberObservedCalls(observed, analysis.profile.ID)
}

func sourceFunctionPackagePaths(functions map[*ssa.Function]bool) []string {
	seen := make(map[string]bool)
	for function := range functions {
		if function == nil || function.Package() == nil || function.Package().Pkg == nil {
			continue
		}
		seen[function.Package().Pkg.Path()] = true
	}
	result := make([]string, 0, len(seen))
	for packagePath := range seen {
		result = append(result, packagePath)
	}
	sort.Strings(result)
	return result
}

func sourceFunctionManifest(analysis *loadedAnalysis) []string {
	manifest := make([]string, 0, len(analysis.sourceFuncs))
	for function := range analysis.sourceFuncs {
		if function == nil || function.Package() == nil || function.Package().Pkg == nil {
			continue
		}
		position := analysis.program.Fset.PositionFor(function.Pos(), false)
		filename := position.Filename
		if relative, err := filepath.Rel(analysis.config.RepoRoot, filename); err == nil {
			filename = filepath.ToSlash(relative)
		}
		manifest = append(manifest, function.Package().Pkg.Path()+"|"+filename+"|"+function.String())
	}
	sort.Strings(manifest)
	return manifest
}

func manifestContainsFile(manifest []string, suffix string) bool {
	needle := "/" + suffix + "|"
	for _, entry := range manifest {
		if strings.Contains(entry, needle) {
			return true
		}
	}
	return false
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
