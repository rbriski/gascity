package effectinventory

import (
	"context"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestCanonicalAnalysisProfiles(t *testing.T) {
	want := []analysisProfile{
		{ID: BuildDarwinDefault, GOOS: "darwin", GOARCH: "amd64"},
		{ID: BuildDarwinNative, GOOS: "darwin", GOARCH: "amd64", Tags: []string{"gascity_native_beads"}},
		{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"},
		{ID: BuildLinuxNative, GOOS: "linux", GOARCH: "amd64", Tags: []string{"gascity_native_beads"}},
		{ID: BuildWindowsCompile, GOOS: "windows", GOARCH: "amd64"},
	}

	got := canonicalAnalysisProfiles()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalAnalysisProfiles() = %#v, want %#v", got, want)
	}
	got[1].Tags[0] = "mutated"
	if again := canonicalAnalysisProfiles(); !reflect.DeepEqual(again, want) {
		t.Fatalf("canonicalAnalysisProfiles() retained caller mutation: %#v", again)
	}
}

func TestValidateAnalysisProfileRejectsNonCanonicalDefinitions(t *testing.T) {
	tests := []struct {
		name    string
		profile analysisProfile
		want    string
	}{
		{"unknown id", analysisProfile{ID: "linux/other", GOOS: "linux", GOARCH: "amd64"}, "unknown analysis profile"},
		{"wrong os", analysisProfile{ID: BuildLinuxDefault, GOOS: "darwin", GOARCH: "amd64"}, "must use GOOS"},
		{"wrong arch", analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "arm64"}, "must use GOARCH"},
		{"default with tag", analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64", Tags: []string{"gascity_native_beads"}}, "must use tags"},
		{"native without tag", analysisProfile{ID: BuildLinuxNative, GOOS: "linux", GOARCH: "amd64"}, "must use tags"},
		{"native duplicate tag", analysisProfile{ID: BuildLinuxNative, GOOS: "linux", GOARCH: "amd64", Tags: []string{"gascity_native_beads", "gascity_native_beads"}}, "must use tags"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateAnalysisProfile(tt.profile)
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("validateAnalysisProfile(%#v) error = %v, want %q", tt.profile, err, tt.want)
			}
		})
	}
}

func TestLoadAnalysisRejectsNonCanonicalProfile(t *testing.T) {
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/profiletags",
	})
	profile := analysisProfile{ID: BuildLinuxDefault, GOOS: "darwin", GOARCH: "amd64"}

	_, err := loadAnalysis(context.Background(), config, profile)
	if err == nil || !strings.Contains(err.Error(), "must use GOOS") {
		t.Fatalf("loadAnalysis() error = %v, want canonical GOOS rejection", err)
	}
}

func TestProfileEnvironmentScrubsInheritedGoConfiguration(t *testing.T) {
	t.Setenv("CGO_ENABLED", "1")
	t.Setenv("GOARCH", "arm64")
	t.Setenv("GOCACHEPROG", "/definitely/missing")
	t.Setenv("GODEBUG", "toolchaintrace=1")
	t.Setenv("GOENV", "/tmp/host-goenv")
	t.Setenv("GOEXPERIMENT", "host-experiment")
	t.Setenv("GOFIPS140", "definitely-invalid")
	t.Setenv("GOFLAGS", "-tags=host")
	t.Setenv("GOOS", "plan9")
	t.Setenv("GOPACKAGESDRIVER", "/tmp/host-driver")
	t.Setenv("GOROOT", "/definitely/invalid")
	t.Setenv("GOWORK", "/tmp/host.work")
	t.Setenv("GOAMD64", "v4")
	t.Setenv("GO111MODULE", "off")
	t.Setenv("GOTOOLCHAIN", "definitely-invalid")

	environment := profileEnvironment(analysisProfile{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"})
	want := map[string]string{
		"GO111MODULE":      "on",
		"CGO_ENABLED":      "0",
		"GOARCH":           "amd64",
		"GOCACHEPROG":      "",
		"GODEBUG":          "",
		"GOENV":            "off",
		"GOEXPERIMENT":     "",
		"GOFIPS140":        "off",
		"GOFLAGS":          "",
		"GOOS":             "linux",
		"GOPACKAGESDRIVER": "off",
		"GOROOT":           "",
		"GOTOOLCHAIN":      runtime.Version(),
		"GOWORK":           "off",
		"GOAMD64":          "v1",
	}
	seen := make(map[string]int)
	for _, item := range environment {
		name, value, _ := strings.Cut(item, "=")
		if expected, tracked := want[name]; tracked {
			seen[name]++
			if value != expected {
				t.Errorf("environment %s = %q, want %q", name, value, expected)
			}
		}
	}
	for name := range want {
		if seen[name] != 1 {
			t.Errorf("environment contains %s %d times, want once", name, seen[name])
		}
	}
}

func TestCanonicalProfilesSelectNativeAndDefaultSources(t *testing.T) {
	const packageName = "profiletags"
	config := fixtureAnalysisConfig(t, []string{
		"./internal/reconciletest/effectinventory/testdata/analyzerfixture/" + packageName,
	})
	boundary := BoundaryDefinition{
		ID:     "profiletags.affect",
		Kind:   KindProviderMutation,
		Object: ObjectRef{Package: fixtureModulePath + "/" + packageName, Name: "Affect"},
		Match:  ObjectMatchExact,
	}

	wantFiles := map[BuildProfileID]string{
		BuildDarwinDefault: "default.go",
		BuildDarwinNative:  "native.go",
		BuildLinuxDefault:  "default.go",
		BuildLinuxNative:   "native.go",
	}
	for _, profile := range canonicalAnalysisProfiles() {
		wantFile, relevant := wantFiles[profile.ID]
		if !relevant {
			continue
		}
		observed, err := discoverProfile(context.Background(), config, profile, []BoundaryDefinition{boundary})
		if err != nil {
			t.Fatalf("discoverProfile(%s) error: %v", profile.ID, err)
		}
		if len(observed) != 1 || !strings.HasSuffix(observed[0].Matcher.Enclosing.File, "/"+wantFile) {
			t.Fatalf("discoverProfile(%s) observed = %#v, want one site in %s", profile.ID, observed, wantFile)
		}
	}
}

func TestWindowsProductionAnalysisTypeChecks(t *testing.T) {
	config := fixtureAnalysisConfig(t, nil)
	config.Patterns = canonicalProductionAnalysisPatterns()
	profile, ok := canonicalAnalysisProfile(BuildWindowsCompile)
	if !ok {
		t.Fatal("canonical Windows analysis profile is missing")
	}

	if _, err := loadAnalysis(context.Background(), config, profile); err != nil {
		t.Fatalf("Windows production analysis failed: %v", err)
	}
}
