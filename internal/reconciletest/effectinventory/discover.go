package effectinventory

// discover.go is the canonical, type-aware effect-site analyzer for the P0.1
// reconciler effect inventory. It enumerates DIRECT boundary call sites using
// go/packages + go/types: for each call expression whose resolved callee is a
// seeded boundary method, it confirms the receiver's static type IS or
// IMPLEMENTS the boundary interface. There is no value-flow / escape analysis
// here — a site is a call, nothing more — so an unrelated Stop()/Nudge()/
// Record() on some other type never matches.

import (
	"context"
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"golang.org/x/tools/go/packages"
)

// Profile is one source-selection class (GOOS/GOARCH/build tags) the analyzer
// loads and type-checks independently.
type Profile struct {
	ID     BuildProfileID
	GOOS   string
	GOARCH string
	Tags   []string
}

// DiscoverConfig configures a canonical effect-site discovery run.
//
// ModulePath is advisory (populated by RepoRootFromDir); loading is driven by
// RepoRoot as the go/packages working directory.
type DiscoverConfig struct {
	RepoRoot   string
	ModulePath string
	Profiles   []Profile
	Boundaries []BoundaryDefinition
}

// DiscoveredSite is one direct boundary call site, deduplicated across the
// analyzed build profiles.
type DiscoveredSite struct {
	BoundaryID    string
	Kind          EffectKind
	Matcher       OperationSite
	Package       string
	ReceiverType  string
	ViaInterface  bool
	BuildProfiles []BuildProfileID
}

// discoverScopes are the four plan scopes (IMPLEMENTATION_PLAN.md P0.1). The
// analyzer walks call sites only within these; boundary interfaces from deps
// (beads, events) are resolved for typing but never walked.
var discoverScopes = []string{
	"./cmd/gc/...",
	"./internal/session/...",
	"./internal/worker/...",
	"./internal/runtime/...",
}

// runtimeTestSupportPkg is the conformance test-support harness; excluded from
// site enumeration (it is test doubles, not reconciler code).
const runtimeTestSupportPkg = "github.com/gastownhall/gascity/internal/runtime/runtimetest"

// CanonicalProfiles returns the five build profiles the P0.1 inventory unions
// over. All five cross-compile the four scopes with CGO disabled.
func CanonicalProfiles() []Profile {
	return []Profile{
		{ID: BuildLinuxDefault, GOOS: "linux", GOARCH: "amd64"},
		{ID: BuildLinuxNative, GOOS: "linux", GOARCH: "amd64", Tags: []string{"gascity_native_beads"}},
		{ID: BuildDarwinDefault, GOOS: "darwin", GOARCH: "amd64"},
		{ID: BuildDarwinNative, GOOS: "darwin", GOARCH: "amd64", Tags: []string{"gascity_native_beads"}},
		{ID: BuildWindowsCompile, GOOS: "windows", GOARCH: "amd64"},
	}
}

// Discover loads each profile once, enumerates direct boundary call sites, and
// unions them: each returned DiscoveredSite lists the profiles it appeared in
// (sorted). The result is sorted deterministically by BoundaryID, then the
// enclosing function's Package/Receiver/Name, then Ordinal. A non-nil error is
// returned only for a real package-load failure, never for a "no sites" outcome.
func Discover(ctx context.Context, cfg DiscoverConfig) ([]DiscoveredSite, error) {
	if strings.TrimSpace(cfg.RepoRoot) == "" {
		return nil, fmt.Errorf("effect discovery: repository root is required")
	}
	if len(cfg.Profiles) == 0 {
		return nil, fmt.Errorf("effect discovery: at least one profile is required")
	}
	if len(cfg.Boundaries) == 0 {
		return nil, fmt.Errorf("effect discovery: at least one boundary is required")
	}
	repoRoot, err := filepath.Abs(cfg.RepoRoot)
	if err != nil {
		return nil, fmt.Errorf("effect discovery: resolving repository root: %w", err)
	}

	// Deterministic profile order makes the first-seen ReceiverType/ViaInterface
	// for a shared site stable.
	profiles := append([]Profile(nil), cfg.Profiles...)
	sort.Slice(profiles, func(i, j int) bool { return profiles[i].ID < profiles[j].ID })

	merged := map[string]*DiscoveredSite{}
	for _, profile := range profiles {
		sites, err := discoverProfileSites(ctx, repoRoot, profile, cfg.Boundaries)
		if err != nil {
			return nil, err
		}
		for _, site := range sites {
			key := site.BoundaryID + "\x00" + site.Matcher.key()
			if existing, ok := merged[key]; ok {
				existing.BuildProfiles = append(existing.BuildProfiles, profile.ID)
				continue
			}
			site.BuildProfiles = []BuildProfileID{profile.ID}
			clone := site
			merged[key] = &clone
		}
	}

	result := make([]DiscoveredSite, 0, len(merged))
	for _, site := range merged {
		sort.Slice(site.BuildProfiles, func(i, j int) bool { return site.BuildProfiles[i] < site.BuildProfiles[j] })
		result = append(result, *site)
	}
	sort.Slice(result, func(i, j int) bool {
		return discoveredSortKey(result[i]) < discoveredSortKey(result[j])
	})
	return result, nil
}

func discoveredSortKey(site DiscoveredSite) string {
	object := site.Matcher.Enclosing.Object
	return strings.Join([]string{
		site.BoundaryID, object.Package, object.Receiver, object.Name,
		site.Matcher.Enclosing.File, fmt.Sprintf("%08d", site.Matcher.Ordinal),
	}, "\x00")
}

// discoverProfileSites loads one profile and returns its per-profile sites with
// edit-stable ordinals assigned.
func discoverProfileSites(ctx context.Context, repoRoot string, profile Profile, boundaries []BoundaryDefinition) ([]DiscoveredSite, error) {
	buildFlags := []string{"-mod=readonly"}
	tags := append([]string(nil), profile.Tags...)
	sort.Strings(tags)
	if len(tags) > 0 {
		buildFlags = append(buildFlags, "-tags="+strings.Join(tags, ","))
	}
	fset := token.NewFileSet()
	roots, err := packages.Load(&packages.Config{
		Context: ctx,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedImports |
			packages.NeedDeps | packages.NeedTypes | packages.NeedTypesInfo |
			packages.NeedSyntax | packages.NeedModule,
		Dir:        repoRoot,
		Env:        discoverEnv(profile),
		BuildFlags: buildFlags,
		Fset:       fset,
		Tests:      false,
	}, discoverScopes...)
	if err != nil {
		return nil, fmt.Errorf("effect discovery: loading profile %q: %w", profile.ID, err)
	}

	// Index roots + transitive deps so boundary interfaces that live in deps
	// (beads, events) can be resolved to their *types.Interface.
	index := map[string]*packages.Package{}
	packages.Visit(roots, nil, func(pkg *packages.Package) {
		if pkg != nil && pkg.Types != nil && pkg.PkgPath != "" {
			index[pkg.PkgPath] = pkg
		}
	})

	resolved := compileBoundarySet(boundaries, index)

	var raw []rawMatch
	for _, pkg := range roots {
		if pkg.TypesInfo == nil || shouldSkipPackage(pkg.PkgPath) {
			continue
		}
		for _, file := range pkg.Syntax {
			if isGeneratedFile(file) {
				continue
			}
			walkFileForMatches(pkg, file, resolved, &raw)
		}
	}
	return numberMatches(raw), nil
}

// resolvedBoundarySet is the per-profile compiled boundary index.
type resolvedBoundarySet struct {
	interfaces   []resolvedInterfaceBoundary
	exactMethods []exactMethodBoundary
	exactFuncs   []exactFuncBoundary
	methodNames  map[string]bool // fast pre-filter for method-value calls
}

type resolvedInterfaceBoundary struct {
	def   BoundaryDefinition
	iface *types.Interface
}

type exactMethodBoundary struct {
	def      BoundaryDefinition
	pkgPath  string
	recvName string
}

type exactFuncBoundary struct {
	def     BoundaryDefinition
	pkgPath string
	name    string
}

// compileBoundarySet resolves the seed set against one profile's loaded packages.
// An interface seed whose interface is absent from the load set is silently
// skipped for this profile (it contributes no matches); the golden gate catches
// any unexpected disappearance as drift.
func compileBoundarySet(defs []BoundaryDefinition, index map[string]*packages.Package) resolvedBoundarySet {
	set := resolvedBoundarySet{methodNames: map[string]bool{}}
	for _, def := range defs {
		switch def.Match {
		case ObjectMatchInterfaceImplementors:
			set.methodNames[def.Object.Name] = true
			iface := lookupInterface(index, def.Object.Package, def.Object.Receiver)
			if iface == nil {
				continue
			}
			set.interfaces = append(set.interfaces, resolvedInterfaceBoundary{def: def, iface: iface})
		case ObjectMatchExact:
			if def.Object.Receiver == "" {
				set.exactFuncs = append(set.exactFuncs, exactFuncBoundary{def: def, pkgPath: def.Object.Package, name: def.Object.Name})
			} else {
				set.methodNames[def.Object.Name] = true
				set.exactMethods = append(set.exactMethods, exactMethodBoundary{def: def, pkgPath: def.Object.Package, recvName: def.Object.Receiver})
			}
		default:
			// ObjectMatchChannel and any future kinds are out of scope for the
			// direct-call analyzer.
		}
	}
	return set
}

func lookupInterface(index map[string]*packages.Package, pkgPath, name string) *types.Interface {
	pkg := index[pkgPath]
	if pkg == nil || pkg.Types == nil {
		return nil
	}
	obj := pkg.Types.Scope().Lookup(name)
	if obj == nil {
		return nil
	}
	iface, _ := obj.Type().Underlying().(*types.Interface)
	return iface
}

// rawMatch is one matched call before ordinals are assigned.
type rawMatch struct {
	def          BoundaryDefinition
	enclosing    FunctionRef
	pkgPath      string
	receiverType string
	viaInterface bool
	position     token.Pos
}

// walkFileForMatches walks one file, folding closures into their enclosing
// top-level FuncDecl, and appends every boundary match to raw.
func walkFileForMatches(pkg *packages.Package, file *ast.File, resolved resolvedBoundarySet, raw *[]rawMatch) {
	info := pkg.TypesInfo
	var process func(node ast.Node, enclosing *types.Func)
	process = func(node ast.Node, enclosing *types.Func) {
		ast.Inspect(node, func(n ast.Node) bool {
			switch v := n.(type) {
			case *ast.FuncDecl:
				if v.Body != nil {
					fn, _ := info.Defs[v.Name].(*types.Func)
					process(v.Body, fn)
				}
				return false
			case *ast.FuncLit:
				process(v.Body, enclosing)
				return false
			case *ast.CallExpr:
				if enclosing != nil {
					matchCall(pkg, v, enclosing, resolved, raw)
				}
				return true
			}
			return true
		})
	}
	process(file, nil)
}

// matchCall resolves one call expression against the boundary set and appends a
// rawMatch on a hit. This is the canonical type-aware matcher.
func matchCall(pkg *packages.Package, call *ast.CallExpr, enclosing *types.Func, resolved resolvedBoundarySet, raw *[]rawMatch) {
	selExpr, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return
	}
	info := pkg.TypesInfo

	// Method-value call x.Method(...) resolved via a types.Selection.
	if sel := info.Selections[selExpr]; sel != nil && sel.Kind() == types.MethodVal {
		fn, ok := sel.Obj().(*types.Func)
		if !ok || fn == nil {
			return
		}
		name := fn.Name()
		if !resolved.methodNames[name] { // fast pre-filter
			return
		}
		recv := sel.Recv()
		if recv == nil {
			return
		}
		for _, boundary := range resolved.interfaces {
			if boundary.def.Object.Name != name {
				continue
			}
			if typeSatisfies(recv, boundary.iface) {
				appendMatch(raw, boundary.def, pkg, enclosing, recv, selExpr, isInterfaceStatic(recv))
				return
			}
		}
		for _, boundary := range resolved.exactMethods {
			if boundary.def.Object.Name != name {
				continue
			}
			if namedTypeMatches(recv, boundary.pkgPath, boundary.recvName) {
				appendMatch(raw, boundary.def, pkg, enclosing, recv, selExpr, false)
				return
			}
		}
		return
	}

	// Package-qualified function call syscall.Kill(...): not a selection.
	if fn, ok := info.Uses[selExpr.Sel].(*types.Func); ok && fn != nil {
		for _, boundary := range resolved.exactFuncs {
			if fn.Name() != boundary.name || fn.Pkg() == nil || fn.Pkg().Path() != boundary.pkgPath {
				continue
			}
			if sig, ok := fn.Type().(*types.Signature); ok && sig.Recv() == nil {
				appendMatch(raw, boundary.def, pkg, enclosing, nil, selExpr, false)
				return
			}
		}
	}
}

func appendMatch(raw *[]rawMatch, def BoundaryDefinition, pkg *packages.Package, enclosing *types.Func, recv types.Type, selExpr *ast.SelectorExpr, viaInterface bool) {
	position := pkg.Fset.Position(selExpr.Sel.Pos())
	match := rawMatch{
		def: def,
		enclosing: FunctionRef{
			Object: objectRefForFunction(enclosing),
			File:   filepath.Base(position.Filename),
		},
		pkgPath:      pkg.PkgPath,
		viaInterface: viaInterface,
		position:     selExpr.Sel.Pos(),
	}
	if recv != nil {
		match.receiverType = types.TypeString(recv, discoverQualifier)
	}
	*raw = append(*raw, match)
}

// numberMatches assigns edit-stable ordinals: 1-based among calls to the same
// BoundaryID inside the same enclosing FunctionRef, ordered by source position.
func numberMatches(raw []rawMatch) []DiscoveredSite {
	sort.Slice(raw, func(i, j int) bool {
		left := raw[i].def.ID + "|" + raw[i].enclosing.key()
		right := raw[j].def.ID + "|" + raw[j].enclosing.key()
		if left != right {
			return left < right
		}
		return raw[i].position < raw[j].position
	})
	ordinals := map[string]int{}
	sites := make([]DiscoveredSite, 0, len(raw))
	for _, match := range raw {
		group := match.def.ID + "|" + match.enclosing.key()
		ordinals[group]++
		sites = append(sites, DiscoveredSite{
			BoundaryID: match.def.ID,
			Kind:       match.def.Kind,
			Matcher: OperationSite{
				Operation: OperationCall,
				Enclosing: match.enclosing,
				Ordinal:   ordinals[group],
			},
			Package:      match.pkgPath,
			ReceiverType: match.receiverType,
			ViaInterface: match.viaInterface,
		})
	}
	return sites
}

// typeSatisfies is the type-aware gate: does recv's static type satisfy iface,
// tolerating the value-receiver / pointer-method selection gotcha.
func typeSatisfies(recv types.Type, iface *types.Interface) bool {
	if recv == nil || iface == nil {
		return false
	}
	if types.Implements(recv, iface) {
		return true
	}
	switch recv.Underlying().(type) {
	case *types.Pointer, *types.Interface:
		return false
	default:
		return types.Implements(types.NewPointer(recv), iface)
	}
}

func isInterfaceStatic(t types.Type) bool {
	if t == nil {
		return false
	}
	_, ok := t.Underlying().(*types.Interface)
	return ok
}

// namedTypeMatches reports whether t (deref'd once) is the named type pkgPath.name.
func namedTypeMatches(t types.Type, pkgPath, name string) bool {
	t = types.Unalias(t)
	if pointer, ok := t.(*types.Pointer); ok {
		t = types.Unalias(pointer.Elem())
	}
	named, ok := t.(*types.Named)
	if !ok {
		return false
	}
	obj := named.Obj()
	return obj != nil && obj.Pkg() != nil && obj.Pkg().Path() == pkgPath && obj.Name() == name
}

func discoverQualifier(pkg *types.Package) string {
	if pkg == nil {
		return ""
	}
	return pkg.Name()
}

var discoverGeneratedRe = regexp.MustCompile(`^// Code generated .* DO NOT EDIT\.$`)

func isGeneratedFile(file *ast.File) bool {
	for _, group := range file.Comments {
		for _, comment := range group.List {
			if discoverGeneratedRe.MatchString(comment.Text) {
				return true
			}
		}
	}
	return false
}

func shouldSkipPackage(pkgPath string) bool {
	if pkgPath == runtimeTestSupportPkg {
		return true
	}
	// Defensive: the analyzer's own package is never in scope, but never walk it.
	return strings.Contains(pkgPath, "/reconciletest/effectinventory")
}

func discoverEnv(profile Profile) []string {
	overridden := map[string]bool{
		"CGO_ENABLED": true, "GOARCH": true, "GOENV": true, "GOEXPERIMENT": true,
		"GOFLAGS": true, "GOOS": true, "GOPACKAGESDRIVER": true, "GOWORK": true, "GOAMD64": true,
	}
	env := make([]string, 0, len(os.Environ())+8)
	for _, item := range os.Environ() {
		name, _, _ := strings.Cut(item, "=")
		if !overridden[name] {
			env = append(env, item)
		}
	}
	goos := profile.GOOS
	if goos == "" {
		goos = "linux"
	}
	goarch := profile.GOARCH
	if goarch == "" {
		goarch = "amd64"
	}
	env = append(env,
		"CGO_ENABLED=0",
		"GOARCH="+goarch,
		"GOENV=off",
		"GOEXPERIMENT=",
		"GOFLAGS=",
		"GOOS="+goos,
		"GOPACKAGESDRIVER=off",
		"GOWORK=off",
	)
	return env
}

// RepoRootFromDir walks up from dir to the nearest go.mod, returning the
// directory that holds it and the module path declared inside.
func RepoRootFromDir(dir string) (root, modulePath string, err error) {
	current, err := filepath.Abs(dir)
	if err != nil {
		return "", "", fmt.Errorf("effect discovery: resolving %q: %w", dir, err)
	}
	for {
		goMod := filepath.Join(current, "go.mod")
		if data, readErr := os.ReadFile(goMod); readErr == nil {
			path := modulePathFromGoMod(data)
			if path == "" {
				return "", "", fmt.Errorf("effect discovery: %s declares no module path", goMod)
			}
			return current, path, nil
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", "", fmt.Errorf("effect discovery: no go.mod found above %q", dir)
		}
		current = parent
	}
}

func modulePathFromGoMod(data []byte) string {
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if rest, ok := strings.CutPrefix(line, "module"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return ""
}
