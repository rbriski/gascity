package effectinventory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

const canonicalModulePath = "github.com/gastownhall/gascity"

// CanonicalCompileRequest pins one production inventory compilation to an
// exact clean Git revision. The analysis vocabulary, package root, build
// profiles, and Tests=false setting are intentionally not caller-configurable.
type CanonicalCompileRequest struct {
	RepoRoot            string
	ExpectedGitRevision string
	Registry            Registry
	AsOf                time.Time

	testRuntime *canonicalCompileRuntime
}

// CanonicalDiscoveryReport is the structured, non-authoritative observation
// report used to author and review registry entries. It cannot be passed to
// CompileRegistry as analyzer evidence.
type CanonicalDiscoveryReport struct {
	BoundaryDigest    string
	SourceScopeDigest string
	GitRevision       string
	GitHeadIdentity   string
	EvidenceDigest    string
	Profiles          []CanonicalProfileDiscovery
}

// CanonicalProfileDiscovery reports one completed canonical profile, including
// a valid zero-site result and its exact in-module dependency-closure files.
type CanonicalProfileDiscovery struct {
	Profile     BuildProfileID
	Sites       []ObservedSite
	SourceFiles []string
}

type canonicalCompileRuntime struct {
	probeGit     func(context.Context, string) (gitSnapshot, error)
	auditSources func(context.Context, sourceSelectionConfig) (sourceAudit, error)
	runProfile   func(context.Context, analysisConfig, analysisProfile, Registry) (canonicalProfileRun, error)
	verifyScope  func(string, sourceScopeManifest) error
}

type canonicalProfileRun struct {
	profile BuildProfileID
	sites   []ObservedSite
	files   []sourceFingerprint
}

// CompileCanonicalRegistry is the production effect-inventory entry point. It
// runs the five fixed Tests=false ./cmd/gc dependency-closure profiles and
// returns a complete structured discovery report even when catalog
// reconciliation fails. Analyzer, source-scope, cancellation, or VCS failures
// return no report, so partial discovery cannot be mistaken for complete.
func CompileCanonicalRegistry(ctx context.Context, request CanonicalCompileRequest) (CompiledRegistry, CanonicalDiscoveryReport, error) {
	if ctx == nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: context is required")
	}
	if err := ctx.Err(); err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: canceled before analysis: %w", err)
	}
	if strings.TrimSpace(request.RepoRoot) == "" {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: repository root is required")
	}
	if !lowerHex(request.ExpectedGitRevision, 40) {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: expected git revision must be 40 lowercase hexadecimal characters")
	}
	if request.AsOf.IsZero() {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: validation date is required")
	}

	registry := cloneRegistry(request.Registry)
	boundaries := CanonicalBoundaries()
	boundaryDigest := deriveBoundaryDigest(boundaries)
	if got := deriveBoundaryDigest(registry.Boundaries); got != boundaryDigest {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf(
			"canonical effect inventory: registry boundary digest %q does not match canonical digest %q",
			got,
			boundaryDigest,
		)
	}

	runtime := productionCanonicalCompileRuntime()
	if request.testRuntime != nil {
		runtime = runtime.withOverrides(*request.testRuntime)
	}
	pre, err := runtime.probeGit(ctx, request.RepoRoot)
	if err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, request.RepoRoot, "probing Git before analysis", err)
	}
	if err := validateInitialGitSnapshot(pre, request.RepoRoot, request.ExpectedGitRevision); err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, request.RepoRoot, "validating Git before analysis", err)
	}

	audit, err := runtime.auditSources(ctx, sourceSelectionConfig{
		RepoRoot:   pre.repoRoot,
		ModulePath: canonicalModulePath,
		Roots:      canonicalSourceAuditRoots(),
	})
	if err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, pre.repoRoot, "auditing source scope", err)
	}

	config := analysisConfig{
		RepoRoot:   pre.repoRoot,
		ModulePath: canonicalModulePath,
		Patterns:   canonicalProductionAnalysisPatterns(),
	}
	profiles := canonicalAnalysisProfiles()
	runs := make([]canonicalProfileRun, 0, len(profiles))
	for _, profile := range profiles {
		if err := ctx.Err(); err != nil {
			return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: canceled before profile %q: %w", profile.ID, err)
		}
		run, err := runtime.runProfile(ctx, config, profile, cloneRegistry(registry))
		if err != nil {
			return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, pre.repoRoot, fmt.Sprintf("discovering profile %q", profile.ID), err)
		}
		if run.profile != profile.ID {
			return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf(
				"canonical effect inventory: profile runner returned profile %q while running %q",
				run.profile,
				profile.ID,
			)
		}
		runs = append(runs, run)
	}
	if len(runs) != len(profiles) {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf(
			"canonical effect inventory: completed %d profiles, want %d",
			len(runs),
			len(profiles),
		)
	}
	if err := ctx.Err(); err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: canceled after profile discovery: %w", err)
	}

	manifest, err := buildSourceScopeManifest(config, canonicalSourceAuditRoots(), audit, runs)
	if err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, pre.repoRoot, "binding source scope", err)
	}
	if err := runtime.verifyScope(pre.repoRoot, manifest); err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, pre.repoRoot, "verifying source scope", err)
	}
	post, err := runtime.probeGit(ctx, pre.repoRoot)
	if err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, pre.repoRoot, "probing Git after analysis", err)
	}
	if err := validateFinalGitSnapshot(pre, post, request.ExpectedGitRevision); err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, canonicalStageError(ctx, pre.repoRoot, "validating Git after analysis", err)
	}
	if err := ctx.Err(); err != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: canceled before registry compilation: %w", err)
	}

	discoveries := make([]profileDiscovery, len(runs))
	for index, run := range runs {
		discoveries[index] = profileDiscovery{profile: run.profile, sites: run.sites}
	}
	discovery := newDiscoveryResult(
		boundaryDigest,
		manifest.digest,
		request.ExpectedGitRevision,
		post.revision,
		post.headIdentity,
		discoveries,
	)
	report := newCanonicalDiscoveryReport(discovery, runs)
	compiled, err := CompileRegistry(registry, discovery, request.AsOf)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return CompiledRegistry{}, CanonicalDiscoveryReport{}, fmt.Errorf("canonical effect inventory: canceled during registry compilation: %w", ctxErr)
	}
	if err != nil {
		return CompiledRegistry{}, report, canonicalStageError(ctx, pre.repoRoot, "compiling registry", err)
	}
	return compiled, report, nil
}

func productionCanonicalCompileRuntime() canonicalCompileRuntime {
	return canonicalCompileRuntime{
		probeGit:     probeGitRepository,
		auditSources: auditCanonicalSources,
		runProfile:   runCanonicalProfile,
		verifyScope:  verifySourceScopeManifest,
	}
}

func (runtime canonicalCompileRuntime) withOverrides(overrides canonicalCompileRuntime) canonicalCompileRuntime {
	if overrides.probeGit != nil {
		runtime.probeGit = overrides.probeGit
	}
	if overrides.auditSources != nil {
		runtime.auditSources = overrides.auditSources
	}
	if overrides.runProfile != nil {
		runtime.runProfile = overrides.runProfile
	}
	if overrides.verifyScope != nil {
		runtime.verifyScope = overrides.verifyScope
	}
	return runtime
}

func validateInitialGitSnapshot(snapshot gitSnapshot, requestedRoot, expectedRevision string) error {
	if strings.TrimSpace(snapshot.repoRoot) == "" {
		return fmt.Errorf("git repository root is missing")
	}
	if !sameFilesystemPath(snapshot.repoRoot, requestedRoot) {
		return fmt.Errorf("requested repository root is not the exact Git worktree root")
	}
	if snapshot.dirty {
		return fmt.Errorf("repository is dirty")
	}
	if snapshot.revision != expectedRevision {
		return fmt.Errorf("git HEAD revision %q does not match expected revision %q", snapshot.revision, expectedRevision)
	}
	if strings.TrimSpace(snapshot.headIdentity) == "" {
		return fmt.Errorf("git HEAD identity is missing")
	}
	return nil
}

func validateFinalGitSnapshot(before, after gitSnapshot, expectedRevision string) error {
	if !sameFilesystemPath(before.repoRoot, after.repoRoot) {
		return fmt.Errorf("git worktree root changed during canonical analysis")
	}
	if after.dirty {
		return fmt.Errorf("repository became dirty during canonical analysis")
	}
	if after.revision != expectedRevision || after.revision != before.revision {
		return fmt.Errorf(
			"git HEAD revision changed during canonical analysis: before %q after %q expected %q",
			before.revision,
			after.revision,
			expectedRevision,
		)
	}
	if after.headIdentity != before.headIdentity {
		return fmt.Errorf(
			"git HEAD identity changed during canonical analysis: before %q after %q",
			before.headIdentity,
			after.headIdentity,
		)
	}
	return nil
}

func sameFilesystemPath(left, right string) bool {
	leftAbsolute, leftErr := filepath.Abs(left)
	rightAbsolute, rightErr := filepath.Abs(right)
	if leftErr != nil || rightErr != nil {
		return false
	}
	if filepath.Clean(leftAbsolute) == filepath.Clean(rightAbsolute) {
		return true
	}
	leftInfo, leftErr := os.Stat(leftAbsolute)
	rightInfo, rightErr := os.Stat(rightAbsolute)
	return leftErr == nil && rightErr == nil && os.SameFile(leftInfo, rightInfo)
}

func canonicalStageError(ctx context.Context, repoRoot, stage string, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("canonical effect inventory: %s: %w", stage, ctxErr)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return fmt.Errorf("canonical effect inventory: %s: %w", stage, err)
	}
	return fmt.Errorf("canonical effect inventory: %s: %s", stage, stableDiagnosticText(repoRoot, err.Error()))
}

func newCanonicalDiscoveryReport(discovery DiscoveryResult, runs []canonicalProfileRun) CanonicalDiscoveryReport {
	report := CanonicalDiscoveryReport{
		BoundaryDigest:    discovery.boundaryDigest,
		SourceScopeDigest: discovery.sourceScopeDigest,
		GitRevision:       discovery.observedGitRevision,
		GitHeadIdentity:   discovery.gitHeadIdentity,
		EvidenceDigest:    discovery.evidenceDigest,
		Profiles:          make([]CanonicalProfileDiscovery, len(runs)),
	}
	for index, run := range runs {
		sites := cloneProfileDiscoveries([]profileDiscovery{{profile: run.profile, sites: run.sites}})[0].sites
		sort.Slice(sites, func(i, j int) bool {
			return canonicalObservedSite(sites[i]) < canonicalObservedSite(sites[j])
		})
		files := make([]string, len(run.files))
		for fileIndex, file := range run.files {
			files[fileIndex] = file.path
		}
		sort.Strings(files)
		report.Profiles[index] = CanonicalProfileDiscovery{
			Profile:     run.profile,
			Sites:       sites,
			SourceFiles: files,
		}
	}
	return report
}

func cloneRegistry(registry Registry) Registry {
	result := Registry{
		Boundaries:    append([]BoundaryDefinition(nil), registry.Boundaries...),
		Registrations: make([]SiteRegistration, len(registry.Registrations)),
	}
	for index, registration := range registry.Registrations {
		result.Registrations[index] = SiteRegistration{
			BoundaryID: registration.BoundaryID,
			Matcher:    cloneOperationSite(registration.Matcher),
			Cases:      make([]ProfileCase, len(registration.Cases)),
		}
		for caseIndex, profileCase := range registration.Cases {
			result.Registrations[index].Cases[caseIndex] = ProfileCase{
				BuildProfiles: append([]BuildProfileID(nil), profileCase.BuildProfiles...),
				Routes:        make([]Route, len(profileCase.Routes)),
			}
			for routeIndex, route := range profileCase.Routes {
				result.Registrations[index].Cases[caseIndex].Routes[routeIndex] = cloneRoute(route)
			}
		}
	}
	return result
}
