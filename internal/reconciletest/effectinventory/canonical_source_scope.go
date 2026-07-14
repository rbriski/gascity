package effectinventory

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type sourceFingerprint struct {
	path   string
	digest string
}

type sourceAudit struct {
	candidates []string
	files      []sourceFingerprint
}

type sourceScopeManifest struct {
	modulePath string
	patterns   []string
	auditRoots []string
	audit      sourceAudit
	runs       []canonicalProfileRun
	digest     string
}

func auditCanonicalSources(ctx context.Context, config sourceSelectionConfig) (sourceAudit, error) {
	if err := auditCanonicalSourceSelection(ctx, config); err != nil {
		return sourceAudit{}, err
	}
	candidates, err := sourceCandidates(config.RepoRoot, config.Roots)
	if err != nil {
		return sourceAudit{}, err
	}
	inputs := append([]string(nil), candidates...)
	for _, name := range []string{"go.mod", "go.sum"} {
		if _, err := os.Stat(filepath.Join(config.RepoRoot, name)); err != nil {
			return sourceAudit{}, fmt.Errorf("source-scope input %s: %s", name, stableFilesystemError(err))
		}
		inputs = append(inputs, name)
	}
	files, err := fingerprintSourcePaths(config.RepoRoot, inputs)
	if err != nil {
		return sourceAudit{}, err
	}
	return sourceAudit{
		candidates: append([]string(nil), candidates...),
		files:      files,
	}, nil
}

func runCanonicalProfile(ctx context.Context, config analysisConfig, profile analysisProfile, boundaries []BoundaryDefinition) (canonicalProfileRun, error) {
	analysis, err := loadAnalysis(ctx, config, profile)
	if err != nil {
		return canonicalProfileRun{}, err
	}
	paths, err := analysisSourcePaths(analysis)
	if err != nil {
		return canonicalProfileRun{}, err
	}
	before, err := fingerprintSourcePaths(config.RepoRoot, paths)
	if err != nil {
		return canonicalProfileRun{}, err
	}
	if err := validateCanonicalRawProcessGuard(analysis); err != nil {
		return canonicalProfileRun{}, err
	}
	sites, err := discoverLoadedProfile(analysis, boundaries)
	if err != nil {
		return canonicalProfileRun{}, err
	}
	after, err := fingerprintSourcePaths(config.RepoRoot, paths)
	if err != nil {
		return canonicalProfileRun{}, err
	}
	if !equalSourceFingerprints(before, after) {
		return canonicalProfileRun{}, fmt.Errorf("source scope drift while discovering profile %q", profile.ID)
	}
	return canonicalProfileRun{
		profile: profile.ID,
		sites:   sites,
		files:   before,
	}, nil
}

func analysisSourcePaths(analysis *loadedAnalysis) ([]string, error) {
	seen := make(map[string]bool)
	var result []string
	for _, pkg := range analysis.sourcePackages {
		if len(pkg.CompiledGoFiles) == 0 {
			return nil, fmt.Errorf("source-scope package %s has no compiled Go files", pkg.PkgPath)
		}
		for _, filename := range pkg.CompiledGoFiles {
			relative, inside, err := sourcePathRelativeToRepo(analysis.config.RepoRoot, filename)
			if err != nil {
				return nil, fmt.Errorf("source-scope package %s has invalid source path: %w", pkg.PkgPath, err)
			}
			if !inside {
				return nil, fmt.Errorf("source-scope package %s has source outside the repository", pkg.PkgPath)
			}
			if strings.HasSuffix(relative, "_test.go") {
				return nil, fmt.Errorf("source-scope Tests=false profile selected test file %s", relative)
			}
			if !cleanRepoPath(relative) {
				return nil, fmt.Errorf("source-scope file %q is not a clean repository-relative path", relative)
			}
			if !seen[relative] {
				seen[relative] = true
				result = append(result, relative)
			}
		}
	}
	sort.Strings(result)
	if len(result) == 0 {
		return nil, fmt.Errorf("source-scope profile selected no in-module source files")
	}
	return result, nil
}

func buildSourceScopeManifest(config analysisConfig, auditRoots []string, audit sourceAudit, runs []canonicalProfileRun) (sourceScopeManifest, error) {
	manifest := sourceScopeManifest{
		modulePath: config.ModulePath,
		patterns:   append([]string(nil), config.Patterns...),
		auditRoots: append([]string(nil), auditRoots...),
		audit: sourceAudit{
			candidates: append([]string(nil), audit.candidates...),
			files:      append([]sourceFingerprint(nil), audit.files...),
		},
		runs: cloneCanonicalProfileRuns(runs),
	}
	var problems []string
	if manifest.modulePath != canonicalModulePath {
		problems = append(problems, fmt.Sprintf("module path %q is not canonical", manifest.modulePath))
	}
	if !equalStrings(manifest.patterns, canonicalProductionAnalysisPatterns()) {
		problems = append(problems, fmt.Sprintf("analysis patterns %q are not canonical", manifest.patterns))
	}
	if !equalStrings(manifest.auditRoots, canonicalSourceAuditRoots()) {
		problems = append(problems, fmt.Sprintf("source-audit roots %q are not canonical", manifest.auditRoots))
	}
	validateSourcePaths("source audit candidates", manifest.audit.candidates, &problems)
	validateSourceFingerprints("source audit", manifest.audit.files, &problems)
	profiles := canonicalAnalysisProfiles()
	if len(manifest.runs) != len(profiles) {
		problems = append(problems, fmt.Sprintf("profile runs = %d, want %d", len(manifest.runs), len(profiles)))
	}
	for index, run := range manifest.runs {
		if index >= len(profiles) {
			break
		}
		if run.profile != profiles[index].ID {
			problems = append(problems, fmt.Sprintf("profile run %d = %q, want %q", index, run.profile, profiles[index].ID))
		}
		validateSourceFingerprints(fmt.Sprintf("profile %q", run.profile), run.files, &problems)
		for _, site := range run.sites {
			if site.Profile != run.profile {
				problems = append(problems, fmt.Sprintf("profile %q contains site labeled %q", run.profile, site.Profile))
			}
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return sourceScopeManifest{}, fmt.Errorf("invalid source-scope manifest:\n- %s", strings.Join(compactStrings(problems), "\n- "))
	}
	manifest.digest = deriveSourceScopeDigest(manifest)
	return manifest, nil
}

func verifySourceScopeManifest(repoRoot string, manifest sourceScopeManifest) error {
	if deriveSourceScopeDigest(manifest) != manifest.digest {
		return fmt.Errorf("source scope drift: manifest digest changed during analysis")
	}
	if manifest.audit.candidates != nil {
		candidates, err := sourceCandidates(repoRoot, manifest.auditRoots)
		if err != nil {
			return err
		}
		if !equalStrings(candidates, manifest.audit.candidates) {
			return fmt.Errorf("source scope drift: production source census changed during analysis")
		}
	}

	expected := make(map[string]string)
	var problems []string
	add := func(files []sourceFingerprint) {
		for _, file := range files {
			if previous, exists := expected[file.path]; exists && previous != file.digest {
				problems = append(problems, fmt.Sprintf("%s had conflicting content digests", file.path))
				continue
			}
			expected[file.path] = file.digest
		}
	}
	add(manifest.audit.files)
	for _, run := range manifest.runs {
		add(run.files)
	}
	paths := make([]string, 0, len(expected))
	for path := range expected {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	actual, err := fingerprintSourcePaths(repoRoot, paths)
	if err != nil {
		return fmt.Errorf("source scope drift: %w", err)
	}
	for _, file := range actual {
		if file.digest != expected[file.path] {
			problems = append(problems, fmt.Sprintf("%s changed during analysis", file.path))
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		return fmt.Errorf("source scope drift:\n- %s", strings.Join(compactStrings(problems), "\n- "))
	}
	return nil
}

func fingerprintSourcePaths(repoRoot string, paths []string) ([]sourceFingerprint, error) {
	paths = append([]string(nil), paths...)
	sort.Strings(paths)
	paths = compactStrings(paths)
	result := make([]sourceFingerprint, 0, len(paths))
	for _, relative := range paths {
		if !cleanRepoPath(relative) {
			return nil, fmt.Errorf("source-scope file %q is not a clean repository-relative path", relative)
		}
		filename := filepath.Join(repoRoot, filepath.FromSlash(relative))
		info, err := os.Lstat(filename)
		if err != nil {
			return nil, fmt.Errorf("fingerprinting %s: %s", relative, stableFilesystemError(err))
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("fingerprinting %s: symbolic links are forbidden", relative)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("fingerprinting %s: source is not a regular file", relative)
		}
		content, err := os.ReadFile(filename)
		if err != nil {
			return nil, fmt.Errorf("fingerprinting %s: %s", relative, stableFilesystemError(err))
		}
		digest := sha256.Sum256(content)
		result = append(result, sourceFingerprint{path: relative, digest: fmt.Sprintf("%x", digest)})
	}
	return result, nil
}

func validateSourceFingerprints(scope string, files []sourceFingerprint, problems *[]string) {
	if len(files) == 0 {
		*problems = append(*problems, scope+" has no source files")
		return
	}
	previous := ""
	for _, file := range files {
		if !cleanRepoPath(file.path) {
			*problems = append(*problems, fmt.Sprintf("%s file %q is not a clean repository-relative path", scope, file.path))
		}
		if !lowerHex(file.digest, 64) {
			*problems = append(*problems, fmt.Sprintf("%s file %q has invalid content digest", scope, file.path))
		}
		if file.path <= previous {
			*problems = append(*problems, fmt.Sprintf("%s source files are not strictly sorted by path", scope))
		}
		previous = file.path
	}
}

func validateSourcePaths(scope string, paths []string, problems *[]string) {
	previous := ""
	for _, path := range paths {
		if !cleanRepoPath(path) {
			*problems = append(*problems, fmt.Sprintf("%s path %q is not a clean repository-relative path", scope, path))
		}
		if path <= previous {
			*problems = append(*problems, scope+" are not strictly sorted")
		}
		previous = path
	}
}

func deriveSourceScopeDigest(manifest sourceScopeManifest) string {
	auditFiles := make([]string, len(manifest.audit.files))
	for index, file := range manifest.audit.files {
		auditFiles[index] = canonicalSourceFingerprint(file)
	}
	profileRecords := make([]string, len(manifest.runs))
	for index, run := range manifest.runs {
		files := make([]string, len(run.files))
		for fileIndex, file := range run.files {
			files[fileIndex] = canonicalSourceFingerprint(file)
		}
		profileRecords[index] = canonicalFields(
			"source-profile-v1",
			string(run.profile),
			canonicalStringList("source-files-v1", files),
		)
	}
	return deriveContentID(sourceScopeDigestPrefix, canonicalFields(
		"source-scope-v1",
		manifest.modulePath,
		canonicalStringList("analysis-patterns-v1", append([]string(nil), manifest.patterns...)),
		canonicalStringList("source-audit-roots-v1", append([]string(nil), manifest.auditRoots...)),
		canonicalStringList("source-audit-candidates-v1", append([]string(nil), manifest.audit.candidates...)),
		canonicalStringList("source-audit-files-v1", auditFiles),
		canonicalStringList("source-profiles-v1", profileRecords),
	))
}

func canonicalSourceFingerprint(file sourceFingerprint) string {
	return canonicalFields("source-file-v1", file.path, file.digest)
}

func equalSourceFingerprints(left, right []sourceFingerprint) bool {
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

func cloneCanonicalProfileRuns(runs []canonicalProfileRun) []canonicalProfileRun {
	result := make([]canonicalProfileRun, len(runs))
	for index, run := range runs {
		result[index] = canonicalProfileRun{
			profile: run.profile,
			files:   append([]sourceFingerprint(nil), run.files...),
		}
		result[index].sites = cloneProfileDiscoveries([]profileDiscovery{{profile: run.profile, sites: run.sites}})[0].sites
	}
	return result
}
