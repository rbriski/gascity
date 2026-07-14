package effectinventory

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

const fixtureModulePath = "github.com/gastownhall/gascity/internal/reconciletest/effectinventory/testdata/analyzerfixture"

func TestDiscoverProfileDoesNotMutateAnalyzedModuleFiles(t *testing.T) {
	sourceConfig := fixtureAnalysisConfig(t, nil)
	repoRoot := t.TempDir()
	for _, name := range []string{"go.mod", "go.sum"} {
		content, err := os.ReadFile(filepath.Join(sourceConfig.RepoRoot, name))
		if err != nil {
			t.Fatalf("reading source %s: %v", name, err)
		}
		if err := os.WriteFile(filepath.Join(repoRoot, name), content, 0o600); err != nil {
			t.Fatalf("writing copied %s: %v", name, err)
		}
	}
	fixturePath := filepath.Join("internal", "reconciletest", "effectinventory", "testdata", "analyzerfixture")
	if err := os.CopyFS(filepath.Join(repoRoot, fixturePath), os.DirFS(filepath.Join(sourceConfig.RepoRoot, fixturePath))); err != nil {
		t.Fatalf("copying analyzer fixture: %v", err)
	}
	config := analysisConfig{
		RepoRoot:   repoRoot,
		ModulePath: sourceConfig.ModulePath,
		Patterns: []string{
			"./" + filepath.ToSlash(filepath.Join(fixturePath, "boundary")),
			"./" + filepath.ToSlash(filepath.Join(fixturePath, "routes")),
		},
	}
	beforeMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	beforeSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := discoverProfile(context.Background(), config, fixtureLinuxProfile(), fixtureBoundaries()); err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}
	afterMod, err := os.ReadFile(filepath.Join(repoRoot, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	afterSum, err := os.ReadFile(filepath.Join(repoRoot, "go.sum"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterMod, beforeMod) {
		t.Error("discoverProfile() mutated analyzed go.mod")
	}
	if !bytes.Equal(afterSum, beforeSum) {
		t.Error("discoverProfile() mutated analyzed go.sum")
	}
}

func TestDiscoverProfileFindsExactTypedAndVTAEffectSites(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/routes",
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}

	observed, err := discoverProfile(context.Background(), config, profile, fixtureBoundaries())
	if err != nil {
		t.Fatalf("discoverProfile() error: %v", err)
	}

	want := []observedKey{
		fixtureCall("mutator.mutate", "routes", "routes.go", "SharedRoute", nil, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "routes.go", "InterfaceAliasRoute", nil, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "routes.go", "ValueImplementorRoute", nil, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "routes.go", "PointerImplementorRoute", nil, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "routes.go", "ConcreteAliasRoute", nil, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "routes.go", "PromotedMethodRoute", nil, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "routes.go", "ClosureRoute", []int{1}, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "routes.go", "MethodValueRoute", nil, OperationCall, 1),
		fixtureCall("mutator.mutate", "routes", "platform_linux.go", "PlatformRoute", nil, OperationCall, 1),
		fixtureCall("boundary.emit", "routes", "routes.go", "FunctionVariableRoute", nil, OperationCall, 1),
		fixtureCall("boundary.emit", "routes", "routes.go", "FunctionFieldRoute", nil, OperationCall, 1),
		fixtureCall("boundary.emit", "routes", "routes.go", "GoroutineRoute", nil, OperationGo, 1),
		fixtureCall("boundary.emit", "routes", "routes.go", "DeferredRoute", nil, OperationDefer, 1),
	}

	assertObservedSites(t, observed, want)
	for _, site := range observed {
		if strings.Contains(site.Matcher.Enclosing.Object.Name, "Unrelated") {
			t.Fatalf("unrelated same-name method was classified as an effect: %+v", site)
		}
	}
}

func TestDiscoverProfileSelectsPlatformSpecificSite(t *testing.T) {
	tests := []struct {
		profile analysisProfile
		file    string
	}{
		{analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}, "platform_linux.go"},
		{analysisProfile{ID: BuildDarwinDefault, GOOS: "darwin", GOARCH: "amd64"}, "platform_darwin.go"},
		{analysisProfile{ID: BuildWindowsCompile, GOOS: "windows", GOARCH: "amd64"}, "platform_windows.go"},
	}

	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/routes",
	})
	for _, tt := range tests {
		t.Run(string(tt.profile.ID), func(t *testing.T) {
			observed, err := discoverProfile(context.Background(), config, tt.profile, fixtureBoundaries()[:1])
			if err != nil {
				t.Fatalf("discoverProfile() error: %v", err)
			}
			var platformFiles []string
			for _, site := range observed {
				if site.Matcher.Enclosing.Object.Name == "PlatformRoute" {
					platformFiles = append(platformFiles, filepath.Base(site.Matcher.Enclosing.File))
				}
			}
			if len(platformFiles) != 1 || platformFiles[0] != tt.file {
				t.Fatalf("platform files = %v, want [%s]", platformFiles, tt.file)
			}
		})
	}
}

func TestDiscoverProfileRejectsUnresolvedAndReflectiveEffectEscapes(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/boundary",
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/escape",
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"}

	_, err := discoverProfile(context.Background(), config, profile, fixtureBoundaries())
	if err == nil {
		t.Fatal("discoverProfile() returned nil, want unresolved-effect error")
	}
	for _, want := range []string{"DynamicFunctionParameter", "DynamicFunctionField", "ReflectiveMethod"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("discoverProfile() error = %q, want %q", err, want)
		}
	}
}

func fixtureAnalysisConfig(t testing.TB, patterns []string) analysisConfig {
	t.Helper()
	_, sourceFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller() failed")
	}
	return analysisConfig{
		RepoRoot:   filepath.Clean(filepath.Join(filepath.Dir(sourceFile), "..", "..", "..")),
		ModulePath: "github.com/gastownhall/gascity",
		Patterns:   patterns,
	}
}

func fixtureBoundaries() []BoundaryDefinition {
	return []BoundaryDefinition{
		{
			ID:   "mutator.mutate",
			Kind: KindProviderMutation,
			Object: ObjectRef{
				Package:  fixtureModulePath + "/boundary",
				Receiver: "Mutator",
				Name:     "Mutate",
			},
			Match: ObjectMatchInterfaceImplementors,
		},
		{
			ID:   "boundary.emit",
			Kind: KindProviderMutation,
			Object: ObjectRef{
				Package: fixtureModulePath + "/boundary",
				Name:    "Emit",
			},
			Match: ObjectMatchExact,
		},
	}
}

type observedKey struct {
	BoundaryID string
	Matcher    OperationSite
}

func fixtureCall(boundaryID, packageName, file, function string, closure []int, operation OperationKind, ordinal int) observedKey {
	packagePath := fixtureModulePath
	if packageName != "" {
		packagePath += "/" + packageName
	}
	return observedKey{
		BoundaryID: boundaryID,
		Matcher: OperationSite{
			Operation: operation,
			Enclosing: FunctionRef{
				Object: ObjectRef{Package: packagePath, Name: function},
				File: filepath.ToSlash(filepath.Join(
					"internal/reconciletest/effectinventory/testdata/analyzerfixture",
					packageName,
					file,
				)),
				ClosurePath: closure,
			},
			Ordinal: ordinal,
		},
	}
}

func assertObservedSites(t *testing.T, observed []ObservedSite, want []observedKey) {
	t.Helper()
	gotKeys := make([]string, len(observed))
	for index, site := range observed {
		gotKeys[index] = observedKey{BoundaryID: site.BoundaryID, Matcher: site.Matcher}.key()
	}
	wantKeys := make([]string, len(want))
	for index, site := range want {
		wantKeys[index] = site.key()
	}
	sort.Strings(gotKeys)
	sort.Strings(wantKeys)
	if strings.Join(gotKeys, "\n") != strings.Join(wantKeys, "\n") {
		t.Fatalf("observed sites mismatch\n got:\n%s\nwant:\n%s", strings.Join(gotKeys, "\n"), strings.Join(wantKeys, "\n"))
	}
}

func (key observedKey) key() string {
	return key.BoundaryID + "|" + key.Matcher.key()
}
