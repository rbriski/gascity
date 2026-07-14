package effectinventory

import "fmt"

func canonicalAnalysisProfiles() []analysisProfile {
	return []analysisProfile{
		{ID: BuildDarwinDefault, GOOS: "darwin", GOARCH: "amd64"},
		{ID: BuildDarwinNative, GOOS: "darwin", GOARCH: "amd64", Tags: []string{"gascity_native_beads"}},
		{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"},
		{ID: BuildLinuxNative, GOOS: "linux", GOARCH: "amd64", Tags: []string{"gascity_native_beads"}},
		{ID: BuildWindowsCompile, GOOS: "windows", GOARCH: "amd64"},
	}
}

func canonicalAnalysisProfile(id BuildProfileID) (analysisProfile, bool) {
	for _, profile := range canonicalAnalysisProfiles() {
		if profile.ID == id {
			return profile, true
		}
	}
	return analysisProfile{}, false
}

func validateAnalysisProfile(profile analysisProfile) error {
	want, ok := canonicalAnalysisProfile(profile.ID)
	if !ok {
		return fmt.Errorf("unknown analysis profile %q", profile.ID)
	}
	if profile.GOOS != want.GOOS {
		return fmt.Errorf("analysis profile %q must use GOOS %q, got %q", profile.ID, want.GOOS, profile.GOOS)
	}
	if profile.GOARCH != want.GOARCH {
		return fmt.Errorf("analysis profile %q must use GOARCH %q, got %q", profile.ID, want.GOARCH, profile.GOARCH)
	}
	if !equalStrings(profile.Tags, want.Tags) {
		return fmt.Errorf("analysis profile %q must use tags %q, got %q", profile.ID, want.Tags, profile.Tags)
	}
	return nil
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
