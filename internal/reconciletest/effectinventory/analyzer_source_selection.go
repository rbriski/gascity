package effectinventory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"go/build/constraint"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

type sourceSelectionConfig struct {
	RepoRoot   string
	ModulePath string
	Roots      []string
}

type sourceSelectionProblem struct {
	path   string
	kind   string
	detail string
}

func canonicalAnalysisRoots() []string {
	return []string{
		"cmd/gc",
		"internal/session",
		"internal/worker",
		"internal/runtime",
	}
}

func canonicalSourcePatterns(roots []string) ([]string, error) {
	if len(roots) == 0 {
		return nil, fmt.Errorf("source-selection audit: at least one source root is required")
	}

	seen := make(map[string]bool, len(roots))
	patterns := make([]string, 0, len(roots))
	for _, root := range roots {
		switch {
		case root == "":
			return nil, fmt.Errorf("source-selection audit: source root is empty")
		case filepath.IsAbs(root) || path.IsAbs(root) || hasWindowsDrivePrefix(root):
			return nil, fmt.Errorf("source-selection audit: source root %q is absolute", root)
		case filepath.ToSlash(root) != root || strings.Contains(root, `\`):
			return nil, fmt.Errorf("source-selection audit: source root %q must use slash separators", root)
		case path.Clean(root) != root || root == "." || root == ".." || strings.HasPrefix(root, "../"):
			return nil, fmt.Errorf("source-selection audit: source root %q is not a clean repository-relative path", root)
		case strings.ContainsAny(root, "*?[") || strings.Contains(root, "..."):
			return nil, fmt.Errorf("source-selection audit: source root %q contains a package wildcard", root)
		case seen[root]:
			return nil, fmt.Errorf("source-selection audit: duplicate source root %q", root)
		}
		seen[root] = true
		patterns = append(patterns, "./"+root+"/...")
	}
	return patterns, nil
}

func hasWindowsDrivePrefix(value string) bool {
	return len(value) >= 2 && ((value[0] >= 'a' && value[0] <= 'z') || (value[0] >= 'A' && value[0] <= 'Z')) && value[1] == ':'
}

func sourceCandidates(repoRoot string, roots []string) ([]string, error) {
	if strings.TrimSpace(repoRoot) == "" {
		return nil, fmt.Errorf("source-selection audit: repository root is required")
	}
	if _, err := canonicalSourcePatterns(roots); err != nil {
		return nil, err
	}

	absoluteRoot, err := filepath.Abs(repoRoot)
	if err != nil {
		return nil, fmt.Errorf("source-selection audit: resolving repository root: %w", err)
	}
	candidates := make(map[string]bool)
	for _, root := range roots {
		walkRoot := filepath.Join(absoluteRoot, filepath.FromSlash(root))
		info, err := os.Stat(walkRoot)
		if err != nil {
			return nil, fmt.Errorf("source-selection audit: inspecting source root %q: %s", root, stableFilesystemError(err))
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("source-selection audit: source root %q is not a directory", root)
		}

		err = filepath.WalkDir(walkRoot, func(filename string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				relative, relErr := filepath.Rel(absoluteRoot, filename)
				if relErr != nil {
					relative = root
				}
				return fmt.Errorf("%s: %s", filepath.ToSlash(relative), stableFilesystemError(walkErr))
			}
			relativeToWalkRoot, err := filepath.Rel(walkRoot, filename)
			if err != nil {
				return fmt.Errorf("resolving path beneath source root %q: %s", root, stableFilesystemError(err))
			}
			if entry.IsDir() {
				if relativeToWalkRoot == "." {
					return nil
				}
				base := entry.Name()
				if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || base == "testdata" || belowVendorDirectory(relativeToWalkRoot) {
					return filepath.SkipDir
				}
				return nil
			}

			base := entry.Name()
			if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") || !strings.HasSuffix(base, ".go") || strings.HasSuffix(base, "_test.go") {
				return nil
			}
			relativeToRepo, err := filepath.Rel(absoluteRoot, filename)
			if err != nil || relativeToRepo == ".." || strings.HasPrefix(relativeToRepo, ".."+string(filepath.Separator)) {
				return fmt.Errorf("source file beneath root %q escapes repository root", root)
			}
			diagnosticPath := filepath.ToSlash(relativeToRepo)
			if entry.Type()&fs.ModeSymlink != 0 {
				return fmt.Errorf("source file %s is a symbolic link", diagnosticPath)
			}
			info, err := entry.Info()
			if err != nil {
				return fmt.Errorf("inspecting source file %s: %s", diagnosticPath, stableFilesystemError(err))
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("source file %s is not regular", diagnosticPath)
			}
			candidates[diagnosticPath] = true
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("source-selection audit: walking source root %q: %w", root, err)
		}
	}

	result := make([]string, 0, len(candidates))
	for candidate := range candidates {
		result = append(result, candidate)
	}
	sort.Strings(result)
	return result, nil
}

func stableFilesystemError(err error) string {
	var pathError *fs.PathError
	if errors.As(err, &pathError) {
		return fmt.Sprintf("%s: %v", pathError.Op, pathError.Err)
	}
	return err.Error()
}

func belowVendorDirectory(relative string) bool {
	parts := strings.Split(filepath.ToSlash(relative), "/")
	for _, part := range parts[:len(parts)-1] {
		if part == "vendor" {
			return true
		}
	}
	return false
}

func implicitArchitectureConstraint(filename string) (string, bool) {
	base := path.Base(filepath.ToSlash(filename))
	if strings.HasPrefix(base, ".") || strings.HasPrefix(base, "_") {
		return "", false
	}
	stem, _, _ := strings.Cut(base, ".")
	stem = strings.TrimSuffix(stem, "_test")
	separator := strings.LastIndexByte(stem, '_')
	if separator <= 0 || separator == len(stem)-1 {
		return "", false
	}
	architecture := stem[separator+1:]
	if !knownGOARCH(architecture) {
		return "", false
	}
	return architecture, true
}

// canonicalGOARCHs mirrors internal/syslist.KnownArch in the Go toolchain
// pinned by go.mod. The toolchain-source parity test makes additions or
// removals fail closed instead of silently weakening the profile union.
func canonicalGOARCHs() []string {
	return []string{
		"386", "amd64", "amd64p32",
		"arm", "armbe", "arm64", "arm64be",
		"loong64",
		"mips", "mipsle", "mips64", "mips64le", "mips64p32", "mips64p32le",
		"ppc", "ppc64", "ppc64le",
		"riscv", "riscv64",
		"s390", "s390x",
		"sparc", "sparc64",
		"wasm",
	}
}

func knownGOARCH(value string) bool {
	for _, architecture := range canonicalGOARCHs() {
		if value == architecture {
			return true
		}
	}
	return false
}

func explicitArchitectureConstraint(content []byte) (string, bool, error) {
	content = bytes.TrimPrefix(content, []byte{0xef, 0xbb, 0xbf})
	header, goBuild, err := sourceLeadingBuildHeader(content)
	if err != nil {
		return "", false, err
	}

	var expressions []constraint.Expr
	if goBuild != nil {
		expression, err := constraint.Parse(string(goBuild))
		if err != nil {
			return "", false, fmt.Errorf("parsing //go:build constraint: %w", err)
		}
		expressions = append(expressions, expression)
	} else {
		for len(header) > 0 {
			line := header
			if index := bytes.IndexByte(line, '\n'); index >= 0 {
				line, header = line[:index], header[index+1:]
			} else {
				header = nil
			}
			text := string(bytes.TrimSpace(line))
			if !constraint.IsPlusBuild(text) {
				continue
			}
			expression, err := constraint.Parse(text)
			if err == nil {
				expressions = append(expressions, expression)
			}
		}
	}

	tags := make(map[string]bool)
	for _, expression := range expressions {
		collectArchitectureTags(expression, tags)
	}
	if len(tags) == 0 {
		return "", false, nil
	}
	ordered := make([]string, 0, len(tags))
	for tag := range tags {
		ordered = append(ordered, tag)
	}
	sort.Strings(ordered)
	return ordered[0], true, nil
}

func collectArchitectureTags(expression constraint.Expr, tags map[string]bool) {
	switch expression := expression.(type) {
	case *constraint.TagExpr:
		if architectureTag(expression.Tag) {
			tags[expression.Tag] = true
		}
	case *constraint.NotExpr:
		collectArchitectureTags(expression.X, tags)
	case *constraint.AndExpr:
		collectArchitectureTags(expression.X, tags)
		collectArchitectureTags(expression.Y, tags)
	case *constraint.OrExpr:
		collectArchitectureTags(expression.X, tags)
		collectArchitectureTags(expression.Y, tags)
	}
}

func architectureTag(tag string) bool {
	if knownGOARCH(tag) {
		return true
	}
	architecture, feature, hasFeature := strings.Cut(tag, ".")
	return hasFeature && feature != "" && knownGOARCH(architecture)
}

// sourceLeadingBuildHeader mirrors go/build's placement rules: modern
// constraints may appear before the package clause, while legacy constraints
// must precede the last separating blank in the leading line-comment block.
func sourceLeadingBuildHeader(content []byte) (header, goBuild []byte, err error) {
	end := 0
	rest := content
	ended := false
	inBlock := false

Lines:
	for len(rest) > 0 {
		line := rest
		if index := bytes.IndexByte(line, '\n'); index >= 0 {
			line, rest = line[:index], rest[index+1:]
		} else {
			rest = nil
		}
		line = bytes.TrimSpace(line)
		if len(line) == 0 && !ended {
			end = len(content) - len(rest)
			continue
		}
		if !bytes.HasPrefix(line, []byte("//")) {
			ended = true
		}
		if !inBlock && constraint.IsGoBuild(string(line)) {
			if goBuild != nil {
				return nil, nil, errors.New("multiple //go:build comments")
			}
			goBuild = line
		}

		for len(line) > 0 {
			if inBlock {
				if index := bytes.Index(line, []byte("*/")); index >= 0 {
					inBlock = false
					line = bytes.TrimSpace(line[index+2:])
					continue
				}
				continue Lines
			}
			switch {
			case bytes.HasPrefix(line, []byte("//")):
				continue Lines
			case bytes.HasPrefix(line, []byte("/*")):
				inBlock = true
				line = bytes.TrimSpace(line[2:])
			default:
				break Lines
			}
		}
	}
	return content[:end], goBuild, nil
}

func auditCanonicalSourceSelection(ctx context.Context, config sourceSelectionConfig) error {
	if strings.TrimSpace(config.ModulePath) == "" {
		return fmt.Errorf("source-selection audit: module path is required")
	}
	patterns, err := canonicalSourcePatterns(config.Roots)
	if err != nil {
		return err
	}
	candidates, err := sourceCandidates(config.RepoRoot, config.Roots)
	if err != nil {
		return err
	}

	var problems []sourceSelectionProblem
	invalidHeader := false
	for _, candidate := range candidates {
		if architecture, found := implicitArchitectureConstraint(candidate); found {
			problems = append(problems, sourceSelectionProblem{
				path:   candidate,
				kind:   "architecture-filename",
				detail: fmt.Sprintf("architecture-specific filename suffix %q is forbidden", architecture),
			})
		}
		content, err := os.ReadFile(filepath.Join(config.RepoRoot, filepath.FromSlash(candidate)))
		if err != nil {
			problems = append(problems, sourceSelectionProblem{path: candidate, kind: "read", detail: stableFilesystemError(err)})
			invalidHeader = true
			continue
		}
		architecture, found, err := explicitArchitectureConstraint(content)
		switch {
		case err != nil:
			problems = append(problems, sourceSelectionProblem{path: candidate, kind: "build-constraint", detail: err.Error()})
			invalidHeader = true
		case found:
			problems = append(problems, sourceSelectionProblem{
				path:   candidate,
				kind:   "architecture-build-constraint",
				detail: fmt.Sprintf("build constraint references GOARCH %q", architecture),
			})
		}
	}
	if invalidHeader {
		return sourceSelectionProblems(problems)
	}

	selected, selectionProblems, err := selectedCanonicalSources(ctx, config, patterns, candidates)
	if err != nil {
		return err
	}
	problems = append(problems, selectionProblems...)
	problems = append(problems, missingSourceProblems(candidates, selected)...)
	return sourceSelectionProblems(problems)
}

func selectedCanonicalSources(ctx context.Context, config sourceSelectionConfig, patterns, candidates []string) (map[string]bool, []sourceSelectionProblem, error) {
	repoRoot, err := filepath.Abs(config.RepoRoot)
	if err != nil {
		return nil, nil, fmt.Errorf("source-selection audit: resolving repository root: %w", err)
	}
	candidateSet := make(map[string]bool, len(candidates))
	for _, candidate := range candidates {
		candidateSet[candidate] = true
	}

	selected := make(map[string]bool)
	var problems []sourceSelectionProblem
	for _, profile := range canonicalAnalysisProfiles() {
		loaded, loadErr := packages.Load(&packages.Config{
			Context:    ctx,
			Mode:       packages.LoadFiles | packages.NeedModule,
			Dir:        repoRoot,
			Env:        profileEnvironment(profile),
			BuildFlags: profileBuildFlags(profile),
			Tests:      false,
		}, patterns...)
		if loadErr != nil {
			return nil, nil, fmt.Errorf("source-selection audit: loading profile %q: %s", profile.ID, stableDiagnosticText(repoRoot, loadErr.Error()))
		}

		var loadProblems []string
		for _, pkg := range loaded {
			if pkg == nil {
				loadProblems = append(loadProblems, "loader returned a nil package")
				continue
			}
			for _, packageErr := range pkg.Errors {
				loadProblems = append(loadProblems, fmt.Sprintf("package %s: %s", pkg.PkgPath, stableDiagnosticText(repoRoot, packageErr.Error())))
			}
			if pkg.Module == nil || pkg.Module.Path != config.ModulePath {
				modulePath := "<nil>"
				if pkg.Module != nil {
					modulePath = pkg.Module.Path
				}
				loadProblems = append(loadProblems, fmt.Sprintf("package %s belongs to module %s, want %s", pkg.PkgPath, modulePath, config.ModulePath))
			}
			if pkg.PkgPath != config.ModulePath && !strings.HasPrefix(pkg.PkgPath, config.ModulePath+"/") {
				loadProblems = append(loadProblems, fmt.Sprintf("package %s escapes module %s", pkg.PkgPath, config.ModulePath))
			}

			for _, compiled := range pkg.CompiledGoFiles {
				relative, inside, err := sourcePathRelativeToRepo(repoRoot, compiled)
				if err != nil {
					loadProblems = append(loadProblems, fmt.Sprintf("package %s has invalid compiled source path: %s", pkg.PkgPath, stableDiagnosticText(repoRoot, err.Error())))
					continue
				}
				if !inside {
					continue
				}
				if !candidateSet[relative] {
					problems = append(problems, sourceSelectionProblem{
						path:   relative,
						kind:   "selected-outside-census",
						detail: fmt.Sprintf("profile %q selected a file outside the production source census", profile.ID),
					})
					continue
				}
				selected[relative] = true
			}
		}
		if len(loadProblems) != 0 {
			sort.Strings(loadProblems)
			loadProblems = compactStrings(loadProblems)
			return nil, nil, fmt.Errorf("source-selection audit could not load profile %q:\n- %s", profile.ID, strings.Join(loadProblems, "\n- "))
		}
	}
	return selected, problems, nil
}

func stableDiagnosticText(repoRoot, message string) string {
	root := strings.TrimSuffix(filepath.ToSlash(repoRoot), "/")
	result := filepath.ToSlash(message)
	if root == "" {
		return result
	}
	result = strings.ReplaceAll(result, root+"/", "")
	return strings.ReplaceAll(result, root, ".")
}

func sourcePathRelativeToRepo(repoRoot, filename string) (string, bool, error) {
	absolute, err := filepath.Abs(filename)
	if err != nil {
		return "", false, err
	}
	relative, err := filepath.Rel(repoRoot, absolute)
	if err != nil {
		return "", false, err
	}
	if relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", false, nil
	}
	return filepath.ToSlash(relative), true, nil
}

func missingSourceProblems(candidates []string, selected map[string]bool) []sourceSelectionProblem {
	byDirectory := make(map[string][]string)
	selectedByDirectory := make(map[string]bool)
	for _, candidate := range candidates {
		directory := path.Dir(candidate)
		byDirectory[directory] = append(byDirectory[directory], candidate)
		if selected[candidate] {
			selectedByDirectory[directory] = true
		}
	}

	var problems []sourceSelectionProblem
	for directory, files := range byDirectory {
		if !selectedByDirectory[directory] {
			basenames := make([]string, len(files))
			for index, file := range files {
				basenames[index] = path.Base(file)
			}
			sort.Strings(basenames)
			problems = append(problems, sourceSelectionProblem{
				path:   directory,
				kind:   "unselected-package",
				detail: fmt.Sprintf("no source selected by any canonical analysis profile (%s)", strings.Join(basenames, ", ")),
			})
			continue
		}
		for _, file := range files {
			if !selected[file] {
				problems = append(problems, sourceSelectionProblem{
					path:   file,
					kind:   "unselected-file",
					detail: "not selected by any canonical analysis profile",
				})
			}
		}
	}
	return problems
}

func sourceSelectionProblems(problems []sourceSelectionProblem) error {
	if len(problems) == 0 {
		return nil
	}
	sort.Slice(problems, func(i, j int) bool {
		if problems[i].path != problems[j].path {
			return problems[i].path < problems[j].path
		}
		if problems[i].kind != problems[j].kind {
			return problems[i].kind < problems[j].kind
		}
		return problems[i].detail < problems[j].detail
	})
	lines := make([]string, 0, len(problems))
	for _, problem := range problems {
		lines = append(lines, fmt.Sprintf("[%s] %s: %s", problem.kind, problem.path, problem.detail))
	}
	return fmt.Errorf("source-selection audit failed:\n- %s", strings.Join(lines, "\n- "))
}
