package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gastownhall/gascity/internal/config"
)

func TestForegroundStartAcquiresBeforeEffectsAndTransfersOnce(t *testing.T) {
	files := parseControllerStopProductionFiles(t)
	start := findProductionFunc(t, files["cmd_start.go"], "doStartStandalone")
	runWrapper := findProductionFunc(t, files["controller.go"], "runController")
	runOwned := findProductionFunc(t, files["controller.go"], "runControllerWithLease")

	firstEffects := map[string]bool{
		"ensureCityScaffold":                         true,
		"ensureLegacyNamedPacksCached":               true,
		"loadStartCityConfig":                        true,
		"applyFeatureFlags":                          true,
		"ensureInitArtifacts":                        true,
		"startBeadsLifecycle":                        true,
		"healthBeadsProvider":                        true,
		"RunWarmupChecks":                            true,
		"ResolveFormulas":                            true,
		"pruneLegacyConfiguredScripts":               true,
		"runStage1SkillMaterialization":              true,
		"runStage1MCPProjection":                     true,
		"newSessionProvider":                         true,
		"newFileEventsRecorder":                      true,
		"checkAgentImages":                           true,
		"enforceGCPermissions":                       true,
		"runPoolOnBoot":                              true,
		"openCityStoreAt":                            true,
		"reconcileSessionBeadsAtPathWithNamedDemand": true,
	}
	var acquirePos, transferPos, ownedRunPos token.Pos
	acquireCalls := 0
	transferCalls := 0
	legacyRunCalls := 0
	seenEffects := make(map[string]token.Pos, len(firstEffects))
	ast.Inspect(start.Body, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok {
			return true
		}
		callee := controllerStopCalledIdent(call.Fun)
		switch callee {
		case "acquireControllerLock":
			acquireCalls++
			if acquirePos == token.NoPos {
				acquirePos = call.Pos()
			}
		case "Transfer":
			transferCalls++
			transferPos = call.Pos()
		case "runControllerWithLease":
			ownedRunPos = call.Pos()
		case "runController":
			legacyRunCalls++
		}
		if firstEffects[callee] {
			if _, exists := seenEffects[callee]; !exists {
				seenEffects[callee] = call.Pos()
			}
		}
		return true
	})
	if acquireCalls != 1 || acquirePos == token.NoPos {
		t.Fatalf("foreground start lock acquisitions = %d, want exactly 1", acquireCalls)
	}
	for name, pos := range seenEffects {
		if pos < acquirePos {
			t.Errorf("foreground start effect %s appears before controller-lock acquisition", name)
		}
	}
	if len(seenEffects) != len(firstEffects) {
		t.Fatalf("foreground start effect inventory found %d/%d entries: %v", len(seenEffects), len(firstEffects), seenEffects)
	}
	if transferCalls != 1 || transferPos == token.NoPos {
		t.Fatalf("foreground start Transfer calls = %d, want exactly 1", transferCalls)
	}
	if legacyRunCalls != 0 {
		t.Fatalf("foreground start calls acquiring runController %d time(s), want transferred-lease path only", legacyRunCalls)
	}
	if ownedRunPos == token.NoPos || (acquirePos >= transferPos || transferPos >= ownedRunPos) {
		t.Fatalf("foreground acquire/transfer/run order = %d/%d/%d, want acquire < transfer < run", acquirePos, transferPos, ownedRunPos)
	}

	if got := countProductionCalls(runWrapper, "acquireControllerLock"); got != 1 {
		t.Fatalf("runController compatibility wrapper acquisitions = %d, want 1", got)
	}
	if got := countProductionCalls(runOwned, "acquireControllerLock"); got != 0 {
		t.Fatalf("runControllerWithLease acquisitions = %d, want 0", got)
	}
}

func findProductionFunc(t *testing.T, file *ast.File, name string) *ast.FuncDecl {
	t.Helper()
	if file == nil {
		t.Fatalf("production file for %s was not parsed", name)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == name {
			return fn
		}
	}
	t.Fatalf("production function %s not found", name)
	return nil
}

func countProductionCalls(fn *ast.FuncDecl, want string) int {
	count := 0
	ast.Inspect(fn.Body, func(node ast.Node) bool {
		if call, ok := node.(*ast.CallExpr); ok && controllerStopCalledIdent(call.Fun) == want {
			count++
		}
		return true
	})
	return count
}

func TestValidateControllerRuntimeLeaseAcceptsOnlyLiveExactOwner(t *testing.T) {
	cityPath := t.TempDir()
	if err := validateControllerRuntimeLease(nil, cityPath); !errors.Is(err, errControllerLockLeaseClosed) {
		t.Fatalf("nil lease error = %v, want errControllerLockLeaseClosed", err)
	}

	source, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = source.Close() })
	if err := validateControllerRuntimeLease(source, cityPath); err != nil {
		t.Fatalf("fresh source lease rejected: %v", err)
	}
	if err := validateControllerRuntimeLease(source, t.TempDir()); err == nil {
		t.Fatal("lease for a different city path was accepted")
	}

	runtimeLease, err := source.Transfer()
	if err != nil {
		t.Fatalf("Transfer: %v", err)
	}
	t.Cleanup(func() { _ = runtimeLease.Close() })
	if err := validateControllerRuntimeLease(source, cityPath); !errors.Is(err, errControllerLockLeaseClosed) {
		t.Fatalf("transferred source error = %v, want errControllerLockLeaseClosed", err)
	}
	if err := validateControllerRuntimeLease(runtimeLease, cityPath); err != nil {
		t.Fatalf("transferred runtime lease rejected: %v", err)
	}
	if contender, err := acquireControllerLock(cityPath); contender != nil || !errors.Is(err, errControllerAlreadyRunning) {
		if contender != nil {
			_ = contender.Close()
		}
		t.Fatalf("competing acquisition while runtime lease is held = (%v, %v), want held", contender, err)
	}
	if err := runtimeLease.Close(); err != nil {
		t.Fatalf("closing runtime lease: %v", err)
	}
	if err := validateControllerRuntimeLease(runtimeLease, cityPath); !errors.Is(err, errControllerLockLeaseClosed) {
		t.Fatalf("closed runtime lease error = %v, want errControllerLockLeaseClosed", err)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after runtime lease close: %v", err)
	}
	_ = reacquired.Close()
}

func TestRunControllerWithLeaseClosesRejectedOwner(t *testing.T) {
	ownedCity := t.TempDir()
	lease, err := acquireControllerLock(ownedCity)
	if err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := runControllerWithLease(
		lease,
		t.TempDir(),
		"",
		nil,
		"",
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		nil,
		&stdout,
		&stderr,
	)
	if code != 1 {
		t.Fatalf("runControllerWithLease code = %d, want 1", code)
	}
	if !strings.Contains(stderr.String(), "ownership path") {
		t.Fatalf("stderr = %q, want ownership-path error", stderr.String())
	}
	reacquired, err := acquireControllerLock(ownedCity)
	if err != nil {
		t.Fatalf("reacquire after rejected owner: %v", err)
	}
	_ = reacquired.Close()
}

func TestDoStartStandaloneHeldControllerLockRunsNoStartupEffects(t *testing.T) {
	resetFlags(t)
	oldExtraConfigFiles := extraConfigFiles
	oldNoStrictMode := noStrictMode
	oldDryRunMode := dryRunMode
	extraConfigFiles = nil
	noStrictMode = false
	dryRunMode = false
	t.Cleanup(func() {
		extraConfigFiles = oldExtraConfigFiles
		noStrictMode = oldNoStrictMode
		dryRunMode = oldDryRunMode
	})

	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_DOLT", "skip")

	cityPath := shortSocketTempDir(t, "gc-foreground-lock-")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "mcp-managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	remotePack := initBarePackRepo(t, "foreground-lock-pack")
	onBootMarker := filepath.Join(cityPath, "on-boot-ran")
	configText := fmt.Sprintf(`[workspace]
name = "locked-city"
includes = ["foreground-lock-pack"]

[packs.foreground-lock-pack]
source = %q

[session]
provider = "fake"

[[agent]]
name = "local-worker"
provider = "claude"
min_active_sessions = 0
max_active_sessions = 1
on_boot = "touch %s"
`, remotePack, onBootMarker)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}

	// Reaching stage-1 MCP projection would remove or rewrite this stale
	// managed projection. It must remain byte-identical when ownership is held.
	mcpTarget := filepath.Join(cityPath, ".mcp.json")
	mcpBefore := []byte(`{"mcpServers":{"stale":{"command":"old"}}}`)
	if err := os.WriteFile(mcpTarget, mcpBefore, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, ".gc", "mcp-managed", "claude.json"), []byte(`{"managed_by":"gc"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	beadsLog := filepath.Join(t.TempDir(), "beads-ops.log")
	t.Setenv("GC_BEADS", "exec:"+writeSpyScript(t, beadsLog))
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	// Shadow tmux so a regression cannot contact the personal/default server.
	tmuxLog := filepath.Join(t.TempDir(), "tmux-ops.log")
	binDir := t.TempDir()
	tmuxSpy := filepath.Join(binDir, "tmux")
	if err := os.WriteFile(tmuxSpy, []byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 99\n", tmuxLog)), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	held, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("pre-hold controller lock: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	var stdout, stderr bytes.Buffer
	if code := doStartStandalone([]string{cityPath}, true, &stdout, &stderr); code != 1 {
		t.Fatalf("doStartStandalone code = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}

	if ops := readOpLog(t, beadsLog); len(ops) != 0 {
		t.Fatalf("held-lock foreground start reached bead/store lifecycle: %v", ops)
	}
	if _, err := os.Stat(onBootMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("held-lock foreground start ran on_boot, stat error = %v", err)
	}
	cacheDir := config.PackCachePath(cityPath, "foreground-lock-pack", config.PackSource{Source: remotePack})
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("held-lock foreground start materialized pack cache at %s: %v", cacheDir, err)
	}
	if got, err := os.ReadFile(mcpTarget); err != nil || !bytes.Equal(got, mcpBefore) {
		t.Fatalf("held-lock foreground start mutated MCP projection: got %q, err=%v", got, err)
	}
	for _, path := range []string{
		filepath.Join(cityPath, ".gc", "cache"),
		filepath.Join(cityPath, ".gc", "system"),
		filepath.Join(cityPath, ".gc", "runtime"),
		filepath.Join(cityPath, ".gc", "events.jsonl"),
		filepath.Join(cityPath, ".gc", "controller.sock"),
		filepath.Join(cityPath, ".gc", "controller.token"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("held-lock foreground start created %s: %v", path, err)
		}
	}
	if ops := readOpLog(t, tmuxLog); len(ops) != 0 {
		t.Fatalf("held-lock foreground start invoked tmux: %v", ops)
	}
	if !strings.Contains(stderr.String(), "controller already running") {
		t.Fatalf("stderr = %q, want controller ownership failure", stderr.String())
	}
}
