package main

import (
	"go/ast"
	"go/importer"
	"go/token"
	"go/types"
	"strings"
	"testing"
)

func TestNudgeKeyReconcilerProductionSurfaceUsesReadOnlyPagerAndHasNoCapabilityEscape(t *testing.T) {
	fset, file := parseGCTestSource(t, "nudge_key_reconciler.go")
	wantImports := map[string]bool{
		"context": true,
		"errors":  true,
		"fmt":     true,
		"github.com/gastownhall/gascity/internal/nudgequeue":   true,
		"github.com/gastownhall/gascity/internal/reconcilekey": true,
	}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, "\"")
		if !wantImports[path] {
			t.Errorf("keyed nudge reconciler imports unapproved capability %q at %s", path, fset.Position(imported.Pos()))
		}
		delete(wantImports, path)
	}
	for missing := range wantImports {
		t.Errorf("keyed nudge reconciler import allowlist is stale: %q is absent", missing)
	}

	assertASTCallsOnly(t, fset, file, "keyed nudge reconciler", map[string]bool{
		"errors.New":                             true,
		"fmt.Errorf":                             true,
		"fmt.Sprintf":                            true,
		"nudgequeue.ValidateCommandStoreBinding": true,
		"nudgeCommandReconcileScope":             true,
		"r.ReconcilePage":                        true,
		"ctx.Err":                                true,
		"key.IsZero":                             true,
		"key.StoreID":                            true,
		"r.pager.Page":                           true,
		"key.SessionID":                          true,
		"inspectNudgeKeyPageEntry":               true,
		"errors.Is":                              true,
		"len":                                    true,
	})

	// These helpers are part of the reviewed surface. Keeping their bodies in
	// this file prevents an allowed call name from hiding an unscanned helper
	// that was moved behind a capability-bearing implementation.
	findGCFunction(t, file, "nudgeCommandReconcileScope")
	findGCFunction(t, file, "inspectNudgeKeyPageEntry")
	reconcilePage := findGCFunction(t, file, "ReconcilePage")
	typeInfo := &types.Info{
		Defs: make(map[*ast.Ident]types.Object),
		Uses: make(map[*ast.Ident]types.Object),
	}
	typeConfig := types.Config{
		Importer: importer.Default(),
		Error:    func(error) {},
	}
	_, _ = typeConfig.Check("github.com/gastownhall/gascity/cmd/gc", fset, []*ast.File{file}, typeInfo)

	var resultObject types.Object
	ast.Inspect(reconcilePage.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || assignment.Tok != token.DEFINE {
			return true
		}
		for _, lhs := range assignment.Lhs {
			ident, ok := lhs.(*ast.Ident)
			if !ok || ident.Name != "result" {
				continue
			}
			if resultObject != nil {
				t.Errorf("keyed nudge reconciler declares more than one result local at %s", fset.Position(ident.Pos()))
			}
			resultObject = typeInfo.Defs[ident]
		}
		return true
	})
	if resultObject == nil {
		t.Fatal("keyed nudge reconciler canonical result local was not found")
	}

	protectedNames := map[string]bool{
		"context":                    true,
		"errors":                     true,
		"fmt":                        true,
		"len":                        true,
		"nudgequeue":                 true,
		"reconcilekey":               true,
		"nudgeCommandReconcileScope": true,
		"inspectNudgeKeyPageEntry":   true,
	}
	allowedLocals := map[string]bool{
		"barrierSeen":      true,
		"ctxErr":           true,
		"entry":            true,
		"err":              true,
		"facts":            true,
		"hasCommand":       true,
		"hasOpaque":        true,
		"page":             true,
		"position":         true,
		"previousSequence": true,
		"result":           true,
	}
	for _, declaration := range file.Decls {
		if generic, ok := declaration.(*ast.GenDecl); ok && generic.Tok == token.VAR {
			t.Errorf("keyed nudge reconciler declares package state at %s", fset.Position(generic.Pos()))
		}
	}
	ast.Inspect(file, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.GoStmt:
			t.Errorf("keyed nudge reconciler starts a goroutine at %s", fset.Position(typed.Pos()))
		case *ast.SendStmt:
			t.Errorf("keyed nudge reconciler sends on a channel at %s", fset.Position(typed.Pos()))
		case *ast.AssignStmt:
			for _, lhs := range typed.Lhs {
				if !nudgeKeyReconcilerAssignmentAllowed(lhs, allowedLocals, resultObject, typeInfo) {
					t.Errorf("keyed nudge reconciler writes through %q at %s", selectorPath(lhs), fset.Position(lhs.Pos()))
				}
			}
			if typed.Tok == token.DEFINE {
				for _, lhs := range typed.Lhs {
					if ident, ok := lhs.(*ast.Ident); ok && protectedNames[ident.Name] {
						t.Errorf("keyed nudge reconciler shadows protected name %q at %s", ident.Name, fset.Position(ident.Pos()))
					}
				}
			}
		case *ast.RangeStmt:
			for _, target := range []ast.Expr{typed.Key, typed.Value} {
				if target != nil && !nudgeKeyReconcilerAssignmentAllowed(target, allowedLocals, resultObject, typeInfo) {
					t.Errorf("keyed nudge reconciler range writes through %q at %s", selectorPath(target), fset.Position(target.Pos()))
				}
			}
		case *ast.IncDecStmt:
			if !nudgeKeyReconcilerAssignmentAllowed(typed.X, allowedLocals, resultObject, typeInfo) {
				t.Errorf("keyed nudge reconciler increments through %q at %s", selectorPath(typed.X), fset.Position(typed.Pos()))
			}
		case *ast.ValueSpec:
			for _, name := range typed.Names {
				if protectedNames[name.Name] {
					t.Errorf("keyed nudge reconciler shadows protected name %q at %s", name.Name, fset.Position(name.Pos()))
				}
			}
		case *ast.Field:
			for _, name := range typed.Names {
				if protectedNames[name.Name] {
					t.Errorf("keyed nudge reconciler parameter shadows protected name %q at %s", name.Name, fset.Position(name.Pos()))
				}
			}
		case *ast.SelectorExpr:
			switch typed.Sel.Name {
			case "Raw", "Message", "CommandID":
				t.Errorf("keyed nudge reconciler reaches identity/content field %q at %s", typed.Sel.Name, fset.Position(typed.Pos()))
			}
		}
		return true
	})
}

func nudgeKeyReconcilerAssignmentAllowed(expr ast.Expr, allowedLocals map[string]bool, resultObject types.Object, typeInfo *types.Info) bool {
	switch typed := expr.(type) {
	case *ast.Ident:
		object := typeInfo.ObjectOf(typed)
		variable, ok := object.(*types.Var)
		if !allowedLocals[typed.Name] || !ok || variable.Parent() == nil {
			return false
		}
		if typed.Name == "result" {
			return object == resultObject
		}
		return variable.Pkg() == nil || variable.Parent() != variable.Pkg().Scope()
	case *ast.SelectorExpr:
		root := nudgeKeyReconcilerSelectorRoot(typed)
		return root != nil && typeInfo.ObjectOf(root) == resultObject
	default:
		return false
	}
}

func nudgeKeyReconcilerSelectorRoot(expr ast.Expr) *ast.Ident {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed
	case *ast.SelectorExpr:
		return nudgeKeyReconcilerSelectorRoot(typed.X)
	default:
		return nil
	}
}
