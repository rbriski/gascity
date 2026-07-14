package effectinventory

import (
	"context"
	"testing"
)

func BenchmarkDiscoverProductionLinuxDefaultAllBoundaries(b *testing.B) {
	config := fixtureAnalysisConfig(b, canonicalProductionAnalysisPatterns())
	profile, ok := canonicalAnalysisProfile(BuildLinuxDefault)
	if !ok {
		b.Fatal("canonical Linux/default analysis profile is missing")
	}
	boundaries := CanonicalBoundaries()

	b.ResetTimer()
	for b.Loop() {
		// The benchmark covers discovery cost independently of whether an
		// in-progress catalog has discharged every fail-closed diagnostic.
		_, _ = discoverProfile(context.Background(), config, profile, boundaries)
	}
}
