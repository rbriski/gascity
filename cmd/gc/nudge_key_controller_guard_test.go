package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNudgeKeyControllerHasNoProductionActivation pins this bootstrap slice's
// ownership boundary. The queue implementation is compiled into production,
// but no non-test path may construct it until the shadow activation gate lands.
func TestNudgeKeyControllerHasNoProductionActivation(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("ReadDir(%q): %v", dir, err)
	}

	constructorLiterals := 0
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		path := filepath.Join(dir, name)
		source, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("ReadFile(%q): %v", path, err)
		}
		file, err := parser.ParseFile(fset, path, source, 0)
		if err != nil {
			t.Fatalf("ParseFile(%q): %v", path, err)
		}
		var constructorDeclaration token.Pos
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok && fn.Name.Name == "newNudgeKeyController" {
				constructorDeclaration = fn.Name.Pos()
			}
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.Ident:
				if typed.Name == "newNudgeKeyController" && typed.Pos() != constructorDeclaration {
					t.Errorf("%s references the default-inert nudge keyed controller constructor at %s; activation requires the shadow ownership gate", name, fset.Position(typed.Pos()))
				}
			case *ast.CompositeLit:
				ident, ok := typed.Type.(*ast.Ident)
				if ok && ident.Name == "nudgeKeyController" {
					constructorLiterals++
					if name != "nudge_key_controller.go" {
						t.Errorf("%s constructs nudgeKeyController directly at %s; activation requires the shadow ownership gate", name, fset.Position(typed.Pos()))
					}
				}
			case *ast.CallExpr:
				builtin, ok := typed.Fun.(*ast.Ident)
				if !ok || builtin.Name != "new" || len(typed.Args) != 1 {
					break
				}
				ident, ok := typed.Args[0].(*ast.Ident)
				if ok && ident.Name == "nudgeKeyController" {
					t.Errorf("%s constructs nudgeKeyController with new at %s; activation requires the shadow ownership gate", name, fset.Position(typed.Pos()))
				}
			}
			return true
		})
	}
	if constructorLiterals != 1 {
		t.Fatalf("production nudgeKeyController composite literals = %d, want only the constructor-owned literal", constructorLiterals)
	}
}
