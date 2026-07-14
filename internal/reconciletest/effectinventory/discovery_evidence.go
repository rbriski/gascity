package effectinventory

import (
	"fmt"
	"sort"
	"strings"
)

const (
	boundaryDigestPrefix     = "boundaries-v1-"
	sourceScopeDigestPrefix  = "source-scope-v1-"
	discoveryEvidencePrefix  = "discovery-evidence-v1-"
	sha256LowerHexCharacters = 64
)

type profileDiscovery struct {
	profile BuildProfileID
	sites   []ObservedSite
}

// DiscoveryResult is opaque analyzer evidence consumed by CompileRegistry.
// Its unexported state prevents callers outside this package from substituting
// catalog-derived observations for a completed canonical analysis.
type DiscoveryResult struct {
	boundaryDigest      string
	sourceScopeDigest   string
	expectedGitRevision string
	observedGitRevision string
	gitHeadIdentity     string
	profiles            []profileDiscovery
	evidenceDigest      string
}

func newDiscoveryResult(boundaryDigest, sourceScopeDigest, expectedGitRevision, observedGitRevision, gitHeadIdentity string, profiles []profileDiscovery) DiscoveryResult {
	result := DiscoveryResult{
		boundaryDigest:      boundaryDigest,
		sourceScopeDigest:   sourceScopeDigest,
		expectedGitRevision: expectedGitRevision,
		observedGitRevision: observedGitRevision,
		gitHeadIdentity:     gitHeadIdentity,
		profiles:            cloneProfileDiscoveries(profiles),
	}
	result.evidenceDigest = deriveDiscoveryEvidenceDigest(result)
	return result
}

func validateDiscoveryEvidence(discovery DiscoveryResult, expectedBoundaryDigest string, problems *[]string) {
	if discovery.boundaryDigest != expectedBoundaryDigest {
		*problems = append(*problems, fmt.Sprintf(
			"discovery boundary digest %q does not match registry digest %q",
			discovery.boundaryDigest,
			expectedBoundaryDigest,
		))
	}
	if !contentDigest(discovery.sourceScopeDigest, sourceScopeDigestPrefix) {
		*problems = append(*problems, "discovery source-scope digest must be source-scope-v1- plus 64 lowercase hexadecimal characters")
	}
	if !lowerHex(discovery.expectedGitRevision, 40) {
		*problems = append(*problems, "discovery expected git revision must be 40 lowercase hexadecimal characters")
	}
	if !lowerHex(discovery.observedGitRevision, 40) {
		*problems = append(*problems, "discovery observed git revision must be 40 lowercase hexadecimal characters")
	}
	if discovery.expectedGitRevision != discovery.observedGitRevision {
		*problems = append(*problems, fmt.Sprintf(
			"discovery observed git revision %q does not match expected git revision %q",
			discovery.observedGitRevision,
			discovery.expectedGitRevision,
		))
	}
	if strings.TrimSpace(discovery.gitHeadIdentity) == "" {
		*problems = append(*problems, "discovery git HEAD identity is required")
	} else if !strings.HasSuffix(discovery.gitHeadIdentity, "@"+discovery.observedGitRevision) {
		*problems = append(*problems, "discovery git HEAD identity does not bind the observed git revision")
	}
	wantEvidenceDigest := deriveDiscoveryEvidenceDigest(discovery)
	if discovery.evidenceDigest != wantEvidenceDigest {
		*problems = append(*problems, fmt.Sprintf(
			"discovery evidence digest %q does not match derived digest %q",
			discovery.evidenceDigest,
			wantEvidenceDigest,
		))
	}
}

func deriveDiscoveryEvidenceDigest(discovery DiscoveryResult) string {
	return deriveContentID(discoveryEvidencePrefix, canonicalFields(
		"discovery-evidence-v1",
		discovery.boundaryDigest,
		discovery.sourceScopeDigest,
		discovery.expectedGitRevision,
		discovery.observedGitRevision,
		discovery.gitHeadIdentity,
		canonicalProfileDiscoveries(discovery.profiles),
	))
}

func canonicalProfileDiscoveries(profiles []profileDiscovery) string {
	records := make([]string, len(profiles))
	for index, profile := range profiles {
		sites := make([]string, len(profile.sites))
		for siteIndex, site := range profile.sites {
			sites[siteIndex] = canonicalObservedSite(site)
		}
		sort.Strings(sites)
		records[index] = canonicalFields(
			"profile-discovery-v1",
			string(profile.profile),
			canonicalStringList("observed-sites-v1", sites),
		)
	}
	sort.Strings(records)
	return canonicalStringList("profile-discoveries-v1", records)
}

func canonicalObservedSite(site ObservedSite) string {
	return canonicalFields(
		"observed-site-v1",
		site.BoundaryID,
		canonicalOperationSite(site.Matcher),
		string(site.Profile),
	)
}

func cloneProfileDiscoveries(profiles []profileDiscovery) []profileDiscovery {
	result := make([]profileDiscovery, len(profiles))
	for index, profile := range profiles {
		result[index].profile = profile.profile
		result[index].sites = make([]ObservedSite, len(profile.sites))
		for siteIndex, site := range profile.sites {
			result[index].sites[siteIndex] = site
			result[index].sites[siteIndex].Matcher = cloneOperationSite(site.Matcher)
		}
	}
	return result
}

func contentDigest(value, prefix string) bool {
	return strings.HasPrefix(value, prefix) && lowerHex(strings.TrimPrefix(value, prefix), sha256LowerHexCharacters)
}
