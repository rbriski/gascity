package effectinventory

import (
	"context"
	"go/ast"
	"go/build"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestSourceSelectionCanonicalRootsAndPatterns(t *testing.T) {
	wantRoots := []string{
		"cmd/gc",
		"internal/session",
		"internal/worker",
		"internal/runtime",
	}
	wantPatterns := []string{
		"./cmd/gc/...",
		"./internal/session/...",
		"./internal/worker/...",
		"./internal/runtime/...",
	}

	roots := canonicalAnalysisRoots()
	if !reflect.DeepEqual(roots, wantRoots) {
		t.Fatalf("canonicalAnalysisRoots() = %#v, want %#v", roots, wantRoots)
	}
	patterns, err := canonicalSourcePatterns(roots)
	if err != nil {
		t.Fatalf("canonicalSourcePatterns() error: %v", err)
	}
	if !reflect.DeepEqual(patterns, wantPatterns) {
		t.Fatalf("canonicalSourcePatterns() = %#v, want %#v", patterns, wantPatterns)
	}

	// Neither helper may expose mutable package state or retain caller state.
	roots[0] = "mutated"
	patterns[0] = "mutated"
	if again := canonicalAnalysisRoots(); !reflect.DeepEqual(again, wantRoots) {
		t.Fatalf("canonicalAnalysisRoots() retained caller mutation: %#v", again)
	}
	ordered := []string{"zeta", "alpha/nested"}
	got, err := canonicalSourcePatterns(ordered)
	if err != nil {
		t.Fatalf("canonicalSourcePatterns(arbitrary roots) error: %v", err)
	}
	ordered[0] = "mutated"
	if want := []string{"./zeta/...", "./alpha/nested/..."}; !reflect.DeepEqual(got, want) {
		t.Fatalf("canonicalSourcePatterns(arbitrary roots) = %#v, want %#v", got, want)
	}
}

func TestSourceSelectionPatternsRejectInvalidRoots(t *testing.T) {
	tests := []struct {
		name  string
		roots []string
	}{
		{name: "empty set"},
		{name: "empty root", roots: []string{""}},
		{name: "absolute", roots: []string{"/scope"}},
		{name: "windows drive absolute", roots: []string{"C:/scope"}},
		{name: "windows drive relative", roots: []string{"C:scope"}},
		{name: "windows bare drive", roots: []string{"C:"}},
		{name: "parent traversal", roots: []string{"../scope"}},
		{name: "embedded traversal", roots: []string{"scope/../other"}},
		{name: "current directory", roots: []string{"."}},
		{name: "leading dot slash", roots: []string{"./scope"}},
		{name: "trailing slash", roots: []string{"scope/"}},
		{name: "unclean separator", roots: []string{"scope//nested"}},
		{name: "recursive wildcard", roots: []string{"scope/..."}},
		{name: "other wildcard", roots: []string{"scope/*"}},
		{name: "duplicate", roots: []string{"scope", "scope"}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if patterns, err := canonicalSourcePatterns(tt.roots); err == nil {
				t.Fatalf("canonicalSourcePatterns(%q) = %#v, want error", tt.roots, patterns)
			}
		})
	}
}

func TestSourceSelectionCandidatesFollowGoFilesystemScope(t *testing.T) {
	repoRoot := t.TempDir()
	files := map[string]string{
		"scope/direct.go":                          "package scope\n",
		"scope/nested/child.go":                    "package nested\n",
		"outside/outside.go":                       "package outside\n",
		"scope/direct_test.go":                     "package scope\n",
		"scope/.ignored.go":                        "package scope\n",
		"scope/_ignored.go":                        "package scope\n",
		"scope/testdata/fixture.go":                "package fixture\n",
		"scope/nested/testdata/deeper.go":          "package fixture\n",
		"scope/.hidden/hidden.go":                  "package hidden\n",
		"scope/_generated/generated.go":            "package generated\n",
		"scope/nested/.state/hidden.go":            "package hidden\n",
		"scope/nested/_scratch/scratch.go":         "package scratch\n",
		"scope/vendor/direct.go":                   "package vendor\n",
		"scope/vendor/dependency/deeper.go":        "package dependency\n",
		"scope/nested/vendor/direct.go":            "package vendor\n",
		"scope/nested/vendor/dependency/deeper.go": "package dependency\n",
		"scope/nested/not-go.txt":                  "package nested\n",
		"scope/nested/almost.go.generated":         "package nested\n",
		"scope/nested/also_test.go.generated":      "package nested\n",
	}
	for name, content := range files {
		writeSourceSelectionFile(t, repoRoot, name, content)
	}

	got, err := sourceCandidates(repoRoot, []string{"scope"})
	if err != nil {
		t.Fatalf("sourceCandidates() error: %v", err)
	}
	want := []string{
		"scope/direct.go",
		"scope/nested/child.go",
		"scope/nested/vendor/direct.go",
		"scope/vendor/direct.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sourceCandidates() = %#v, want %#v", got, want)
	}
}

func TestSourceSelectionImplicitArchitectureConstraint(t *testing.T) {
	tests := []struct {
		name     string
		path     string
		wantArch string
		want     bool
	}{
		{name: "architecture", path: "scope/value_amd64.go", wantArch: "amd64", want: true},
		{name: "os and architecture", path: "scope/value_linux_arm64.go", wantArch: "arm64", want: true},
		{name: "test suffix is stripped", path: "scope/value_amd64_test.go", wantArch: "amd64", want: true},
		{name: "first dot terminates name", path: "scope/value_amd64.go.generated", wantArch: "amd64", want: true},
		{name: "less common known architecture", path: "scope/value_linux_amd64p32.go", wantArch: "amd64p32", want: true},
		{name: "os only", path: "scope/value_linux.go"},
		{name: "architecture without separator", path: "scope/amd64.go"},
		{name: "empty prefix", path: "scope/_amd64.go"},
		{name: "architecture after first dot", path: "scope/value.amd64.go"},
		{name: "tag merely contains architecture name", path: "scope/value_featureamd64.go"},
		{name: "unknown suffix", path: "scope/value_quantum64.go"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arch, got := implicitArchitectureConstraint(tt.path)
			if arch != tt.wantArch || got != tt.want {
				t.Fatalf("implicitArchitectureConstraint(%q) = (%q, %t), want (%q, %t)", tt.path, arch, got, tt.wantArch, tt.want)
			}
		})
	}
}

func TestSourceSelectionKnownGOARCHMatchesPinnedToolchain(t *testing.T) {
	filename := filepath.Join(build.Default.GOROOT, "src", "internal", "syslist", "syslist.go")
	file, err := parser.ParseFile(token.NewFileSet(), filename, nil, 0)
	if err != nil {
		t.Fatalf("parsing pinned toolchain architecture list: %v", err)
	}

	var toolchain []string
	ast.Inspect(file, func(node ast.Node) bool {
		valueSpec, ok := node.(*ast.ValueSpec)
		if !ok {
			return true
		}
		for index, name := range valueSpec.Names {
			if name.Name != "KnownArch" || index >= len(valueSpec.Values) {
				continue
			}
			literal, ok := valueSpec.Values[index].(*ast.CompositeLit)
			if !ok {
				t.Fatalf("internal/syslist.KnownArch is %T, want composite literal", valueSpec.Values[index])
			}
			for _, element := range literal.Elts {
				keyValue, ok := element.(*ast.KeyValueExpr)
				if !ok {
					t.Fatalf("internal/syslist.KnownArch element is %T, want key-value expression", element)
				}
				key, ok := keyValue.Key.(*ast.BasicLit)
				if !ok || key.Kind != token.STRING {
					t.Fatalf("internal/syslist.KnownArch key is %T, want string literal", keyValue.Key)
				}
				architecture, err := strconv.Unquote(key.Value)
				if err != nil {
					t.Fatalf("unquoting internal/syslist.KnownArch key %s: %v", key.Value, err)
				}
				toolchain = append(toolchain, architecture)
			}
		}
		return true
	})
	if len(toolchain) == 0 {
		t.Fatal("pinned toolchain internal/syslist.KnownArch was not found")
	}

	want := canonicalGOARCHs()
	sort.Strings(toolchain)
	sort.Strings(want)
	if !reflect.DeepEqual(want, toolchain) {
		t.Fatalf("canonical GOARCH set = %q, pinned toolchain = %q", want, toolchain)
	}
}

func TestSourceSelectionExplicitArchitectureConstraint(t *testing.T) {
	tooComplexLegacy := "// +build " + strings.Repeat("feature ", 101) + "amd64\n\npackage fixture\n"
	tests := []struct {
		name     string
		content  string
		wantArch string
		want     bool
	}{
		{
			name:     "modern architecture",
			content:  "//go:build amd64\n\npackage fixture\n",
			wantArch: "amd64",
			want:     true,
		},
		{
			name:     "modern negated architecture",
			content:  "//go:build linux && !arm64\n\npackage fixture\n",
			wantArch: "arm64",
			want:     true,
		},
		{
			name:     "modern nested expression",
			content:  "//go:build linux && (gascity_native_beads || riscv64)\n\npackage fixture\n",
			wantArch: "riscv64",
			want:     true,
		},
		{
			name:     "amd64 feature level one",
			content:  "//go:build amd64.v1\n\npackage fixture\n",
			wantArch: "amd64.v1",
			want:     true,
		},
		{
			name:     "amd64 feature level two",
			content:  "//go:build !amd64.v2\n\npackage fixture\n",
			wantArch: "amd64.v2",
			want:     true,
		},
		{
			name:     "arm64 feature level",
			content:  "//go:build arm64.v8.0\n\npackage fixture\n",
			wantArch: "arm64.v8.0",
			want:     true,
		},
		{
			name:     "legacy architecture",
			content:  "// +build linux,amd64\n\npackage fixture\n",
			wantArch: "amd64",
			want:     true,
		},
		{
			name:     "leading byte order mark",
			content:  "\xef\xbb\xbf//go:build wasm\n\npackage fixture\n",
			wantArch: "wasm",
			want:     true,
		},
		{
			name:    "feature tag",
			content: "//go:build gascity_native_beads\n\npackage fixture\n",
		},
		{
			name:    "os tag",
			content: "//go:build linux || darwin\n\npackage fixture\n",
		},
		{
			name:    "structural match not substring",
			content: "//go:build feature_amd64\n\npackage fixture\n",
		},
		{
			name:    "ordinary comment",
			content: "// amd64 is mentioned in prose\n\npackage fixture\n",
		},
		{
			name:    "modern supersedes legacy",
			content: "//go:build linux\n// +build amd64\n\npackage fixture\n",
		},
		{
			name:     "modern does not need legacy blank separator",
			content:  "//go:build arm64\npackage fixture\n",
			wantArch: "arm64",
			want:     true,
		},
		{
			name:    "legacy needs blank separator",
			content: "// +build arm64\npackage fixture\n",
		},
		{
			name:    "modern after package is misplaced",
			content: "package fixture\n\n//go:build amd64\n",
		},
		{
			name:    "modern inside block comment is not a directive",
			content: "/*\n//go:build amd64\n*/\npackage fixture\n",
		},
		{
			name:    "invalid legacy literal is normalized away",
			content: "// +build (\n\npackage fixture\n",
		},
		{
			name:    "legacy parse failure is ignored",
			content: tooComplexLegacy,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arch, got, err := explicitArchitectureConstraint([]byte(tt.content))
			if err != nil {
				t.Fatalf("explicitArchitectureConstraint() error: %v", err)
			}
			if arch != tt.wantArch || got != tt.want {
				t.Fatalf("explicitArchitectureConstraint() = (%q, %t), want (%q, %t)", arch, got, tt.wantArch, tt.want)
			}
		})
	}
}

func TestSourceSelectionExplicitArchitectureConstraintRejectsMalformedModernHeaders(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "malformed expression",
			content: "//go:build linux &&\n\npackage fixture\n",
		},
		{
			name:    "duplicate modern directives",
			content: "//go:build linux\n//go:build amd64\n\npackage fixture\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if arch, found, err := explicitArchitectureConstraint([]byte(tt.content)); err == nil {
				t.Fatalf("explicitArchitectureConstraint() = (%q, %t, nil), want error", arch, found)
			}
		})
	}
}

func TestSourceSelectionAuditAcceptsCanonicalProfileUnion(t *testing.T) {
	repoRoot, config := newSourceSelectionModule(t)
	files := map[string]string{
		"scope/common.go":                     "package scope\n\nfunc common() {}\n",
		"scope/platform_darwin.go":            "package scope\n\nfunc darwin() {}\n",
		"scope/platform_linux.go":             "package scope\n\nfunc linux() {}\n",
		"scope/platform_windows.go":           "package scope\n\nfunc windows() {}\n",
		"scope/default.go":                    "//go:build !gascity_native_beads\n\npackage scope\n\nfunc defaultStore() {}\n",
		"scope/native.go":                     "//go:build gascity_native_beads\n\npackage scope\n\nfunc nativeStore() {}\n",
		"scope/nativeonly/native.go":          "//go:build gascity_native_beads\n\npackage nativeonly\n\nfunc selected() {}\n",
		"scope/windowsonly/windows.go":        "//go:build windows\n\npackage windowsonly\n\nfunc selected() {}\n",
		"scope/darwinlinux/unix.go":           "//go:build darwin || linux\n\npackage darwinlinux\n\nfunc selected() {}\n",
		"scope/vendor/direct.go":              "package vendor\n\nfunc selected() {}\n",
		"scope/vendor/dependency/ignored.go":  "//go:build plan9\n\npackage dependency\n",
		"scope/testdata/unsupported_plan9.go": "//go:build plan9\n\npackage fixture\n",
	}
	for name, content := range files {
		writeSourceSelectionFile(t, repoRoot, name, content)
	}

	if err := auditCanonicalSourceSelection(context.Background(), config); err != nil {
		t.Fatalf("auditCanonicalSourceSelection() error: %v", err)
	}
}

func TestProductionSourceSelectionCoveredByCanonicalProfiles(t *testing.T) {
	analysis := fixtureAnalysisConfig(t, nil)
	config := sourceSelectionConfig{
		RepoRoot:   analysis.RepoRoot,
		ModulePath: analysis.ModulePath,
		Roots:      canonicalAnalysisRoots(),
	}

	if err := auditCanonicalSourceSelection(context.Background(), config); err != nil {
		t.Fatalf("production source selection is outside the canonical profiles: %v", err)
	}
}

func TestSourceSelectionAuditRejectsUnselectedFile(t *testing.T) {
	repoRoot, config := newSourceSelectionModule(t)
	writeSourceSelectionFile(t, repoRoot, "scope/common.go", "package scope\n")
	writeSourceSelectionFile(t, repoRoot, "scope/future_plan9.go", "//go:build plan9\n\npackage scope\n")

	err := auditCanonicalSourceSelection(context.Background(), config)
	requireSourceSelectionError(t, err, "scope/future_plan9.go", "not selected by any canonical analysis profile")
}

func TestSourceSelectionAuditRejectsWhollyUnselectedNestedPackage(t *testing.T) {
	repoRoot, config := newSourceSelectionModule(t)
	writeSourceSelectionFile(t, repoRoot, "scope/common.go", "package scope\n")
	writeSourceSelectionFile(t, repoRoot, "scope/unsupported/only_plan9.go", "//go:build plan9\n\npackage unsupported\n")

	err := auditCanonicalSourceSelection(context.Background(), config)
	requireSourceSelectionError(t, err, "scope/unsupported", "no source selected by any canonical analysis profile")
}

func TestSourceSelectionAuditRejectsSelectedAndUnselectedArchitectureFiles(t *testing.T) {
	tests := []struct {
		name string
		file string
		arch string
	}{
		{name: "selected amd64", file: "scope/value_amd64.go", arch: "amd64"},
		{name: "unselected arm64", file: "scope/value_arm64.go", arch: "arm64"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, config := newSourceSelectionModule(t)
			writeSourceSelectionFile(t, repoRoot, "scope/common.go", "package scope\n")
			writeSourceSelectionFile(t, repoRoot, tt.file, "package scope\n")

			err := auditCanonicalSourceSelection(context.Background(), config)
			requireSourceSelectionError(t, err, tt.file, "architecture-specific filename suffix \""+tt.arch+"\"")
		})
	}
}

func TestSourceSelectionAuditRejectsExplicitArchitectureHeaders(t *testing.T) {
	tests := []struct {
		name    string
		content string
		arch    string
	}{
		{
			name:    "selected modern amd64",
			content: "//go:build amd64\n\npackage scope\n",
			arch:    "amd64",
		},
		{
			name:    "unselected modern arm64",
			content: "//go:build arm64\n\npackage scope\n",
			arch:    "arm64",
		},
		{
			name:    "selected legacy amd64",
			content: "// +build amd64\n\npackage scope\n",
			arch:    "amd64",
		},
		{
			name:    "selected amd64 feature level",
			content: "//go:build amd64.v1\n\npackage scope\n",
			arch:    "amd64.v1",
		},
		{
			name:    "unselected amd64 feature level",
			content: "//go:build amd64.v2\n\npackage scope\n",
			arch:    "amd64.v2",
		},
		{
			name:    "unselected arm64 feature level",
			content: "//go:build arm64.v8.0\n\npackage scope\n",
			arch:    "arm64.v8.0",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, config := newSourceSelectionModule(t)
			writeSourceSelectionFile(t, repoRoot, "scope/common.go", "package scope\n")
			const constrainedFile = "scope/constrained.go"
			writeSourceSelectionFile(t, repoRoot, constrainedFile, tt.content)

			err := auditCanonicalSourceSelection(context.Background(), config)
			requireSourceSelectionError(t, err, constrainedFile, "references GOARCH \""+tt.arch+"\"")
		})
	}
}

func TestSourceSelectionAuditReportsMalformedHeadersWithRepoRelativePaths(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{
			name:    "malformed modern",
			content: "//go:build linux &&\n\npackage scope\n",
		},
		{
			name:    "duplicate modern",
			content: "//go:build linux\n//go:build amd64\n\npackage scope\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			repoRoot, config := newSourceSelectionModule(t)
			const malformedFile = "scope/malformed.go"
			writeSourceSelectionFile(t, repoRoot, malformedFile, tt.content)

			err := auditCanonicalSourceSelection(context.Background(), config)
			requireSourceSelectionError(t, err, malformedFile)
			if strings.Contains(err.Error(), filepath.ToSlash(repoRoot)) {
				t.Fatalf("audit error exposes temporary absolute path %q: %v", repoRoot, err)
			}
		})
	}
}

func TestSourceSelectionAuditDiagnosticsAreDeterministicAndSorted(t *testing.T) {
	repoRoot, config := newSourceSelectionModule(t)
	writeSourceSelectionFile(t, repoRoot, "scope/common.go", "package scope\n")
	writeSourceSelectionFile(t, repoRoot, "scope/z_plan9.go", "//go:build plan9\n\npackage scope\n")
	writeSourceSelectionFile(t, repoRoot, "scope/m_amd64.go", "package scope\n")
	writeSourceSelectionFile(t, repoRoot, "scope/a_arm64.go", "package scope\n")

	first := auditCanonicalSourceSelection(context.Background(), config)
	second := auditCanonicalSourceSelection(context.Background(), config)
	if first == nil || second == nil {
		t.Fatalf("auditCanonicalSourceSelection() errors = (%v, %v), want two errors", first, second)
	}
	if first.Error() != second.Error() {
		t.Fatalf("audit diagnostics are nondeterministic:\nfirst:  %v\nsecond: %v", first, second)
	}
	if strings.Contains(first.Error(), filepath.ToSlash(repoRoot)) {
		t.Fatalf("audit error exposes temporary absolute path %q: %v", repoRoot, first)
	}

	wantOrder := []string{"scope/a_arm64.go", "scope/m_amd64.go", "scope/z_plan9.go"}
	previous := -1
	for _, path := range wantOrder {
		index := strings.Index(first.Error(), path)
		if index < 0 {
			t.Fatalf("audit error %q does not mention %q", first, path)
		}
		if index <= previous {
			t.Fatalf("audit error paths are not sorted as %q: %v", wantOrder, first)
		}
		previous = index
	}
}

func TestSourceSelectionAuditFilesystemErrorsAreStableAndRepoRelative(t *testing.T) {
	var messages []string
	for range 2 {
		repoRoot := t.TempDir()
		config := sourceSelectionConfig{
			RepoRoot:   repoRoot,
			ModulePath: "example.com/sourceaudit",
			Roots:      []string{"missing"},
		}

		err := auditCanonicalSourceSelection(context.Background(), config)
		if err == nil {
			t.Fatal("auditCanonicalSourceSelection() error = nil, want missing-root error")
		}
		if strings.Contains(filepath.ToSlash(err.Error()), filepath.ToSlash(repoRoot)) {
			t.Fatalf("audit error exposes temporary absolute path %q: %v", repoRoot, err)
		}
		messages = append(messages, err.Error())
	}
	if messages[0] != messages[1] {
		t.Fatalf("equivalent filesystem errors differ by repository root:\nfirst:  %s\nsecond: %s", messages[0], messages[1])
	}
}

func TestSourceSelectionAuditSymlinkErrorIsRepoRelative(t *testing.T) {
	repoRoot, config := newSourceSelectionModule(t)
	writeSourceSelectionFile(t, repoRoot, "outside.go", "package outside\n")
	if err := os.MkdirAll(filepath.Join(repoRoot, "scope"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(repoRoot, "outside.go"), filepath.Join(repoRoot, "scope", "linked.go")); err != nil {
		t.Skipf("creating source symlink: %v", err)
	}

	err := auditCanonicalSourceSelection(context.Background(), config)
	requireSourceSelectionError(t, err, "scope/linked.go", "symbolic link")
	if strings.Contains(filepath.ToSlash(err.Error()), filepath.ToSlash(repoRoot)) {
		t.Fatalf("audit error exposes temporary absolute path %q: %v", repoRoot, err)
	}
}

func TestSourceSelectionAuditLoaderErrorsAreStableAndRepoRelative(t *testing.T) {
	var messages []string
	for range 2 {
		repoRoot, config := newSourceSelectionModule(t)
		writeSourceSelectionFile(t, repoRoot, "scope/alpha.go", "package alpha\n")
		writeSourceSelectionFile(t, repoRoot, "scope/beta.go", "package beta\n")

		err := auditCanonicalSourceSelection(context.Background(), config)
		if err == nil {
			t.Fatal("auditCanonicalSourceSelection() error = nil, want mixed-package loader error")
		}
		if strings.Contains(filepath.ToSlash(err.Error()), filepath.ToSlash(repoRoot)) {
			t.Fatalf("audit error exposes temporary absolute path %q: %v", repoRoot, err)
		}
		messages = append(messages, err.Error())
	}
	if messages[0] != messages[1] {
		t.Fatalf("equivalent loader errors differ by repository root:\nfirst:  %s\nsecond: %s", messages[0], messages[1])
	}
}

func newSourceSelectionModule(t *testing.T) (string, sourceSelectionConfig) {
	t.Helper()
	repoRoot := t.TempDir()
	const modulePath = "example.com/sourceaudit"
	writeSourceSelectionFile(t, repoRoot, "go.mod", "module "+modulePath+"\n\ngo 1.26.5\n")
	return repoRoot, sourceSelectionConfig{
		RepoRoot:   repoRoot,
		ModulePath: modulePath,
		Roots:      []string{"scope"},
	}
}

func writeSourceSelectionFile(t *testing.T, repoRoot, name, content string) {
	t.Helper()
	filename := filepath.Join(repoRoot, filepath.FromSlash(name))
	if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
		t.Fatalf("creating directory for %s: %v", name, err)
	}
	if err := os.WriteFile(filename, []byte(content), 0o600); err != nil {
		t.Fatalf("writing %s: %v", name, err)
	}
}

func requireSourceSelectionError(t *testing.T, err error, fragments ...string) {
	t.Helper()
	if err == nil {
		t.Fatal("auditCanonicalSourceSelection() error = nil, want error")
	}
	for _, fragment := range fragments {
		if !strings.Contains(err.Error(), fragment) {
			t.Fatalf("auditCanonicalSourceSelection() error = %q, want fragment %q", err, fragment)
		}
	}
}
