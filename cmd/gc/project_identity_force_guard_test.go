package main

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func TestOnlyBdDoctorReseedCallsUpsertProjectIDForce(t *testing.T) {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Dir(file)
	fset := token.NewFileSet()
	var callers []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileAST, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			return err
		}
		ast.Inspect(fileAST, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			ident, ok := call.Fun.(*ast.Ident)
			if !ok || ident.Name != "upsertDatabaseProjectIDForce" {
				return true
			}
			pos := fset.Position(call.Pos())
			callers = append(callers, filepath.Base(path)+":"+strconv.Itoa(pos.Line))
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(callers) != 1 || !strings.HasPrefix(callers[0], "cmd_bd_doctor.go:") {
		t.Fatalf("upsertDatabaseProjectIDForce callers = %v, want only cmd_bd_doctor.go", callers)
	}
}
