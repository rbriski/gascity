package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestNudgeKeyControllerProductionActivationIsExactlyShadow pins the read-only
// production activation boundary. Exactly one reviewed CityRuntime method may
// construct the controller, and its callback must be the certified reader.
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
	certifiedReaderCallbacks := 0
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
						t.Errorf("%s assigns a production .reconcile callback at %s; the read-only callback is immutable after certified construction", name, fset.Position(selector.Pos()))
					}
				}
			case *ast.Ident:
				if typed.Name == "newNudgeKeyController" && typed.Pos() != constructorDeclaration {
					constructorReferences++
					owner := ownerAt(typed.Pos())
					if name == "city_runtime.go" && owner == "installNudgeKeyShadow" {
						allowedShadowReferences++
					} else {
						t.Errorf("%s references the nudge keyed controller constructor from %s at %s; only city_runtime.go:installNudgeKeyShadow may activate the read-only shadow", name, owner, fset.Position(typed.Pos()))
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
					workers, ok := typed.Args[0].(*ast.BasicLit)
					if !ok || workers.Kind != token.INT || workers.Value != "1" {
						t.Errorf("shadow constructor worker count at %s is not the literal 1", fset.Position(typed.Args[0].Pos()))
						break
					}
					if selectorPath(typed.Args[2]) != "stderr" {
						t.Errorf("shadow constructor warning sink at %s is not the normalized stderr", fset.Position(typed.Args[2].Pos()))
						break
					}
					if selectorPath(typed.Args[1]) != "reader.reconcile" {
						t.Errorf("shadow constructor callback at %s is %q, want certified reader.reconcile", fset.Position(typed.Args[1].Pos()), selectorPath(typed.Args[1]))
						break
					}
					certifiedReaderCallbacks++
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
	if certifiedReaderCallbacks != 1 {
		t.Fatalf("certified production nudge reader callbacks = %d, want exactly one", certifiedReaderCallbacks)
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

func TestNudgeKeyReadShadowConstructionUsesNudgeClassAndDurableBinding(t *testing.T) {
	fset, file := parseGCTestSource(t, "city_runtime.go")
	install := findGCFunction(t, file, "installNudgeKeyShadow")
	wantCalls := map[string]int{
		"cr.nudgesBeadStore":         1,
		"newNudgeKeyReadShadow":      1,
		"nudgeCommandReconcileScope": 1,
		"newNudgeKeyController":      1,
	}
	gotCalls := make(map[string]int)
	ast.Inspect(install.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		name := selectorPath(call.Fun)
		if _, tracked := wantCalls[name]; tracked {
			gotCalls[name]++
		}
		if name == "contract.ReadProjectIdentity" || name == "reconcilekey.NewSession" {
			t.Errorf("install derives scope from non-store identity through %s at %s", name, fset.Position(call.Pos()))
		}
		return true
	})
	for name, want := range wantCalls {
		if got := gotCalls[name]; got != want {
			t.Errorf("install call count %s = %d, want %d", name, got, want)
		}
	}

	readerFSet, readerFile := parseGCTestSource(t, "nudge_key_production_reader.go")
	allowedImports := map[string]bool{
		"context": true, "errors": true, "fmt": true, "sync/atomic": true, "time": true,
		"github.com/gastownhall/gascity/internal/beads":        true,
		"github.com/gastownhall/gascity/internal/nudgequeue":   true,
		"github.com/gastownhall/gascity/internal/reconcilekey": true,
	}
	for _, imported := range readerFile.Imports {
		path := strings.Trim(imported.Path.Value, "\"")
		if !allowedImports[path] {
			t.Errorf("nudge keyed reader imports unapproved capability %q at %s", path, readerFSet.Position(imported.Pos()))
		}
		delete(allowedImports, path)
	}
	for missing := range allowedImports {
		t.Errorf("nudge keyed reader import guard is stale: expected %q", missing)
	}
	forbiddenCalls := map[string]bool{
		"Create": true, "Update": true, "Delete": true, "Close": true,
		"SetMetadata": true, "AtomicReadWrite": true,
		"Nudge": true, "Start": true, "Stop": true,
	}
	ast.Inspect(readerFile, func(node ast.Node) bool {
		switch typed := node.(type) {
		case *ast.CallExpr:
			path := selectorPath(typed.Fun)
			parts := strings.Split(path, ".")
			if forbiddenCalls[parts[len(parts)-1]] {
				t.Errorf("nudge keyed reader calls effect capability %q at %s", path, readerFSet.Position(typed.Pos()))
			}
		case *ast.GoStmt:
			t.Errorf("nudge keyed reader starts a goroutine at %s", readerFSet.Position(typed.Pos()))
		case *ast.SendStmt:
			t.Errorf("nudge keyed reader sends on a channel at %s", readerFSet.Position(typed.Pos()))
		}
		return true
	})
}

func TestNudgeKeySchedulingObserverIsIdentityFreeAndEffectFree(t *testing.T) {
	fset, file := parseGCTestSource(t, "nudge_key_observation.go")
	wantImports := map[string]bool{
		"context": true,
		"io":      true,
		"sync":    true,
		"time":    true,
		"github.com/gastownhall/gascity/internal/telemetry": true,
	}
	for _, imported := range file.Imports {
		path := strings.Trim(imported.Path.Value, "\"")
		if !wantImports[path] {
			t.Errorf("nudge keyed scheduling observer imports unapproved capability %q at %s", path, fset.Position(imported.Pos()))
		}
		delete(wantImports, path)
	}
	for missing := range wantImports {
		t.Errorf("nudge keyed scheduling observer allowlist is stale: expected import %q is absent", missing)
	}

	allowedCalls := map[string]bool{
		"uint8":                              true,
		"<func>":                             true,
		"batch.FirstEnqueuedAt.IsZero":       true,
		"now.Sub":                            true,
		"newNudgeKeySchedulingObservation":   true,
		"emitNudgeKeySchedulingObservation":  true,
		"invokeNudgeKeySchedulingEmitter":    true,
		"recover":                            true,
		"warnings.warn":                      true,
		"warnings.once.Do":                   true,
		"writeNudgeKeyObservationWarning":    true,
		"io.WriteString":                     true,
		"emit":                               true,
		"telemetry.RecordNudgeKeyScheduling": true,
		"telemetry.NudgeKeySchedulingRecord": true,
	}
	assertASTCallsOnly(t, fset, file, "nudge keyed scheduling observer", allowedCalls)
	assertASTHasNoCapabilityEscape(t, fset, file, "nudge keyed scheduling observer", map[string]bool{
		"observation": true,
		"delay":       true,
		"delayState":  true,
		"failed":      true,
		"_":           true,
	})

	observe := findGCFunction(t, file, "observeNudgeKeyScheduling")
	if got := len(observe.Type.Params.List); got != 4 {
		t.Fatalf("observeNudgeKeyScheduling parameters = %d, want context, batch, time, and bounded warning state only", got)
	}
	for _, param := range observe.Type.Params.List {
		if selectorPath(param.Type) == "reconcilekey.Session" {
			t.Errorf("observeNudgeKeyScheduling accepts a stable key at %s; identity must be discarded by its caller", fset.Position(param.Pos()))
		}
	}
}

func TestNudgeKeySchedulingTelemetryRecorderIsTheSoleCapabilityBoundary(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repoRoot := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", ".."))
	recorderPath := filepath.Join(repoRoot, "internal", "telemetry", "recorder_nudge_key.go")
	fset := token.NewFileSet()
	recorder, err := parser.ParseFile(fset, recorderPath, nil, 0)
	if err != nil {
		t.Fatalf("ParseFile(%q): %v", recorderPath, err)
	}
	wantImports := map[string]bool{
		"context":                            true,
		"errors":                             true,
		"fmt":                                true,
		"sync":                               true,
		"sync/atomic":                        true,
		"time":                               true,
		"go.opentelemetry.io/otel":           true,
		"go.opentelemetry.io/otel/attribute": true,
		"go.opentelemetry.io/otel/metric":    true,
	}
	for _, imported := range recorder.Imports {
		path := strings.Trim(imported.Path.Value, "\"")
		if !wantImports[path] {
			t.Errorf("nudge scheduling telemetry recorder imports unapproved capability %q at %s", path, fset.Position(imported.Pos()))
		}
		delete(wantImports, path)
	}
	for missing := range wantImports {
		t.Errorf("nudge scheduling telemetry recorder allowlist is stale: expected import %q is absent", missing)
	}
	assertASTCallsOnly(t, fset, recorder, "nudge scheduling telemetry recorder", map[string]bool{
		"loadNudgeKeySchedulingInstruments":               true,
		"int64":                                           true,
		"attribute.Int64":                                 true,
		"attribute.Bool":                                  true,
		"attribute.String":                                true,
		"nudgeKeyQueueDelayStateLabel":                    true,
		"metric.WithAttributes":                           true,
		"snapshot.instruments.total.Add":                  true,
		"snapshot.instruments.queueDelay.Record":          true,
		"float64":                                         true,
		"nudgeKeySchedulingInstrumentState.current.Load":  true,
		"nudgeKeySchedulingInstrumentState.mu.Lock":       true,
		"nudgeKeySchedulingInstrumentState.mu.Unlock":     true,
		"otel.GetMeterProvider":                           true,
		"Meter":                                           true,
		"meter.Int64Counter":                              true,
		"meter.Float64Histogram":                          true,
		"metric.WithDescription":                          true,
		"metric.WithUnit":                                 true,
		"errors.Join":                                     true,
		"wrapNudgeKeySchedulingInstrumentError":           true,
		"nudgeKeySchedulingInstrumentState.current.Store": true,
		"fmt.Errorf":                                      true,
	})

	type caller struct {
		path  string
		owner string
	}
	var callers []caller
	err = filepath.Walk(repoRoot, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "vendor" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fileSet := token.NewFileSet()
		file, parseErr := parser.ParseFile(fileSet, path, nil, 0)
		if parseErr != nil {
			return parseErr
		}
		aliases := make(map[string]bool)
		for _, imported := range file.Imports {
			if strings.Trim(imported.Path.Value, "\"") != "github.com/gastownhall/gascity/internal/telemetry" {
				continue
			}
			alias := "telemetry"
			if imported.Name != nil {
				alias = imported.Name.Name
			}
			if alias == "." {
				return fmt.Errorf("production file %s dot-imports internal/telemetry", path)
			}
			aliases[alias] = true
		}
		var functions []*ast.FuncDecl
		for _, declaration := range file.Decls {
			if fn, ok := declaration.(*ast.FuncDecl); ok {
				functions = append(functions, fn)
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
			selector, ok := node.(*ast.SelectorExpr)
			if !ok || selector.Sel.Name != "RecordNudgeKeyScheduling" {
				return true
			}
			base, ok := selector.X.(*ast.Ident)
			if !ok || !aliases[base.Name] {
				return true
			}
			relative, relErr := filepath.Rel(repoRoot, path)
			if relErr != nil {
				t.Errorf("Rel(%q): %v", path, relErr)
				return true
			}
			callers = append(callers, caller{path: filepath.ToSlash(relative), owner: ownerAt(selector.Pos())})
			return true
		})
		return nil
	})
	if err != nil {
		t.Fatalf("walking production callers: %v", err)
	}
	if len(callers) != 1 || callers[0].path != "cmd/gc/nudge_key_observation.go" || callers[0].owner != "recordNudgeKeySchedulingObservation" {
		t.Fatalf("production RecordNudgeKeyScheduling callers = %+v, want only cmd/gc/nudge_key_observation.go:recordNudgeKeySchedulingObservation", callers)
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
		"make":   true, "len": true, "uint8": true, "close": true, "clear": true, "delete": true, "recover": true,
		"cancelWorkers": true, "onClosed": true,
		"fmt.Errorf": true, "fmt.Fprintf": true, "context.WithCancel": true, "debug.Stack": true,
		"workqueue.NewTypedItemExponentialFailureRateLimiter": true, "workqueue.NewTypedRateLimitingQueue": true,
		"normalizeNudgeKeyControllerOptions": true, "outcome.validate": true,
		"key.IsZero": true, "ctx.Err": true, "ctx.Done": true,
		"c.mu.Lock": true, "c.mu.Unlock": true, "c.stderrMu.Lock": true, "c.stderrMu.Unlock": true, "c.now": true,
		"c.queue.Add": true, "c.queue.ShutDown": true, "c.queue.Get": true, "c.queue.Done": true, "c.queue.Forget": true,
		"workers.Add": true, "workers.Done": true, "workers.Wait": true,
		"c.runWorker": true, "c.closeAdmission": true, "c.takeBatch": true, "c.invoke": true,
		"c.restoreBatch": true, "c.restoreBatchLocked": true, "c.deferBatch": true, "c.reportFailure": true, "c.reconcile": true,
		"c.afterGet": true, "c.onEmptyReplay": true, "c.onDeferred": true, "c.onForget": true, "c.addAfter": true,
		"c.limiter.When": true, "c.logTransient": true,
		"failed.FirstEnqueuedAt.IsZero": true, "failed.FirstEnqueuedAt.Before": true,
		"pending.FirstEnqueuedAt.IsZero": true, "now.Before": true, "now.Add": true,
	}
	assertASTCallsOnly(t, fset, file, "nudge keyed core", allowedCalls)
}

func TestNudgeExactIngressCallGraphRemainsReadOnly(t *testing.T) {
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
		"ctx.Err":                             true,
		"cr.nudgeKeyReader.acceptCommandHint": true,
		"cr.warnNudgeKeyHintDiagnostic":       true,
		"cr.enqueueNudgeKeyShadow":            true,
	})
	assertASTHasNoCapabilityEscape(t, cityFSet, accept, "CityRuntime exact admission", map[string]bool{
		"sessionID": true, "accepted": true, "err": true,
	})

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
	var installPos, listenerPos, readyPos token.Pos
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
		if calledFunction(call) == "cr.installNudgeKeyShadow" {
			if installPos != token.NoPos {
				t.Errorf("multiple production nudge reader installs: %s and %s", fset.Position(installPos), fset.Position(call.Pos()))
			}
			installPos = call.Pos()
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
	if installPos == token.NoPos || listenerPos == token.NoPos || readyPos == token.NoPos {
		t.Fatalf("install/listener/readiness positions = %s/%s/%s, want all production calls", fset.Position(installPos), fset.Position(listenerPos), fset.Position(readyPos))
	}
	if installPos >= listenerPos {
		t.Fatalf("nudge reader installs at %s after ingress opens at %s", fset.Position(installPos), fset.Position(listenerPos))
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
