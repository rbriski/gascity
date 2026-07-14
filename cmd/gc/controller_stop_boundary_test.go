package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"
)

func TestControllerStopWireStaysConfinedToTypedClientAndServer(t *testing.T) {
	files := parseControllerStopProductionFiles(t)
	wantWire := map[string]int{
		"cmd_supervisor.go:stopSupervisorViaSocketJSON:\"stop\\n\"": 1,
		"controller_stop_client.go:stop:\"stop\\n\"":                1,
		"controller_stop_client.go:stop:\"stop-force\\n\"":          1,
	}
	gotWire := make(map[string]int)

	for name, file := range files {
		ast.Inspect(file, func(node ast.Node) bool {
			ident, ok := node.(*ast.Ident)
			if ok && (ident.Name == "tryStopController" || ident.Name == "tryStopControllerWithForce") {
				t.Errorf("%s contains forbidden bool-valued controller stop API %s", name, ident.Name)
			}
			return true
		})

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CallExpr:
					callee := controllerStopCalledIdent(n.Fun)
					if callee == "controllerSocketPath" && name != "controller.go" && name != "controller_stop_client.go" && name != "controller_lock.go" {
						t.Errorf("%s:%s calls controllerSocketPath outside the server/typed client boundary", name, fn.Name.Name)
					}
					if strings.HasPrefix(callee, "sendControllerCommand") && len(n.Args) >= 2 {
						if command, ok := controllerStopStringLiteral(n.Args[1]); ok {
							line := controllerStopFirstWireLine(command)
							if line == "stop" || line == "stop-force" {
								t.Errorf("%s:%s routes %q through generic controller transport", name, fn.Name.Name, command)
							}
						}
					}
				case *ast.BasicLit:
					value, ok := controllerStopStringLiteral(n)
					if !ok || (value != "stop\n" && value != "stop-force\n") {
						break
					}
					key := fmt.Sprintf("%s:%s:%s", name, fn.Name.Name, strconv.Quote(value))
					gotWire[key]++
				}
				return true
			})
		}
	}

	if diff := controllerStopInventoryDiff(wantWire, gotWire); diff != "" {
		t.Fatalf("controller stop wire inventory changed; route per-city shutdown only through controller_stop_client.go:\n%s", diff)
	}
}

func TestControllerStopProductionCallSitesStayTriState(t *testing.T) {
	files := parseControllerStopProductionFiles(t)
	wantRequests := map[string]int{
		"cmd_stop.go:cmdStopJSON": 1,
		"cmd_supervisor_city.go:unregisterCityFromSupervisorWithOptionsResult": 2,
	}
	wantConsumers := map[string]bool{
		"cmd_stop.go:cmdStopJSON":                                              true,
		"cmd_stop.go:cmdStopBodyWithHeldOwnership":                             true,
		"cmd_supervisor_city.go:unregisterCityFromSupervisorWithOptionsResult": true,
	}
	gotRequests := make(map[string]int)
	seenConsumers := make(map[string]bool)
	required := []string{
		"controllerStopAcknowledged",
		"controllerStopDefinitePreEntryUnavailable",
		"controllerStopMayHaveEntered",
		"controllerStopOutcomeInvalid",
		"failClosedError",
	}

	for name, file := range files {
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			key := name + ":" + fn.Name.Name
			refs := make(map[string]bool)
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CallExpr:
					callee := controllerStopCalledIdent(n.Fun)
					if callee == "controllerStopRequestForCommand" || callee == "controllerStopRequestUntilForCommand" || callee == "sendControllerStop" {
						gotRequests[key]++
					}
				case *ast.Ident:
					refs[n.Name] = true
				case *ast.SelectorExpr:
					refs[n.Sel.Name] = true
				}
				return true
			})
			if !wantConsumers[key] {
				continue
			}
			seenConsumers[key] = true
			for _, symbol := range required {
				if !refs[symbol] {
					t.Errorf("%s does not explicitly preserve controller stop state %s", key, symbol)
				}
			}
		}
	}

	if diff := controllerStopInventoryDiff(wantRequests, gotRequests); diff != "" {
		t.Fatalf("controller stop request call-site inventory changed; every new caller requires ownership review:\n%s", diff)
	}
	for key := range wantConsumers {
		if !seenConsumers[key] {
			t.Errorf("required tri-state consumer %s not found", key)
		}
	}
}

func parseControllerStopProductionFiles(t *testing.T) map[string]*ast.File {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(currentFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	fset := token.NewFileSet()
	files := make(map[string]*ast.File)
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		files[name] = file
	}
	return files
}

func controllerStopCalledIdent(expr ast.Expr) string {
	switch e := expr.(type) {
	case *ast.Ident:
		return e.Name
	case *ast.SelectorExpr:
		return e.Sel.Name
	default:
		return ""
	}
}

func controllerStopStringLiteral(expr ast.Expr) (string, bool) {
	lit, ok := expr.(*ast.BasicLit)
	if !ok || lit.Kind != token.STRING {
		return "", false
	}
	value, err := strconv.Unquote(lit.Value)
	return value, err == nil
}

func controllerStopFirstWireLine(command string) string {
	if newline := strings.IndexByte(command, '\n'); newline >= 0 {
		command = command[:newline]
	}
	return strings.TrimSuffix(command, "\r")
}

func controllerStopInventoryDiff(want, got map[string]int) string {
	keys := make(map[string]struct{}, len(want)+len(got))
	for key := range want {
		keys[key] = struct{}{}
	}
	for key := range got {
		keys[key] = struct{}{}
	}
	ordered := make([]string, 0, len(keys))
	for key := range keys {
		ordered = append(ordered, key)
	}
	sort.Strings(ordered)
	var lines []string
	for _, key := range ordered {
		if want[key] != got[key] {
			lines = append(lines, fmt.Sprintf("  %s: got %d, want %d", key, got[key], want[key]))
		}
	}
	return strings.Join(lines, "\n")
}
