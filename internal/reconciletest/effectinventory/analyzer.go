package effectinventory

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/cha"
	"golang.org/x/tools/go/callgraph/vta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

type analysisConfig struct {
	RepoRoot   string
	ModulePath string
	Patterns   []string
}

type analysisProfile struct {
	ID     BuildProfileID
	GOOS   string
	GOARCH string
	Tags   []string
}

// ObservedSite is one typed physical boundary crossing selected by a build
// profile. Matcher is stable across source line movement.
type ObservedSite struct {
	BoundaryID string
	Matcher    OperationSite
	Profile    BuildProfileID
}

type loadedAnalysis struct {
	config        analysisConfig
	profile       analysisProfile
	roots         []*packages.Package
	ssaRoots      []*ssa.Package
	program       *ssa.Program
	packages      map[string]*packages.Package
	sourceFuncs   map[*ssa.Function]bool
	callGraph     *callgraph.Graph
	selectOps     map[token.Pos]OperationKind
	receivers     map[token.Pos]types.Type
	initReachable map[*ssa.Function]bool
	channelInputs map[ssa.Value]map[string]bool
}

type resolvedBoundary struct {
	definition    BoundaryDefinition
	object        types.Object
	function      *types.Func
	interfaceType *types.Interface
	channel       types.Type
}

type observedCall struct {
	boundaryID string
	function   FunctionRef
	operation  OperationKind
	position   token.Pos
}

func discoverProfile(ctx context.Context, config analysisConfig, profile analysisProfile, definitions []BoundaryDefinition) ([]ObservedSite, error) {
	analysis, err := loadAnalysis(ctx, config, profile)
	if err != nil {
		return nil, err
	}
	boundaries, err := resolveBoundaries(analysis.packages, definitions)
	if err != nil {
		return nil, err
	}
	inputProblems := analysis.indexChannelInputBoundaries(boundaries)

	var observed []observedCall
	problems := append([]string(nil), inputProblems...)
	for function := range analysis.sourceFuncs {
		for _, block := range function.Blocks {
			for _, instruction := range block.Instrs {
				if call, ok := instruction.(ssa.CallInstruction); ok {
					callSite, callProblems := analysis.observeCallInstruction(function, call, boundaries)
					problems = append(problems, callProblems...)
					if callSite != nil {
						observed = append(observed, *callSite)
					}
					continue
				}
				channelSites, channelProblems := analysis.observeChannelInstruction(function, instruction, boundaries)
				observed = append(observed, channelSites...)
				problems = append(problems, channelProblems...)
			}
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		problems = compactStrings(problems)
		return nil, fmt.Errorf("effect discovery failed for profile %q:\n- %s", profile.ID, strings.Join(problems, "\n- "))
	}

	return numberObservedCalls(observed, profile.ID), nil
}

func loadAnalysis(ctx context.Context, config analysisConfig, profile analysisProfile) (*loadedAnalysis, error) {
	if strings.TrimSpace(config.RepoRoot) == "" {
		return nil, fmt.Errorf("effect discovery: repository root is required")
	}
	if strings.TrimSpace(config.ModulePath) == "" {
		return nil, fmt.Errorf("effect discovery: module path is required")
	}
	if len(config.Patterns) == 0 {
		return nil, fmt.Errorf("effect discovery: at least one package pattern is required")
	}
	if strings.TrimSpace(string(profile.ID)) == "" || profile.GOOS == "" || profile.GOARCH == "" {
		return nil, fmt.Errorf("effect discovery: profile id, GOOS, and GOARCH are required")
	}
	if err := validateAnalysisProfile(profile); err != nil {
		return nil, fmt.Errorf("effect discovery: %w", err)
	}

	repoRoot, err := filepath.Abs(config.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("effect discovery: resolving repository root: %w", err)
	}
	fset := token.NewFileSet()
	roots, loadErr := packages.Load(&packages.Config{
		Context:    ctx,
		Mode:       packages.LoadSyntax | packages.NeedModule,
		Dir:        repoRoot,
		Env:        profileEnvironment(profile),
		BuildFlags: profileBuildFlags(profile),
		Fset:       fset,
		Tests:      false,
	}, config.Patterns...)
	if loadErr != nil {
		return nil, fmt.Errorf("effect discovery: loading profile %q: %w", profile.ID, loadErr)
	}
	if len(roots) == 0 {
		return nil, fmt.Errorf("effect discovery: profile %q selected no packages", profile.ID)
	}

	packageIndex := make(map[string]*packages.Package)
	var problems []string
	packages.Visit(roots, nil, func(pkg *packages.Package) {
		if pkg == nil {
			problems = append(problems, "loader returned a nil package")
			return
		}
		for _, packageErr := range pkg.Errors {
			problems = append(problems, fmt.Sprintf("package %s: %s", pkg.PkgPath, packageErr.Error()))
		}
		if pkg.IllTyped {
			problems = append(problems, fmt.Sprintf("package %s is ill typed", pkg.PkgPath))
		}
		if pkg.Types != nil && pkg.Types.Path() != "" {
			if previous, exists := packageIndex[pkg.Types.Path()]; exists && previous.Types != pkg.Types {
				problems = append(problems, fmt.Sprintf("package path %s has multiple type identities", pkg.Types.Path()))
			} else {
				packageIndex[pkg.Types.Path()] = pkg
			}
		}
	})
	for _, root := range roots {
		if root.Types == nil || root.TypesInfo == nil || root.Fset == nil || root.TypesSizes == nil {
			problems = append(problems, fmt.Sprintf("package %s has incomplete typed syntax", root.PkgPath))
		}
		if root.Module == nil || root.Module.Path != config.ModulePath {
			got := "<nil>"
			if root.Module != nil {
				got = root.Module.Path
			}
			problems = append(problems, fmt.Sprintf("package %s belongs to module %s, want %s", root.PkgPath, got, config.ModulePath))
		}
		if root.PkgPath != config.ModulePath && !strings.HasPrefix(root.PkgPath, config.ModulePath+"/") {
			problems = append(problems, fmt.Sprintf("root package %s escapes module %s", root.PkgPath, config.ModulePath))
		}
	}
	if len(problems) != 0 {
		sort.Strings(problems)
		problems = compactStrings(problems)
		return nil, fmt.Errorf("effect discovery could not type-check profile %q:\n- %s", profile.ID, strings.Join(problems, "\n- "))
	}

	program, ssaRoots := ssautil.Packages(roots, ssa.InstantiateGenerics|ssa.SanityCheckFunctions|ssa.BuildSerially)
	program.Build()
	for index, root := range ssaRoots {
		if root == nil {
			return nil, fmt.Errorf("effect discovery: package %s has no SSA package", roots[index].PkgPath)
		}
	}
	sourceFuncs, err := collectSourceFunctions(program, roots, ssaRoots)
	if err != nil {
		return nil, err
	}
	chaGraph := cha.CallGraph(program)
	// Keep the explicit full CHA seed. VTA's lazy nil-initial mode can omit
	// pointer-receiver adaptation wrappers, which would turn a memory saving
	// into a silent inventory false negative.
	graphFunctions := make(map[*ssa.Function]bool, len(chaGraph.Nodes))
	for function := range chaGraph.Nodes {
		if function != nil {
			graphFunctions[function] = true
		}
	}

	resolvedGraph := vta.CallGraph(graphFunctions, chaGraph)
	return &loadedAnalysis{
		config:        analysisConfig{RepoRoot: repoRoot, ModulePath: config.ModulePath, Patterns: append([]string(nil), config.Patterns...)},
		profile:       profile,
		roots:         roots,
		ssaRoots:      ssaRoots,
		program:       program,
		packages:      packageIndex,
		sourceFuncs:   sourceFuncs,
		callGraph:     resolvedGraph,
		selectOps:     collectSelectOperations(roots),
		receivers:     collectSelectionReceivers(roots),
		initReachable: functionsReachableFromInitializers(ssaRoots, resolvedGraph),
	}, nil
}

func functionsReachableFromInitializers(packages []*ssa.Package, graph *callgraph.Graph) map[*ssa.Function]bool {
	reachable := make(map[*ssa.Function]bool)
	var visit func(*ssa.Function)
	visit = func(function *ssa.Function) {
		if function == nil || reachable[function] {
			return
		}
		reachable[function] = true
		if origin := function.Origin(); origin != nil {
			reachable[origin] = true
		}
		if node := graph.Nodes[function]; node != nil {
			for _, edge := range node.Out {
				if edge.Callee != nil {
					visit(edge.Callee.Func)
				}
			}
		}
	}
	for _, pkg := range packages {
		if pkg != nil {
			visit(pkg.Func("init"))
		}
	}
	return reachable
}

func profileEnvironment(profile analysisProfile) []string {
	overridden := map[string]bool{
		"CGO_ENABLED":      true,
		"GO111MODULE":      true,
		"GOARCH":           true,
		"GOCACHEPROG":      true,
		"GODEBUG":          true,
		"GOENV":            true,
		"GOEXPERIMENT":     true,
		"GOFIPS140":        true,
		"GOFLAGS":          true,
		"GOOS":             true,
		"GOPACKAGESDRIVER": true,
		"GOROOT":           true,
		"GOTOOLCHAIN":      true,
		"GOWORK":           true,
		"GOAMD64":          true,
	}
	environment := make([]string, 0, len(os.Environ())+8)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !overridden[name] {
			environment = append(environment, item)
		}
	}
	environment = append(environment,
		"CGO_ENABLED=0",
		"GO111MODULE=on",
		"GOARCH="+profile.GOARCH,
		"GOCACHEPROG=",
		"GODEBUG=",
		"GOENV=off",
		"GOEXPERIMENT=",
		"GOFIPS140=off",
		"GOFLAGS=",
		"GOOS="+profile.GOOS,
		"GOPACKAGESDRIVER=off",
		"GOROOT=",
		// A specific name makes the child use the analyzer's own toolchain,
		// independent of the PATH launcher's bundled version.
		// See https://go.dev/doc/toolchain#select.
		"GOTOOLCHAIN="+runtime.Version(),
		"GOWORK=off",
	)
	if profile.GOARCH == "amd64" {
		environment = append(environment, "GOAMD64=v1")
	}
	return environment
}

func collectSourceFunctions(program *ssa.Program, roots []*packages.Package, ssaRoots []*ssa.Package) (map[*ssa.Function]bool, error) {
	functions := make(map[*ssa.Function]bool)
	var add func(*ssa.Function)
	add = func(function *ssa.Function) {
		if function == nil {
			return
		}
		if origin := function.Origin(); origin != nil {
			function = origin
		}
		if functions[function] {
			return
		}
		functions[function] = true
		for _, child := range function.AnonFuncs {
			add(child)
		}
	}

	for index, pkg := range roots {
		for _, file := range pkg.Syntax {
			for _, declaration := range file.Decls {
				functionDecl, ok := declaration.(*ast.FuncDecl)
				if !ok {
					continue
				}
				object, ok := pkg.TypesInfo.Defs[functionDecl.Name].(*types.Func)
				if !ok {
					return nil, fmt.Errorf("effect discovery: %s has no function object", pkg.Fset.PositionFor(functionDecl.Name.Pos(), false))
				}
				function := program.FuncValue(object)
				if function == nil {
					return nil, fmt.Errorf("effect discovery: %s has no SSA function", object.FullName())
				}
				add(function)
			}
		}
		add(ssaRoots[index].Func("init"))
	}
	return functions, nil
}
