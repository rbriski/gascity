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

// TestNudgeKeyControllerProductionActivationIsExactlyShadow pins the first
// production activation boundary. Exactly one reviewed CityRuntime method may
// construct the controller; every other non-test path remains forbidden.
func TestNudgeKeyControllerProductionActivationIsExactlyShadow(t *testing.T) {
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
	constructorReferences := 0
	allowedShadowReferences := 0
	effectFreeShadowCallbacks := 0
	constructorCallbackBindings := 0
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
		var functions []*ast.FuncDecl
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				functions = append(functions, fn)
				if fn.Name.Name == "newNudgeKeyController" {
					constructorDeclaration = fn.Name.Pos()
				}
			}
		}
		ownerAt := func(pos token.Pos) string {
			for _, fn := range functions {
				if pos >= fn.Pos() && pos <= fn.End() {
					return fn.Name.Name
				}
			}
			return ""
		}
		ast.Inspect(file, func(node ast.Node) bool {
			switch typed := node.(type) {
			case *ast.AssignStmt:
				for _, lhs := range typed.Lhs {
					selector, ok := lhs.(*ast.SelectorExpr)
					if ok && selector.Sel.Name == "reconcile" {
						t.Errorf("%s assigns a production .reconcile callback at %s; the shadow callback is immutable until an effect-ownership gate", name, fset.Position(selector.Pos()))
					}
				}
			case *ast.Ident:
				if typed.Name == "newNudgeKeyController" && typed.Pos() != constructorDeclaration {
					constructorReferences++
					owner := ownerAt(typed.Pos())
					if name == "city_runtime.go" && owner == "installNudgeKeyShadow" {
						allowedShadowReferences++
					} else {
						t.Errorf("%s references the nudge keyed controller constructor from %s at %s; only city_runtime.go:installNudgeKeyShadow may activate the effect-free shadow", name, owner, fset.Position(typed.Pos()))
					}
				}
			case *ast.CompositeLit:
				ident, ok := typed.Type.(*ast.Ident)
				if ok && ident.Name == "nudgeKeyController" {
					constructorLiterals++
					if name != "nudge_key_controller.go" {
						t.Errorf("%s constructs nudgeKeyController directly at %s; activation requires the shadow ownership gate", name, fset.Position(typed.Pos()))
					}
					for _, element := range typed.Elts {
						field, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, keyOK := field.Key.(*ast.Ident)
						value, valueOK := field.Value.(*ast.Ident)
						if keyOK && key.Name == "reconcile" {
							if !valueOK || value.Name != "reconcile" {
								t.Errorf("constructor reconcile field at %s is not bound directly to its callback parameter", fset.Position(field.Pos()))
							} else {
								constructorCallbackBindings++
							}
						}
					}
				}
			case *ast.CallExpr:
				if constructor, ok := typed.Fun.(*ast.Ident); ok &&
					constructor.Name == "newNudgeKeyController" &&
					name == "city_runtime.go" && ownerAt(typed.Pos()) == "installNudgeKeyShadow" {
					if len(typed.Args) != 3 {
						t.Errorf("shadow constructor at %s has %d args, want fixed worker/callback/stderr contract", fset.Position(typed.Pos()), len(typed.Args))
						break
					}
					callback, ok := typed.Args[1].(*ast.FuncLit)
					if !ok || len(callback.Body.List) != 0 {
						t.Errorf("shadow constructor callback at %s is not an empty function literal; effectful activation requires a later ownership gate", fset.Position(typed.Args[1].Pos()))
						break
					}
					effectFreeShadowCallbacks++
				}
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
	if constructorReferences != 1 || allowedShadowReferences != 1 {
		t.Fatalf("production nudge keyed constructor references = %d (allowed shadow = %d), want exactly one reviewed shadow activation", constructorReferences, allowedShadowReferences)
	}
	if effectFreeShadowCallbacks != 1 {
		t.Fatalf("effect-free production nudge shadow callbacks = %d, want exactly one empty callback", effectFreeShadowCallbacks)
	}
	if constructorCallbackBindings != 1 {
		t.Fatalf("nudge keyed constructor callback field bindings = %d, want direct parameter binding", constructorCallbackBindings)
	}
}

func TestNudgeKeyShadowMapperHasNoEffectfulCalls(t *testing.T) {
	fset, file := parseGCTestSource(t, "city_runtime.go")
	fn := findGCFunction(t, file, "enqueueNudgeKeyShadow")
	wantCalls := map[string]int{
		"fmt.Errorf":                    2,
		"reconcilekey.NewSession":       1,
		"cr.nudgeKeyController.Enqueue": 1,
	}
	gotCalls := make(map[string]int)
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			name := selectorPath(typed.Fun)
			if _, ok := wantCalls[name]; !ok {
				t.Errorf("enqueueNudgeKeyShadow calls unapproved %q at %s; shadow ingress may only validate, key, and enqueue", name, fset.Position(typed.Pos()))
			} else {
				gotCalls[name]++
			}
		case *ast.GoStmt:
			t.Errorf("enqueueNudgeKeyShadow starts a goroutine at %s", fset.Position(typed.Pos()))
		case *ast.SendStmt:
			t.Errorf("enqueueNudgeKeyShadow sends on a channel at %s", fset.Position(typed.Pos()))
		}
		return true
	})
	for name, want := range wantCalls {
		if got := gotCalls[name]; got != want {
			t.Errorf("enqueueNudgeKeyShadow call count %s = %d, want %d", name, got, want)
		}
	}
}

func TestNudgeKeyControllerCoreCallSurfaceIsCapabilityFree(t *testing.T) {
	fset, file := parseGCTestSource(t, "nudge_key_controller.go")
	wantImports := map[string]bool{
		"context":       true,
		"fmt":           true,
		"io":            true,
		"runtime/debug": true,
		"sync":          true,
		"time":          true,
		"github.com/gastownhall/gascity/internal/reconcilekey": true,
		"k8s.io/client-go/util/workqueue":                      true,
	}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, "\"")
		if !wantImports[path] {
			t.Errorf("nudge keyed core imports unapproved capability %q at %s", path, fset.Position(imported.Pos()))
		}
		delete(wantImports, path)
	}
	for missing := range wantImports {
		t.Errorf("nudge keyed core allowlist is stale: expected import %q is absent", missing)
	}
	allowedCalls := map[string]bool{
		"<func>": true,
		"make":   true, "uint8": true, "close": true, "clear": true, "delete": true, "recover": true,
		"cancelWorkers": true, "onClosed": true,
		"fmt.Errorf": true, "fmt.Fprintf": true, "workqueue.NewTyped": true, "context.WithCancel": true, "debug.Stack": true,
		"key.IsZero": true, "ctx.Err": true, "ctx.Done": true,
		"c.mu.Lock": true, "c.mu.Unlock": true, "c.now": true,
		"c.queue.Add": true, "c.queue.ShutDown": true, "c.queue.Get": true, "c.queue.Done": true,
		"workers.Add": true, "workers.Done": true, "workers.Wait": true,
		"c.runWorker": true, "c.closeAdmission": true, "c.takeBatch": true, "c.invoke": true,
		"c.restoreBatch": true, "c.reportFailure": true, "c.reconcile": true,
		"c.afterGet": true, "c.onEmptyReplay": true,
		"failed.FirstEnqueuedAt.IsZero": true, "failed.FirstEnqueuedAt.Before": true,
		"pending.FirstEnqueuedAt.IsZero": true,
	}
	assertASTCallsOnly(t, fset, file, "nudge keyed core", allowedCalls)
}

func TestNudgeExactIngressCallGraphRemainsEffectFree(t *testing.T) {
	cityFSet, cityFile := parseGCTestSource(t, "city_runtime.go")
	run := findGCFunction(t, cityFile, "run")
	var onExact *ast.FuncLit
	ast.Inspect(run.Body, func(node ast.Node) bool {
		assignment, ok := node.(*ast.AssignStmt)
		if !ok || len(assignment.Lhs) != 1 || len(assignment.Rhs) != 1 {
			return true
		}
		name, ok := assignment.Lhs[0].(*ast.Ident)
		callback, callbackOK := assignment.Rhs[0].(*ast.FuncLit)
		if ok && callbackOK && name.Name == "onExact" {
			if onExact != nil {
				t.Errorf("multiple onExact callbacks at %s and %s", cityFSet.Position(onExact.Pos()), cityFSet.Position(callback.Pos()))
			}
			onExact = callback
		}
		return true
	})
	if onExact == nil {
		t.Fatal("CityRuntime.run exact nudge callback not found")
	}
	assertASTCallsOnly(t, cityFSet, onExact, "CityRuntime exact ingress", map[string]bool{
		"cr.acceptNudgeKeyShadowHint": true,
	})
	assertASTHasNoCapabilityEscape(t, cityFSet, onExact, "CityRuntime exact ingress", nil)
	accept := findGCFunction(t, cityFile, "acceptNudgeKeyShadowHint")
	assertASTCallsOnly(t, cityFSet, accept, "CityRuntime exact admission", map[string]bool{
		"ctx.Err": true, "cr.enqueueNudgeKeyShadow": true, "fmt.Fprintf": true,
	})
	assertASTHasNoCapabilityEscape(t, cityFSet, accept, "CityRuntime exact admission", map[string]bool{"err": true})

	dispatchFSet, dispatchFile := parseGCTestSource(t, "nudge_dispatcher.go")
	reader := findGCFunction(t, dispatchFile, "readNudgeWakeHintConnection")
	assertASTCallsOnly(t, dispatchFSet, reader, "exact hint framed reader", map[string]bool{
		"conn.Close": true, "conn.SetReadDeadline": true,
		"time.Now": true, "Add": true, "io.ReadAll": true, "io.LimitReader": true,
		"ctx.Err": true, "nudgequeue.DecodeSessionWakeHint": true, "invokeNudgeWakeHint": true,
	})
	assertASTHasNoCapabilityEscape(t, dispatchFSet, reader, "exact hint framed reader", map[string]bool{
		"_": true, "payload": true, "err": true, "hint": true, "ok": true,
	})
	invoke := findGCFunction(t, dispatchFile, "invokeNudgeWakeHint")
	assertASTCallsOnly(t, dispatchFSet, invoke, "exact hint callback wrapper", map[string]bool{
		"<func>": true, "recover": true, "fmt.Fprintf": true, "debug.Stack": true, "onExact": true,
	})
	assertASTHasNoCapabilityEscape(t, dispatchFSet, invoke, "exact hint callback wrapper", map[string]bool{"recovered": true})
}

func TestQueuedNudgeWakeIsStructurallyPostCommit(t *testing.T) {
	fset, file := parseGCTestSource(t, "cmd_nudge.go")
	fn := findGCFunction(t, file, "enqueueQueuedNudgeWithStore")
	if len(fn.Body.List) != 4 {
		t.Fatalf("enqueueQueuedNudgeWithStore statements = %d, want commit, error guard, wake, return", len(fn.Body.List))
	}
	commit, ok := fn.Body.List[0].(*ast.AssignStmt)
	if !ok || len(commit.Rhs) != 1 || calledFunction(commit.Rhs[0]) != "persistQueuedNudgeWithStore" {
		t.Fatalf("first statement at %s does not synchronously call persistQueuedNudgeWithStore", fset.Position(fn.Body.List[0].Pos()))
	}
	guard, ok := fn.Body.List[1].(*ast.IfStmt)
	if !ok || guard.Init != nil || len(guard.Body.List) != 1 {
		t.Fatalf("second statement at %s is not the commit-error guard", fset.Position(fn.Body.List[1].Pos()))
	}
	if _, ok := guard.Body.List[0].(*ast.ReturnStmt); !ok {
		t.Fatalf("commit error guard at %s does not return before wake", fset.Position(guard.Body.List[0].Pos()))
	}
	wake, ok := fn.Body.List[2].(*ast.ExprStmt)
	if !ok || calledFunction(wake.X) != "pingNudgeWakeSocketHint" {
		t.Fatalf("third statement at %s is not the post-commit wake", fset.Position(fn.Body.List[2].Pos()))
	}
	if _, ok := fn.Body.List[3].(*ast.ReturnStmt); !ok {
		t.Fatalf("fourth statement at %s is not successful return", fset.Position(fn.Body.List[3].Pos()))
	}
}

func TestCityRuntimeStartsNudgeIngressBeforePublishingReadiness(t *testing.T) {
	fset, file := parseGCTestSource(t, "city_runtime.go")
	fn := findGCFunction(t, file, "run")
	var listenerPos, readyPos token.Pos
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		if calledFunction(call) == "startNudgeWakeListenerWithHints" {
			if listenerPos != token.NoPos {
				t.Errorf("multiple production nudge listener starts: %s and %s", fset.Position(listenerPos), fset.Position(call.Pos()))
			}
			listenerPos = call.Pos()
		}
		return true
	})
	// The earlier `defer markReady()` only releases the startup watchdog on an
	// aborted run. The actual readiness publication is the sole top-level
	// expression statement in run.
	for _, statement := range fn.Body.List {
		expr, ok := statement.(*ast.ExprStmt)
		if ok && calledFunction(expr.X) == "markReady" {
			if readyPos != token.NoPos {
				t.Errorf("multiple top-level readiness publications: %s and %s", fset.Position(readyPos), fset.Position(expr.Pos()))
			}
			readyPos = expr.Pos()
		}
	}
	if listenerPos == token.NoPos || readyPos == token.NoPos {
		t.Fatalf("listener/readiness positions = %s/%s, want both production calls", fset.Position(listenerPos), fset.Position(readyPos))
	}
	if listenerPos >= readyPos {
		t.Fatalf("nudge listener starts at %s after readiness at %s", fset.Position(listenerPos), fset.Position(readyPos))
	}
}

func parseGCTestSource(t *testing.T, name string) (*token.FileSet, *ast.File) {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	path := filepath.Join(filepath.Dir(currentFile), name)
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, path, nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(%q): %v", path, err)
	}
	return fset, file
}

func findGCFunction(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	for _, declaration := range file.Decls {
		fn, ok := declaration.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("function %s not found", name)
	return nil
}

func calledFunction(expr ast.Expr) string {
	call, ok := expr.(*ast.CallExpr)
	if !ok {
		return ""
	}
	return selectorPath(call.Fun)
}

func selectorPath(expr ast.Expr) string {
	switch typed := expr.(type) {
	case *ast.Ident:
		return typed.Name
	case *ast.SelectorExpr:
		prefix := selectorPath(typed.X)
		if prefix == "" {
			return typed.Sel.Name
		}
		return prefix + "." + typed.Sel.Name
	case *ast.ParenExpr:
		return selectorPath(typed.X)
	case *ast.IndexExpr:
		return selectorPath(typed.X)
	case *ast.IndexListExpr:
		return selectorPath(typed.X)
	case *ast.FuncLit:
		return "<func>"
	default:
		return ""
	}
}

func assertASTCallsOnly(t *testing.T, fset *token.FileSet, root ast.Node, owner string, allowed map[string]bool) {
	t.Helper()
	ast.Inspect(root, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := selectorPath(call.Fun)
		if !allowed[name] {
			t.Errorf("%s calls unapproved %q at %s", owner, name, fset.Position(call.Pos()))
		}
		return true
	})
}

func assertASTHasNoCapabilityEscape(t *testing.T, fset *token.FileSet, root ast.Node, owner string, allowedAssignments map[string]bool) {
	t.Helper()
	ast.Inspect(root, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.GoStmt:
			t.Errorf("%s starts a goroutine at %s", owner, fset.Position(typed.Pos()))
		case *ast.SendStmt:
			t.Errorf("%s sends a capability over a channel at %s", owner, fset.Position(typed.Pos()))
		case *ast.AssignStmt:
			for _, lhs := range typed.Lhs {
				ident, ok := lhs.(*ast.Ident)
				if !ok || !allowedAssignments[ident.Name] {
					t.Errorf("%s assigns through unapproved target %q at %s", owner, selectorPath(lhs), fset.Position(lhs.Pos()))
				}
			}
		}
		return true
	})
}
