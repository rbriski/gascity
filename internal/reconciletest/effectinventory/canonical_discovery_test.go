package effectinventory

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"
)

const canonicalTestRevision = "91597432ce9b44d6dbc0a6530b198df1d6d1f113"

func TestCompileCanonicalRegistryRunsOnlyFixedScopeAndReturnsStructuredReport(t *testing.T) {
	var gitProbes int
	var profiles []analysisProfile
	runtime := canonicalCompileRuntime{
		probeGit: func(context.Context, string) (gitSnapshot, error) {
			gitProbes++
			return cleanGitSnapshot(canonicalTestRevision), nil
		},
		auditSources: func(_ context.Context, config sourceSelectionConfig) (sourceAudit, error) {
			if config.RepoRoot != "/repo" || config.ModulePath != canonicalModulePath {
				t.Fatalf("source audit config = %#v", config)
			}
			if !reflect.DeepEqual(config.Roots, canonicalSourceAuditRoots()) {
				t.Fatalf("source audit roots = %q, want canonical roots", config.Roots)
			}
			return sourceAudit{files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}}, nil
		},
		runProfile: func(_ context.Context, config analysisConfig, profile analysisProfile, boundaries []BoundaryDefinition) (canonicalProfileRun, error) {
			profiles = append(profiles, profile)
			if config.RepoRoot != "/repo" || config.ModulePath != canonicalModulePath {
				t.Fatalf("analysis config = %#v", config)
			}
			if !reflect.DeepEqual(config.Patterns, []string{"./cmd/gc"}) {
				t.Fatalf("analysis patterns = %q, want only ./cmd/gc", config.Patterns)
			}
			if got, want := deriveBoundaryDigest(boundaries), deriveBoundaryDigest(CanonicalBoundaries()); got != want {
				t.Fatalf("boundary digest = %q, want %q", got, want)
			}

			run := canonicalProfileRun{
				profile: profile.ID,
				files:   []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")},
			}
			if profile.ID == BuildLinuxDefault {
				run.files = append(run.files, fixtureFingerprint("internal/api/huma_handlers_services.go"))
				run.sites = []ObservedSite{apiRestartObservedSite(profile.ID)}
			}
			return run, nil
		},
		verifyScope: func(string, sourceScopeManifest) error { return nil },
	}

	compiled, report, err := CompileCanonicalRegistry(context.Background(), CanonicalCompileRequest{
		RepoRoot:            "/repo",
		ExpectedGitRevision: canonicalTestRevision,
		Registry: Registry{
			Boundaries: CanonicalBoundaries(),
		},
		AsOf:        time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		testRuntime: &runtime,
	})
	if err == nil || !strings.Contains(err.Error(), "registry has no site registrations") {
		t.Fatalf("CompileCanonicalRegistry() error = %v, want catalog completeness diagnostic", err)
	}
	if !reflect.DeepEqual(compiled, CompiledRegistry{}) {
		t.Fatalf("compiled registry = %#v, want zero on catalog error", compiled)
	}
	if gitProbes != 2 {
		t.Fatalf("git probes = %d, want pre/post probes", gitProbes)
	}
	if !reflect.DeepEqual(profiles, canonicalAnalysisProfiles()) {
		t.Fatalf("profile runs = %#v, want exactly the five canonical profiles", profiles)
	}
	if report.GitRevision != canonicalTestRevision || report.GitHeadIdentity == "" {
		t.Fatalf("report git provenance = %#v", report)
	}
	if !contentDigest(report.BoundaryDigest, boundaryDigestPrefix) || !contentDigest(report.SourceScopeDigest, sourceScopeDigestPrefix) {
		t.Fatalf("report digests = boundary %q scope %q", report.BoundaryDigest, report.SourceScopeDigest)
	}
	if !contentDigest(report.EvidenceDigest, discoveryEvidencePrefix) {
		t.Fatalf("report evidence digest = %q, want canonical combined digest", report.EvidenceDigest)
	}
	if len(report.Profiles) != len(canonicalAnalysisProfiles()) {
		t.Fatalf("report profiles = %d, want %d", len(report.Profiles), len(canonicalAnalysisProfiles()))
	}
	if !reportContainsSite(report, apiRestartObservedSite(BuildLinuxDefault)) {
		t.Fatal("structured report does not contain the direct API workspace Restart site")
	}
	if reportContainsText(report, "runtimetest") {
		t.Fatal("structured report contains the test-only runtime/runtimetest package")
	}
}

func TestCompileCanonicalRegistryFreezesCallerRegistryBeforeAnalysis(t *testing.T) {
	registry := Registry{Boundaries: CanonicalBoundaries()}
	runtime := successfulCanonicalRuntime()
	runtime.auditSources = func(context.Context, sourceSelectionConfig) (sourceAudit, error) {
		registry.Boundaries[0].ID = "caller-mutated-after-entry"
		registry.Registrations = append(registry.Registrations, SiteRegistration{BoundaryID: "caller-mutated-after-entry"})
		return sourceAudit{files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}}, nil
	}
	request := canonicalCompileFixtureRequest(&runtime)
	request.Registry = registry

	_, report, err := CompileCanonicalRegistry(context.Background(), request)
	if err == nil || !strings.Contains(err.Error(), "registry has no site registrations") {
		t.Fatalf("CompileCanonicalRegistry() error = %v, want frozen empty-catalog diagnostic", err)
	}
	if strings.Contains(err.Error(), "caller-mutated-after-entry") {
		t.Fatalf("CompileCanonicalRegistry() observed caller mutation: %v", err)
	}
	if report.BoundaryDigest != deriveBoundaryDigest(CanonicalBoundaries()) {
		t.Fatalf("report boundary digest = %q after caller mutation", report.BoundaryDigest)
	}
}

func TestCloneRegistryDoesNotAliasNestedCatalogState(t *testing.T) {
	registry := compileRegistryFixture()
	registry.Registrations[0].Matcher.Enclosing.ClosurePath = []int{1}
	frozen := cloneRegistry(registry)
	want := cloneRegistry(registry)

	registry.Boundaries[0].ID = "changed"
	registry.Registrations[0].Matcher.Enclosing.ClosurePath[0] = 9
	registry.Registrations[0].Cases[0].BuildProfiles[0] = BuildLinuxDefault
	registry.Registrations[0].Cases[0].Routes[0].Disposition.Gates[0] = "changed"
	registry.Registrations[0].Cases[0].Routes[0].Exception.RemovalTasks[0] = "changed"
	if !reflect.DeepEqual(frozen, want) {
		t.Fatalf("cloneRegistry() retained caller aliases\n got: %#v\nwant: %#v", frozen, want)
	}
}

func TestCompileCanonicalRegistryFailsClosedBeforeReturningPartialEvidence(t *testing.T) {
	tests := []struct {
		name    string
		prepare func(*canonicalCompileRuntime)
		want    string
	}{
		{
			name: "dirty repository",
			prepare: func(runtime *canonicalCompileRuntime) {
				runtime.probeGit = func(context.Context, string) (gitSnapshot, error) {
					snapshot := cleanGitSnapshot(canonicalTestRevision)
					snapshot.dirty = true
					return snapshot, nil
				}
			},
			want: "repository is dirty",
		},
		{
			name: "revision mismatch",
			prepare: func(runtime *canonicalCompileRuntime) {
				runtime.probeGit = func(context.Context, string) (gitSnapshot, error) {
					return cleanGitSnapshot("a1597432ce9b44d6dbc0a6530b198df1d6d1f113"), nil
				}
			},
			want: "does not match expected revision",
		},
		{
			name: "source audit drift",
			prepare: func(runtime *canonicalCompileRuntime) {
				runtime.auditSources = func(context.Context, sourceSelectionConfig) (sourceAudit, error) {
					return sourceAudit{}, errors.New("source-selection audit failed: unselected production file")
				}
			},
			want: "source-selection audit failed",
		},
		{
			name: "discovery diagnostic",
			prepare: func(runtime *canonicalCompileRuntime) {
				calls := 0
				runtime.runProfile = func(context.Context, analysisConfig, analysisProfile, []BoundaryDefinition) (canonicalProfileRun, error) {
					calls++
					if calls == 3 {
						return canonicalProfileRun{}, errors.New("effect discovery failed: unresolved dynamic call")
					}
					profile := canonicalAnalysisProfiles()[calls-1]
					return canonicalProfileRun{profile: profile.ID, files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}}, nil
				}
			},
			want: "effect discovery failed",
		},
		{
			name: "cancellation after a partial run",
			prepare: func(runtime *canonicalCompileRuntime) {
				calls := 0
				runtime.runProfile = func(_ context.Context, _ analysisConfig, profile analysisProfile, _ []BoundaryDefinition) (canonicalProfileRun, error) {
					calls++
					if calls == 2 {
						return canonicalProfileRun{}, context.Canceled
					}
					return canonicalProfileRun{profile: profile.ID, files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}}, nil
				}
			},
			want: "context canceled",
		},
		{
			name: "profile identity mismatch",
			prepare: func(runtime *canonicalCompileRuntime) {
				runtime.runProfile = func(_ context.Context, _ analysisConfig, profile analysisProfile, _ []BoundaryDefinition) (canonicalProfileRun, error) {
					return canonicalProfileRun{profile: BuildProfileID("wrong/partial"), files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}}, nil
				}
			},
			want: "returned profile",
		},
		{
			name: "source content changed",
			prepare: func(runtime *canonicalCompileRuntime) {
				runtime.verifyScope = func(string, sourceScopeManifest) error {
					return errors.New("source scope drift: cmd/gc/main.go changed during analysis")
				}
			},
			want: "source scope drift",
		},
		{
			name: "head identity changed",
			prepare: func(runtime *canonicalCompileRuntime) {
				calls := 0
				runtime.probeGit = func(context.Context, string) (gitSnapshot, error) {
					calls++
					snapshot := cleanGitSnapshot(canonicalTestRevision)
					if calls == 2 {
						snapshot.headIdentity = "detached@" + canonicalTestRevision
					}
					return snapshot, nil
				}
			},
			want: "git HEAD identity changed",
		},
		{
			name: "revision changed",
			prepare: func(runtime *canonicalCompileRuntime) {
				calls := 0
				runtime.probeGit = func(context.Context, string) (gitSnapshot, error) {
					calls++
					if calls == 2 {
						return cleanGitSnapshot("a1597432ce9b44d6dbc0a6530b198df1d6d1f113"), nil
					}
					return cleanGitSnapshot(canonicalTestRevision), nil
				}
			},
			want: "git HEAD revision changed",
		},
		{
			name: "repository became dirty",
			prepare: func(runtime *canonicalCompileRuntime) {
				calls := 0
				runtime.probeGit = func(context.Context, string) (gitSnapshot, error) {
					calls++
					snapshot := cleanGitSnapshot(canonicalTestRevision)
					snapshot.dirty = calls == 2
					return snapshot, nil
				}
			},
			want: "repository became dirty",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			runtime := successfulCanonicalRuntime()
			tt.prepare(&runtime)
			compiled, report, err := CompileCanonicalRegistry(context.Background(), canonicalCompileFixtureRequest(&runtime))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("CompileCanonicalRegistry() error = %v, want %q", err, tt.want)
			}
			if !reflect.DeepEqual(compiled, CompiledRegistry{}) || !reflect.DeepEqual(report, CanonicalDiscoveryReport{}) {
				t.Fatalf("failure returned partial output: compiled=%#v report=%#v", compiled, report)
			}
		})
	}
}

func TestCompileCanonicalRegistryRejectsCancellationWithoutStartingWork(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	runtime := successfulCanonicalRuntime()
	runtime.probeGit = func(context.Context, string) (gitSnapshot, error) {
		t.Fatal("git probe ran after cancellation")
		return gitSnapshot{}, nil
	}

	_, report, err := CompileCanonicalRegistry(ctx, canonicalCompileFixtureRequest(&runtime))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("CompileCanonicalRegistry() error = %v, want context.Canceled", err)
	}
	if !reflect.DeepEqual(report, CanonicalDiscoveryReport{}) {
		t.Fatalf("canceled compile returned report %#v", report)
	}
}

func TestDeterministicGitEnvironmentScrubsInheritedRepositorySelectors(t *testing.T) {
	inherited := []string{
		"PATH=/usr/bin",
		"HOME=/home/tester",
		"GIT_DIR=/attacker/repository",
		"GIT_WORK_TREE=/attacker/worktree",
		"GIT_INDEX_FILE=/attacker/index",
		"GIT_COMMON_DIR=/attacker/common",
		"GIT_OBJECT_DIRECTORY=/attacker/objects",
		"GIT_ALTERNATE_OBJECT_DIRECTORIES=/attacker/alternate",
		"GIT_CEILING_DIRECTORIES=/repo",
		"GIT_NAMESPACE=attacker",
		"GIT_CONFIG=/attacker/config",
		"GIT_CONFIG_GLOBAL=/attacker/global",
		"GIT_CONFIG_SYSTEM=/attacker/system",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=core.worktree",
		"GIT_CONFIG_VALUE_0=/attacker/worktree",
		"GIT_EXEC_PATH=/attacker/bin",
		"GIT_TRACE=/attacker/trace",
		"LANG=host-locale",
		"LC_ALL=host-locale",
	}

	environment := deterministicGitEnvironment(inherited)
	values := make(map[string][]string)
	for _, item := range environment {
		name, value, _ := strings.Cut(item, "=")
		values[name] = append(values[name], value)
	}
	for _, name := range []string{
		"GIT_DIR", "GIT_WORK_TREE", "GIT_INDEX_FILE", "GIT_COMMON_DIR",
		"GIT_OBJECT_DIRECTORY", "GIT_ALTERNATE_OBJECT_DIRECTORIES",
		"GIT_CEILING_DIRECTORIES", "GIT_NAMESPACE", "GIT_CONFIG",
		"GIT_CONFIG_COUNT", "GIT_CONFIG_KEY_0", "GIT_CONFIG_VALUE_0",
		"GIT_EXEC_PATH", "GIT_TRACE",
	} {
		if len(values[name]) != 0 {
			t.Errorf("hostile inherited %s survived as %q", name, values[name])
		}
	}
	want := map[string]string{
		"GIT_CONFIG_NOSYSTEM":    "1",
		"GIT_CONFIG_GLOBAL":      os.DevNull,
		"GIT_CONFIG_SYSTEM":      os.DevNull,
		"GIT_OPTIONAL_LOCKS":     "0",
		"GIT_TERMINAL_PROMPT":    "0",
		"GIT_NO_REPLACE_OBJECTS": "1",
		"GIT_ATTR_NOSYSTEM":      "1",
		"LANG":                   "C",
		"LC_ALL":                 "C",
	}
	for name, value := range want {
		if got := values[name]; !reflect.DeepEqual(got, []string{value}) {
			t.Errorf("deterministic environment %s = %q, want [%q]", name, got, value)
		}
	}
	if !reflect.DeepEqual(values["PATH"], []string{"/usr/bin"}) || !reflect.DeepEqual(values["HOME"], []string{"/home/tester"}) {
		t.Errorf("non-Git process environment was not preserved: PATH=%q HOME=%q", values["PATH"], values["HOME"])
	}
}

func successfulCanonicalRuntime() canonicalCompileRuntime {
	return canonicalCompileRuntime{
		probeGit: func(context.Context, string) (gitSnapshot, error) {
			return cleanGitSnapshot(canonicalTestRevision), nil
		},
		auditSources: func(context.Context, sourceSelectionConfig) (sourceAudit, error) {
			return sourceAudit{files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}}, nil
		},
		runProfile: func(_ context.Context, _ analysisConfig, profile analysisProfile, _ []BoundaryDefinition) (canonicalProfileRun, error) {
			return canonicalProfileRun{profile: profile.ID, files: []sourceFingerprint{fixtureFingerprint("cmd/gc/main.go")}}, nil
		},
		verifyScope: func(string, sourceScopeManifest) error { return nil },
	}
}

func canonicalCompileFixtureRequest(runtime *canonicalCompileRuntime) CanonicalCompileRequest {
	return CanonicalCompileRequest{
		RepoRoot:            "/repo",
		ExpectedGitRevision: canonicalTestRevision,
		Registry: Registry{
			Boundaries: CanonicalBoundaries(),
		},
		AsOf:        time.Date(2026, 7, 14, 0, 0, 0, 0, time.UTC),
		testRuntime: runtime,
	}
}

func cleanGitSnapshot(revision string) gitSnapshot {
	return gitSnapshot{
		repoRoot:     "/repo",
		revision:     revision,
		headIdentity: "ref:refs/heads/test@" + revision,
	}
}

func fixtureFingerprint(path string) sourceFingerprint {
	return sourceFingerprint{path: path, digest: strings.Repeat("a", 64)}
}

func apiRestartObservedSite(profile BuildProfileID) ObservedSite {
	return ObservedSite{
		BoundaryID: "workspacesvc.registry.Restart",
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
		Profile: profile,
	}
}

func reportContainsSite(report CanonicalDiscoveryReport, want ObservedSite) bool {
	wantKey := canonicalObservedSite(want)
	for _, profile := range report.Profiles {
		for _, site := range profile.Sites {
			if canonicalObservedSite(site) == wantKey {
				return true
			}
		}
	}
	return false
}

func reportContainsText(report CanonicalDiscoveryReport, fragment string) bool {
	for _, profile := range report.Profiles {
		for _, file := range profile.SourceFiles {
			if strings.Contains(file, fragment) {
				return true
			}
		}
		for _, site := range profile.Sites {
			if strings.Contains(canonicalObservedSite(site), fragment) {
				return true
			}
		}
	}
	return false
}
