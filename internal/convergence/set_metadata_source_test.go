package convergence

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
)

func TestConvergenceProductionChecksEverySetMetadataResult(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("locating convergence package")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("reading convergence package: %v", err)
	}

	fset := token.NewFileSet()
	var ignored []string
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("parsing %s: %v", name, err)
		}
		for _, call := range ignoredSetMetadataCalls(file) {
			position := fset.Position(call.Pos())
			ignored = append(ignored, fmt.Sprintf("%s:%d", filepath.Base(position.Filename), position.Line))
		}
	}

	sort.Strings(ignored)
	if len(ignored) != 0 {
		t.Fatalf("production convergence SetMetadata results must be checked; ignored at %s", strings.Join(ignored, ", "))
	}
}

func TestIgnoredSetMetadataGuardRejectsDiscardForms(t *testing.T) {
	const source = `package fixture
	type store interface { SetMetadata(string, string, string) error }
	func discarded(s store) {
		_ = s.SetMetadata("root", "key", "value")
		s.SetMetadata("root", "key", "value")
		go s.SetMetadata("root", "key", "value")
		defer s.SetMetadata("root", "key", "value")
	}
	func checked(s store) error {
		if err := s.SetMetadata("root", "key", "value"); err != nil { return err }
		err := s.SetMetadata("root", "key", "value")
		if err != nil { return err }
		return s.SetMetadata("root", "key", "value")
	}`

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fixture.go", source, 0)
	if err != nil {
		t.Fatalf("parsing guard fixture: %v", err)
	}
	ignored := ignoredSetMetadataCalls(file)
	if len(ignored) != 4 {
		var positions []string
		for _, call := range ignored {
			positions = append(positions, fset.Position(call.Pos()).String())
		}
		t.Fatalf("ignored calls = %d at %v, want all 4 discard forms", len(ignored), positions)
	}
}

func ignoredSetMetadataCalls(file *ast.File) []*ast.CallExpr {
	parents := make(map[ast.Node]ast.Node)
	var stack []ast.Node
	ast.Inspect(file, func(node ast.Node) bool {
		if node == nil {
			stack = stack[:len(stack)-1]
			return true
		}
		if len(stack) != 0 {
			parents[node] = stack[len(stack)-1]
		}
		stack = append(stack, node)
		return true
	})

	var ignored []*ast.CallExpr
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || !isSetMetadataSelectorCall(call) {
			return true
		}

		expr := ast.Node(call)
		parent := parents[expr]
		for {
			if _, ok := parent.(*ast.ParenExpr); !ok {
				break
			}
			expr = parent
			parent = parents[parent]
		}

		switch statement := parent.(type) {
		case *ast.ExprStmt, *ast.GoStmt, *ast.DeferStmt:
			ignored = append(ignored, call)
		case *ast.AssignStmt:
			for i, rhs := range statement.Rhs {
				if rhs != expr || len(statement.Lhs) != len(statement.Rhs) {
					continue
				}
				if ident, ok := statement.Lhs[i].(*ast.Ident); ok && ident.Name == "_" {
					ignored = append(ignored, call)
				}
			}
		case *ast.ValueSpec:
			for i, value := range statement.Values {
				if value != expr || len(statement.Names) != len(statement.Values) {
					continue
				}
				if statement.Names[i].Name == "_" {
					ignored = append(ignored, call)
				}
			}
		}
		return true
	})
	return ignored
}

func isSetMetadataSelectorCall(call *ast.CallExpr) bool {
	selector, ok := call.Fun.(*ast.SelectorExpr)
	return ok && selector.Sel.Name == "SetMetadata"
}
