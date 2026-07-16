package main

import (
	"bytes"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/nudgequeue"
	"github.com/gastownhall/gascity/internal/runtime"
)

func TestRunControllerWithLeaseRequireRefusesBeforeStarted(t *testing.T) {
	cityPath := t.TempDir()
	lease, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-city"},
		Beads: config.BeadsConfig{
			CommandSecurityProfile: string(nudgequeue.CommandSecurityProfileStoreWriterIsController),
		},
		Daemon: config.DaemonConfig{
			NudgeDispatcher:  "supervisor",
			NudgeEffectOwner: "require",
		},
	}
	recorder := events.NewFake()
	var stdout, stderr bytes.Buffer
	code := runControllerWithLease(
		lease,
		cityPath,
		"",
		cfg,
		"",
		nil,
		nil,
		runtime.NewFake(),
		nil,
		nil,
		nil,
		nil,
		recorder,
		recorder,
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("runControllerWithLease code = %d, want 1", code)
	}
	if stdout.String() != "" {
		t.Fatalf("stdout = %q, want no premature startup success", stdout.String())
	}
	for _, want := range []string{errNudgeEffectStartupRefused.Error(), nudgequeue.ErrCommandRepositoryUnsupported.Error()} {
		if !strings.Contains(stderr.String(), want) {
			t.Fatalf("stderr = %q, want %q", stderr.String(), want)
		}
	}
	if len(recorder.Events) != 0 {
		t.Fatalf("events before startup refusal = %#v, want none", recorder.Events)
	}
	if _, err := os.Stat(filepath.Join(cityPath, ".gc", "controller.token")); !os.IsNotExist(err) {
		t.Fatalf("controller token after startup refusal: %v, want absent", err)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after startup refusal: %v", err)
	}
	_ = reacquired.Close()
}

func TestNudgeEffectStartupGateWiredIntoEveryProductionRoot(t *testing.T) {
	files := parseControllerStopProductionFiles(t)
	tests := []struct {
		file       string
		function   string
		startText  string
		failureKey string
		flagsVar   string
	}{
		{file: "controller.go", function: "runControllerWithLease", startText: "Controller started.", flagsVar: "rolloutFlags"},
		{file: "cmd_supervisor.go", function: "startManagedCity", failureKey: "nudge_effect_owner_refused", flagsVar: "bootRolloutFlags"},
	}
	for _, test := range tests {
		test := test
		t.Run(test.function, func(t *testing.T) {
			fn := findProductionFunc(t, files[test.file], test.function)
			var gatePos, runtimePos, statePos, startTextPos token.Pos
			gateCalls := 0
			runtimeOwnershipFields := 0
			runtimeSelectionBindings := 0
			stateCalls := 0
			stateLatchBindings := 0
			failureKeyFound := false
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				switch n := node.(type) {
				case *ast.CallExpr:
					switch controllerStopCalledIdent(n.Fun) {
					case "resolveNudgeEffectStartupForCity":
						gateCalls++
						gatePos = n.Pos()
					case "newCityRuntime":
						runtimePos = n.Pos()
					case "newControllerStateWithRolloutFlags":
						stateCalls++
						statePos = n.Pos()
						if len(n.Args) > 0 {
							arg, ok := n.Args[len(n.Args)-1].(*ast.Ident)
							if ok && arg.Name == test.flagsVar {
								stateLatchBindings++
							}
						}
					}
				case *ast.CompositeLit:
					ident, ok := n.Type.(*ast.Ident)
					if !ok || ident.Name != "CityRuntimeParams" {
						break
					}
					for _, element := range n.Elts {
						field, ok := element.(*ast.KeyValueExpr)
						if !ok {
							continue
						}
						key, ok := field.Key.(*ast.Ident)
						if ok && key.Name == "NudgeEffectOwnership" {
							runtimeOwnershipFields++
							selector, ok := field.Value.(*ast.SelectorExpr)
							if !ok {
								continue
							}
							owner, ownerOK := selector.X.(*ast.Ident)
							if ownerOK && owner.Name == "nudgeEffectSelection" && selector.Sel.Name == "Ownership" {
								runtimeSelectionBindings++
							}
						}
					}
				case *ast.BasicLit:
					if n.Kind != token.STRING {
						break
					}
					value, err := strconv.Unquote(n.Value)
					if err != nil {
						break
					}
					if value == test.startText {
						startTextPos = n.Pos()
					}
					if value == test.failureKey {
						failureKeyFound = true
					}
				}
				return true
			})
			if gateCalls != 1 || gatePos == token.NoPos {
				t.Fatalf("%s gate calls = %d, want exactly one", test.function, gateCalls)
			}
			if runtimeOwnershipFields != 1 {
				t.Fatalf("%s CityRuntimeParams ownership fields = %d, want exactly one", test.function, runtimeOwnershipFields)
			}
			if runtimeSelectionBindings != 1 {
				t.Fatalf("%s selected-ownership bindings = %d, want exactly one", test.function, runtimeSelectionBindings)
			}
			if stateCalls != 1 {
				t.Fatalf("%s boot-latched controller-state calls = %d, want exactly one", test.function, stateCalls)
			}
			if stateLatchBindings != 1 {
				t.Fatalf("%s controller-state latch bindings = %d, want exact %s", test.function, stateLatchBindings, test.flagsVar)
			}
			if runtimePos == token.NoPos || statePos == token.NoPos || gatePos >= runtimePos || gatePos >= statePos {
				t.Fatalf("%s gate/runtime/state order = %d/%d/%d, want gate before both consumers", test.function, gatePos, runtimePos, statePos)
			}
			if test.startText != "" && (startTextPos == token.NoPos || gatePos >= startTextPos) {
				t.Fatalf("%s gate/start-success order = %d/%d, want refusal gate first", test.function, gatePos, startTextPos)
			}
			if test.failureKey != "" && !failureKeyFound {
				t.Fatalf("%s lacks typed startup failure key %q", test.function, test.failureKey)
			}
		})
	}
}
