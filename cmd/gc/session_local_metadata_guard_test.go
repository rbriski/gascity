package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestSessionLifecycleMetadataWritesUseLocalOrDurableHelpers(t *testing.T) {
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatalf("listing Go files: %v", err)
	}

	var failures []string
	fset := token.NewFileSet()
	for _, path := range files {
		if strings.HasSuffix(path, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", path, err)
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			funcName := fn.Name.Name
			lifecycleMaps := map[string]token.Pos{}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.ValueSpec:
					recordLifecycleValueSpec(n, lifecycleMaps)
				case *ast.AssignStmt:
					recordLifecycleAssign(n, lifecycleMaps)
				case *ast.CallExpr:
					failures = append(failures, lifecycleMetadataCallFailures(path, funcName, fset, lifecycleMaps, n)...)
				}
				return true
			})
		}
	}

	if len(failures) > 0 {
		sort.Strings(failures)
		t.Fatalf("lifecycle metadata writes must use setLocalOrDurable/setMetaBatch:\n%s", strings.Join(failures, "\n"))
	}
}

func recordLifecycleValueSpec(spec *ast.ValueSpec, lifecycleMaps map[string]token.Pos) {
	for i, value := range spec.Values {
		if !exprContainsLifecycleMetadataKey(value) {
			continue
		}
		if i < len(spec.Names) {
			lifecycleMaps[spec.Names[i].Name] = spec.Names[i].Pos()
		}
	}
}

func recordLifecycleAssign(stmt *ast.AssignStmt, lifecycleMaps map[string]token.Pos) {
	for i, rhs := range stmt.Rhs {
		if !exprContainsLifecycleMetadataKey(rhs) {
			continue
		}
		switch {
		case i < len(stmt.Lhs):
			recordLifecycleMapTarget(stmt.Lhs[i], lifecycleMaps)
		case len(stmt.Lhs) == 1:
			recordLifecycleMapTarget(stmt.Lhs[0], lifecycleMaps)
		}
	}
	for _, lhs := range stmt.Lhs {
		index, ok := lhs.(*ast.IndexExpr)
		if !ok || !isLifecycleMetadataString(index.Index) {
			continue
		}
		if ident, ok := index.X.(*ast.Ident); ok {
			lifecycleMaps[ident.Name] = ident.Pos()
		}
	}
}

func recordLifecycleMapTarget(expr ast.Expr, lifecycleMaps map[string]token.Pos) {
	switch target := expr.(type) {
	case *ast.Ident:
		lifecycleMaps[target.Name] = target.Pos()
	case *ast.IndexExpr:
		if ident, ok := target.X.(*ast.Ident); ok {
			lifecycleMaps[ident.Name] = ident.Pos()
		}
	}
}

func lifecycleMetadataCallFailures(path, funcName string, fset *token.FileSet, lifecycleMaps map[string]token.Pos, call *ast.CallExpr) []string {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	if !ok {
		return nil
	}
	var failures []string
	switch selector.Sel.Name {
	case "SetLocalString":
		if !allowDirectSetLocalString(path, funcName) {
			failures = append(failures, formatLifecycleGuardFailure(fset, call.Pos(), "direct SetLocalString call outside local metadata helpers"))
		}
	case "SetMetadata":
		if len(call.Args) >= 2 && isLifecycleMetadataString(call.Args[1]) {
			failures = append(failures, formatLifecycleGuardFailure(fset, call.Pos(), "direct SetMetadata call for lifecycle key"))
		}
	case "SetMetadataBatch":
		if len(call.Args) < 2 || allowDirectLifecycleSetMetadataBatch(path, funcName) {
			return failures
		}
		arg := call.Args[1]
		if exprContainsLifecycleMetadataKey(arg) {
			failures = append(failures, formatLifecycleGuardFailure(fset, call.Pos(), "direct SetMetadataBatch map literal contains lifecycle key"))
			return failures
		}
		if ident, ok := arg.(*ast.Ident); ok {
			if pos, contaminated := lifecycleMaps[ident.Name]; contaminated {
				failures = append(failures, formatLifecycleGuardFailure(fset, call.Pos(), "direct SetMetadataBatch uses lifecycle-key map declared at "+fset.Position(pos).String()))
			}
		}
	}
	return failures
}

func allowDirectSetLocalString(path, funcName string) bool {
	switch filepath.Base(path) + ":" + funcName {
	case "session_local_metadata.go:setLocalOrDurableResult",
		"session_beads.go:setMetaBatch",
		"session_local_metadata_migration.go:migrateLocalLifecycleMetadataForBead":
		return true
	default:
		return false
	}
}

func allowDirectLifecycleSetMetadataBatch(path, funcName string) bool {
	return filepath.Base(path) == "session_local_metadata_migration.go" &&
		funcName == "migrateLocalLifecycleMetadataForBead"
}

func exprContainsLifecycleMetadataKey(expr ast.Expr) bool {
	lit, ok := expr.(*ast.CompositeLit)
	if !ok {
		return false
	}
	for _, elt := range lit.Elts {
		kv, ok := elt.(*ast.KeyValueExpr)
		if !ok {
			continue
		}
		if isLifecycleMetadataString(kv.Key) {
			return true
		}
	}
	return false
}

func isLifecycleMetadataString(expr ast.Expr) bool {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return false
	}
	value, err := strconv.Unquote(lit.Value)
	if err != nil {
		return false
	}
	return isLocalLifecycleMetadataKey(value)
}

func formatLifecycleGuardFailure(fset *token.FileSet, pos token.Pos, message string) string {
	return fset.Position(pos).String() + ": " + message
}
