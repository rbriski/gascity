package main

import (
	"bytes"
	"errors"
	"fmt"
	"go/ast"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/testutil"
)

func TestManagedCLIStartHeldControllerLockDoesNotScaffoldOrTouchProviders(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_SESSION", "fake")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_DOLT", "skip")

	cityPath := filepath.Join(t.TempDir(), "locked-city")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace]\nname = \"locked-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	beadsLog := filepath.Join(t.TempDir(), "beads-ops.log")
	t.Setenv("GC_BEADS", "exec:"+writeSpyScript(t, beadsLog))
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)
	tmuxLog := filepath.Join(t.TempDir(), "tmux-ops.log")
	binDir := t.TempDir()
	if err := os.WriteFile(
		filepath.Join(binDir, "tmux"),
		[]byte(fmt.Sprintf("#!/bin/sh\nprintf '%%s\\n' \"$*\" >> %q\nexit 99\n", tmuxLog)),
		0o755,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	held, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("hold controller lock: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", false },
		testutil.GoroutineRaceTimeout,
		10*time.Millisecond,
	)
	oldExtraConfigFiles, oldNoStrictMode := extraConfigFiles, noStrictMode
	oldDryRunMode, oldNoAutoRestartMode := dryRunMode, noAutoRestartMode
	extraConfigFiles, noStrictMode = nil, false
	dryRunMode, noAutoRestartMode = false, false
	t.Cleanup(func() {
		extraConfigFiles, noStrictMode = oldExtraConfigFiles, oldNoStrictMode
		dryRunMode, noAutoRestartMode = oldDryRunMode, oldNoAutoRestartMode
	})

	var stdout, stderr bytes.Buffer
	if code := doStart([]string{cityPath}, false, &stdout, &stderr); code == 0 {
		t.Fatalf("managed start code = 0, want held-lock failure; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}

	for _, rel := range []string{"cache", "system", "runtime", "events.jsonl"} {
		path := filepath.Join(cityPath, ".gc", rel)
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("managed start materialized %s before controller ownership: %v", path, err)
		}
	}
	if ops := readOpLog(t, beadsLog); len(ops) != 0 {
		t.Fatalf("managed start reached bead provider before controller ownership: %v", ops)
	}
	if ops := readOpLog(t, tmuxLog); len(ops) != 0 {
		t.Fatalf("managed start reached tmux before controller ownership: %v", ops)
	}
}

func TestRequireBootstrappedCityHeldTargetDoesNotMaterializeRegisteredSibling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("GC_HOME", gcHome)

	targetPath := filepath.Join(t.TempDir(), "target-city")
	if err := os.MkdirAll(filepath.Join(targetPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetPath, "city.toml"), []byte(`[workspace]
name = "target-city"

[daemon]
formula_v2 = false
`), 0o644); err != nil {
		t.Fatal(err)
	}

	siblingPath := writeCityWithLockedPublicGastownImport(t)
	siblingConfig, err := os.ReadFile(filepath.Join(siblingPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	siblingConfig = append(siblingConfig, []byte("\n[daemon]\nformula_v2 = false\n")...)
	if err := os.WriteFile(filepath.Join(siblingPath, "city.toml"), siblingConfig, 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(siblingPath, "registered-sibling"); err != nil {
		t.Fatal(err)
	}

	held, err := acquireControllerLock(targetPath)
	if err != nil {
		t.Fatalf("hold target controller lock: %v", err)
	}
	t.Cleanup(func() { _ = held.Close() })

	previousFormulaV2 := formula.IsFormulaV2Enabled()
	previousGraphApply := molecule.IsGraphApplyEnabled()
	formula.SetFormulaV2Enabled(true)
	molecule.SetGraphApplyEnabled(true)
	t.Cleanup(func() {
		formula.SetFormulaV2Enabled(previousFormulaV2)
		molecule.SetGraphApplyEnabled(previousGraphApply)
	})

	resolved, err := requireBootstrappedCity(targetPath)
	if err != nil {
		t.Fatalf("requireBootstrappedCity: %v", err)
	}
	if !samePath(resolved, targetPath) {
		t.Fatalf("resolved city = %q, want %q", resolved, targetPath)
	}
	assertPublicGastownSyntheticCacheAbsent(t, gcHome)
	if !formula.IsFormulaV2Enabled() || !molecule.IsGraphApplyEnabled() {
		t.Fatal("pre-lock start path resolution changed process-global feature flags")
	}
}

func TestRegisteredCityNameDefersUncachedLegacyPackProviderValidation(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())

	cityPath, cacheDir := writeUncachedLegacyProviderCity(t, "provider-city")
	name, err := registeredCityName(cityPath, "")
	if err != nil {
		t.Fatalf("read-only registration intent rejected provider from an uncached pack: %v", err)
	}
	if name != "provider-city" {
		t.Fatalf("registered city name = %q, want provider-city", name)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("registration intent materialized legacy pack cache at %s: %v", cacheDir, err)
	}
}

func TestRequireBootstrappedDirectCityDoesNotComposeRegisteredSibling(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())

	targetPath := filepath.Join(t.TempDir(), "target-city")
	if err := os.MkdirAll(filepath.Join(targetPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(targetPath, "city.toml"), []byte("[workspace]\nname = \"target-city\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	siblingPath, cacheDir := writeUncachedLegacyProviderCity(t, "broken-sibling")
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(siblingPath, "broken-sibling"); err != nil {
		t.Fatal(err)
	}

	resolved, err := requireBootstrappedCity(targetPath)
	if err != nil {
		t.Fatalf("direct city resolution depended on registered sibling composition: %v", err)
	}
	if !samePath(resolved, targetPath) {
		t.Fatalf("resolved city = %q, want %q", resolved, targetPath)
	}
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("direct city resolution materialized sibling pack cache at %s: %v", cacheDir, err)
	}
}

func writeUncachedLegacyProviderCity(t *testing.T, cityName string) (string, string) {
	t.Helper()

	repoRoot := t.TempDir()
	workDir := filepath.Join(repoRoot, "work")
	bareDir := filepath.Join(repoRoot, "provider-pack.git")
	mustGit(t, "", "init", workDir)
	packToml := `[pack]
name = "provider-pack"
version = "1.0.0"
schema = 1

[providers.remote-provider]
command = "/bin/true"
`
	if err := os.WriteFile(filepath.Join(workDir, "pack.toml"), []byte(packToml), 0o644); err != nil {
		t.Fatal(err)
	}
	mustGit(t, workDir, "add", "pack.toml")
	mustGit(t, workDir, "commit", "-m", "add provider pack")
	mustGit(t, "", "clone", "--bare", workDir, bareDir)

	cityPath := filepath.Join(t.TempDir(), cityName)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := fmt.Sprintf(`[workspace]
name = %q
includes = ["provider-pack"]

[packs.provider-pack]
source = %q

[[agent]]
name = "worker"
provider = "remote-provider"
`, cityName, bareDir)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	return cityPath, config.PackCachePath(cityPath, "provider-pack", config.PackSource{Source: bareDir})
}

func TestReconcileCitiesHeldControllerLockRunsNoStartupEffects(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_SESSION", "fake")

	cityPath := shortSocketTempDir(t, "gc-supervisor-lock-")
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc", "mcp-managed"), 0o755); err != nil {
		t.Fatal(err)
	}
	remotePack := initBarePackRepo(t, "remote-pack")
	onBootMarker := filepath.Join(cityPath, "on-boot-ran")
	configText := fmt.Sprintf(`[workspace]
name = "locked-city"
includes = ["remote-pack"]

[packs.remote-pack]
source = %q

[session]
provider = "fake"

[[agent]]
name = "worker"
provider = "claude"
min_active_sessions = 0
max_active_sessions = 2
on_boot = "touch %s"
`, remotePack, onBootMarker)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(configText), 0o644); err != nil {
		t.Fatal(err)
	}

	// A stale managed MCP projection is a mutation witness: reaching the MCP
	// projection phase would remove or rewrite it even though no controller may
	// start while the lock is held.
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

	// Shadow tmux with a spy so this regression can prove the default server is
	// never contacted even if a future preparation path bypasses GC_SESSION.
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

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "locked-city"); err != nil {
		t.Fatal(err)
	}
	registry := newCityRegistry()
	var stdout bytes.Buffer
	var stderr lockedBuffer

	reconcileCities(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr)

	if ops := readOpLog(t, beadsLog); len(ops) != 0 {
		t.Fatalf("held-lock startup reached bead/store lifecycle: %v", ops)
	}
	if _, err := os.Stat(onBootMarker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("held-lock startup ran on_boot, stat error = %v", err)
	}
	cacheDir := config.PackCachePath(cityPath, "remote-pack", config.PackSource{Source: remotePack})
	if _, err := os.Stat(filepath.Join(cacheDir, "pack.toml")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("held-lock startup materialized pack cache at %s: %v", cacheDir, err)
	}
	if got, err := os.ReadFile(mcpTarget); err != nil || !bytes.Equal(got, mcpBefore) {
		t.Fatalf("held-lock startup mutated MCP projection: got %q, err=%v", got, err)
	}
	for _, path := range []string{
		filepath.Join(cityPath, ".gc", "events.jsonl"),
		filepath.Join(cityPath, ".gc", "controller.sock"),
		filepath.Join(cityPath, ".gc", "controller.token"),
	} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("held-lock startup created %s: %v", path, err)
		}
	}
	if ops := readOpLog(t, tmuxLog); len(ops) != 0 {
		t.Fatalf("held-lock startup invoked tmux: %v", ops)
	}
	registry.ReadCallback(func(
		cities map[string]*managedCity,
		_ map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		if _, ok := cities[canonicalTestPath(cityPath)]; ok {
			t.Fatal("held-lock city was published as managed")
		}
	})
	if !strings.Contains(stderr.String(), "controller lock") {
		t.Fatalf("stderr = %q, want controller lock failure", stderr.String())
	}
}

func TestReconcileCitiesFailureBeforeTransferReleasesSourceLock(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())

	cityPath := shortSocketTempDir(t, "gc-supervisor-lock-fail-")
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte("[workspace\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "broken-city"); err != nil {
		t.Fatal(err)
	}

	var acquired *controllerLockLease
	acquireCalls := 0
	acquire := func(path string) (*controllerLockLease, error) {
		acquireCalls++
		if !samePath(path, cityPath) {
			t.Fatalf("lock path = %q, want %q", path, cityPath)
		}
		lease, err := acquireControllerLock(path)
		acquired = lease
		return lease, err
	}
	var stdout bytes.Buffer
	var stderr lockedBuffer
	reconcileCitiesWithControllerLock(reg, newCityRegistry(), supervisor.PublicationConfig{}, &stdout, &stderr, acquire, shutdownBeadsProvider)

	if acquireCalls != 1 || acquired == nil {
		t.Fatalf("managed start lock acquisitions = %d, lease=%v; want one acquired source", acquireCalls, acquired)
	}
	acquired.mu.Lock()
	closed, transferred, file := acquired.closed, acquired.transferred, acquired.file
	acquired.mu.Unlock()
	if !closed || transferred || file != nil {
		t.Fatalf("failed-start source state = closed:%t transferred:%t file:%v, want closed untransferred nil", closed, transferred, file)
	}
	if _, err := os.Stat(controllerLockPath(cityPath)); err != nil {
		t.Fatalf("managed start did not acquire the controller lock before config load: %v", err)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after pre-transfer failure: %v", err)
	}
	_ = reacquired.Close()
}

func TestReconcileCitiesTransfersExactSourceLockToManagedRuntime(t *testing.T) {
	cityPath, reg, registry := newSupervisorManagedStartFixture(t)

	var source *controllerLockLease
	var originalFile *os.File
	acquireCalls := 0
	acquire := func(path string) (*controllerLockLease, error) {
		acquireCalls++
		lease, err := acquireControllerLock(path)
		if err == nil {
			source = lease
			originalFile = lease.file
		}
		return lease, err
	}
	var stdout bytes.Buffer
	var stderr lockedBuffer
	reconcileCitiesWithControllerLock(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr, acquire, shutdownBeadsProvider)

	if acquireCalls != 1 || source == nil || originalFile == nil {
		t.Fatalf("managed start lock acquisitions = %d, source=%v file=%v; want one source", acquireCalls, source, originalFile)
	}
	source.mu.Lock()
	transferred, sourceFile := source.transferred, source.file
	source.mu.Unlock()
	if !transferred || sourceFile != nil {
		t.Fatalf("source after launch = transferred:%t file:%v, want transferred with no descriptor", transferred, sourceFile)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("closing transferred source: %v", err)
	}
	if _, err := originalFile.Stat(); err != nil {
		t.Fatalf("source cleanup closed descriptor owned by managed runtime: %v", err)
	}
	if _, err := source.Transfer(); !errors.Is(err, errControllerLockAlreadyTransferred) {
		t.Fatalf("second source transfer error = %v, want errControllerLockAlreadyTransferred", err)
	}
	if contender, err := acquireControllerLock(cityPath); contender != nil || !errors.Is(err, errControllerAlreadyRunning) {
		if contender != nil {
			_ = contender.Close()
		}
		t.Fatalf("competing acquire while managed runtime is live = (%v, %v), want held", contender, err)
	}

	done := registry.CancelCity(canonicalTestPath(cityPath))
	if done == nil {
		t.Fatal("managed city was not published")
	}
	select {
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("managed city cleanup did not complete")
	}
	if _, err := originalFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("managed cleanup did not close the exact acquired descriptor: %v", err)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after managed cleanup: %v", err)
	}
	_ = reacquired.Close()
}

func newSupervisorManagedStartFixture(t *testing.T) (string, *supervisor.Registry, *cityRegistry) {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_SESSION", "fake")

	cityPath := shortSocketTempDir(t, "gc-supervisor-lock-transfer-")
	cleanupManagedDoltTestCity(t, cityPath)
	if err := os.MkdirAll(filepath.Join(cityPath, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cityToml := `[workspace]
name = "test-city"

[orders]
skip = ["beads-health", "cross-rig-deps", "gate-sweep", "jsonl-export", "reaper", "order-tracking-sweep", "orphan-sweep", "prune-branches", "spawn-storm-detect", "wisp-compact"]

[session]
provider = "fake"

[daemon]
shutdown_timeout = "100ms"
`
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), []byte(cityToml), 0o644); err != nil {
		t.Fatal(err)
	}
	beadsLog := filepath.Join(t.TempDir(), "beads-ops.log")
	t.Setenv("GC_BEADS", "exec:"+writeSpyScript(t, beadsLog))
	t.Setenv("GC_BEADS_SCOPE_ROOT", cityPath)

	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "test-city"); err != nil {
		t.Fatal(err)
	}
	return cityPath, reg, newCityRegistry()
}

func managedCityForPath(t *testing.T, registry *cityRegistry, cityPath string) *managedCity {
	t.Helper()
	var mc *managedCity
	registry.ReadCallback(func(
		cities map[string]*managedCity,
		_ map[string]cityInitProgress,
		_ map[string]*initFailRecord,
		_ map[string]*panicRecord,
	) {
		mc = cities[canonicalTestPath(cityPath)]
	})
	if mc == nil {
		t.Fatalf("managed city %q was not published", cityPath)
	}
	return mc
}

func TestStopManagedCityRetainsTransferredLockThroughProviderShutdown(t *testing.T) {
	cityPath, reg, registry := newSupervisorManagedStartFixture(t)

	var source *controllerLockLease
	var originalFile *os.File
	acquire := func(path string) (*controllerLockLease, error) {
		lease, err := acquireControllerLock(path)
		if err == nil {
			source = lease
			originalFile = lease.file
		}
		return lease, err
	}
	shutdownEntered := make(chan struct{})
	releaseShutdown := make(chan struct{})
	shutdown := func(path string) error {
		if !samePath(path, cityPath) {
			return fmt.Errorf("shutdown path = %q, want %q", path, cityPath)
		}
		close(shutdownEntered)
		<-releaseShutdown
		return nil
	}
	var stdout bytes.Buffer
	var stderr lockedBuffer
	reconcileCitiesWithControllerLock(reg, registry, supervisor.PublicationConfig{}, &stdout, &stderr, acquire, shutdown)
	mc := managedCityForPath(t, registry, cityPath)

	stopResult := make(chan error, 1)
	go func() { stopResult <- stopManagedCity(mc, cityPath, &stderr) }()
	select {
	case <-shutdownEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("managed provider shutdown was not entered")
	}

	if err := source.Close(); err != nil {
		t.Fatalf("closing transferred source: %v", err)
	}
	if _, err := originalFile.Stat(); err != nil {
		t.Fatalf("transferred descriptor closed before provider shutdown completed: %v", err)
	}
	if contender, err := acquireControllerLock(cityPath); contender != nil || !errors.Is(err, errControllerAlreadyRunning) {
		if contender != nil {
			_ = contender.Close()
		}
		t.Fatalf("competing acquire during provider shutdown = (%v, %v), want held", contender, err)
	}
	select {
	case <-mc.done:
		t.Fatal("done closed before provider shutdown completed")
	default:
	}
	select {
	case err := <-stopResult:
		t.Fatalf("stopManagedCity returned before provider shutdown completed: %v", err)
	default:
	}

	close(releaseShutdown)
	select {
	case err := <-stopResult:
		if err != nil {
			t.Fatalf("stopManagedCity: %v; stderr=%q", err, stderr.String())
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("stopManagedCity did not return after provider shutdown release")
	}
	if _, err := originalFile.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("managed owner did not close transferred descriptor after provider shutdown: %v", err)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after provider shutdown: %v", err)
	}
	_ = reacquired.Close()
}

func TestStopManagedCityReturnsManagedProviderShutdownError(t *testing.T) {
	cityPath, reg, registry := newSupervisorManagedStartFixture(t)
	wantErr := errors.New("fatal bead provider shutdown")
	var stdout bytes.Buffer
	var stderr lockedBuffer
	reconcileCitiesWithControllerLock(
		reg,
		registry,
		supervisor.PublicationConfig{},
		&stdout,
		&stderr,
		acquireControllerLock,
		func(string) error { return wantErr },
	)
	mc := managedCityForPath(t, registry, cityPath)

	err := stopManagedCity(mc, cityPath, &stderr)
	if !errors.Is(err, wantErr) {
		t.Fatalf("stopManagedCity error = %v, want managed shutdown error %v", err, wantErr)
	}
	if !strings.Contains(stderr.String(), wantErr.Error()) {
		t.Fatalf("stderr = %q, want provider shutdown error", stderr.String())
	}
}

func TestRunManagedCityOwnedPhasesRetainsLeaseThroughCleanupAfterRunPanic(t *testing.T) {
	cityPath := t.TempDir()
	source, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("acquire source lock: %v", err)
	}
	runtimeLock, err := source.Transfer()
	if err != nil {
		t.Fatalf("transfer source lock: %v", err)
	}
	originalFile := runtimeLock.file
	if err := source.Close(); err != nil {
		t.Fatalf("close transferred source wrapper: %v", err)
	}
	t.Cleanup(func() {
		_ = source.Close()
		_ = runtimeLock.Close()
	})

	assertLeaseHeld := func(phase string) {
		t.Helper()
		if _, err := originalFile.Stat(); err != nil {
			t.Fatalf("exact transferred descriptor during %s: %v", phase, err)
		}
		contender, acquireErr := acquireControllerLock(cityPath)
		if contender != nil {
			_ = contender.Close()
		}
		if contender != nil || !errors.Is(acquireErr, errControllerAlreadyRunning) {
			t.Fatalf("controller lock during %s = (%v, %v), want exact lease held", phase, contender, acquireErr)
		}
	}

	var order []string
	providerCalls := 0
	done := make(chan struct{})
	mc := &managedCity{name: "panic-city", done: done}
	var stderr bytes.Buffer
	runManagedCityWithLease(mc, runtimeLock, &stderr, func() {
		result := runManagedCityOwnedPhases(
			func() {
				order = append(order, "run")
				panic("deterministic run panic")
			},
			func() {
				order = append(order, "runtime-shutdown")
				assertLeaseHeld("runtime shutdown")
			},
			func() error {
				providerCalls++
				order = append(order, "provider-shutdown")
				assertLeaseHeld("provider shutdown")
				return nil
			},
		)
		if result.recovered == nil {
			t.Fatal("run panic was not captured")
		}
		order = append(order, "finalize")
		assertLeaseHeld("state finalization")
	})

	if got, want := strings.Join(order, ","), "run,runtime-shutdown,provider-shutdown,finalize"; got != want {
		t.Fatalf("managed phase order = %q, want %q", got, want)
	}
	if providerCalls != 1 {
		t.Fatalf("provider shutdown calls = %d, want exactly 1", providerCalls)
	}
	select {
	case <-done:
	default:
		t.Fatal("managed completion was not signaled")
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after managed completion: %v", err)
	}
	_ = reacquired.Close()
}

func TestRunManagedCityOwnedPhasesDoesNotClassifyCleanupPanicAsRuntimePanic(t *testing.T) {
	result := runManagedCityOwnedPhases(
		func() {},
		func() { panic("runtime shutdown failed") },
		func() error { panic("provider shutdown failed") },
	)

	if result.recovered != nil {
		t.Fatalf("runtime panic classification = %v, want nil for cleanup-only panics", result.recovered)
	}
	if result.cleanupErr == nil || !strings.Contains(result.cleanupErr.Error(), "runtime shutdown panic") ||
		!strings.Contains(result.cleanupErr.Error(), "bead provider shutdown panic") {
		t.Fatalf("cleanup error = %v, want both cleanup panics", result.cleanupErr)
	}
}

func TestRunManagedCityWithLeaseReleasesLeaseAndSignalsDoneAfterEpiloguePanic(t *testing.T) {
	cityPath := t.TempDir()
	lease, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("acquire controller lock: %v", err)
	}
	done := make(chan struct{})
	mc := &managedCity{name: "panic-city", done: done}
	var stderr bytes.Buffer

	runManagedCityWithLease(mc, lease, &stderr, func() {
		panic("deterministic epilogue panic")
	})

	select {
	case <-done:
	default:
		t.Fatal("managed completion was not signaled after epilogue panic")
	}
	if mc.managedShutdownErr == nil || !strings.Contains(mc.managedShutdownErr.Error(), "epilogue panic") {
		t.Fatalf("managed shutdown error = %v, want epilogue panic", mc.managedShutdownErr)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after epilogue panic: %v", err)
	}
	_ = reacquired.Close()
}

func TestStopManagedCityProviderShutdownPanicIsFatalAndReleasesLease(t *testing.T) {
	cityPath, reg, registry := newSupervisorManagedStartFixture(t)
	providerCalls := 0
	var originalFile *os.File
	acquire := func(path string) (*controllerLockLease, error) {
		lease, err := acquireControllerLock(path)
		if err == nil {
			originalFile = lease.file
		}
		return lease, err
	}
	var stdout bytes.Buffer
	var stderr lockedBuffer
	reconcileCitiesWithControllerLock(
		reg,
		registry,
		supervisor.PublicationConfig{},
		&stdout,
		&stderr,
		acquire,
		func(string) error {
			providerCalls++
			panic("deterministic provider shutdown panic")
		},
	)
	mc := managedCityForPath(t, registry, cityPath)

	err := stopManagedCity(mc, cityPath, &stderr)
	if err == nil || !strings.Contains(err.Error(), "provider shutdown panic") {
		t.Fatalf("stopManagedCity error = %v, want provider shutdown panic", err)
	}
	if providerCalls != 1 {
		t.Fatalf("provider shutdown calls = %d, want exactly 1", providerCalls)
	}
	if _, statErr := originalFile.Stat(); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("transferred descriptor after provider panic = %v, want closed", statErr)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after provider panic: %v", err)
	}
	_ = reacquired.Close()
}

func TestSupervisorManagedStartAcquiresBeforeEffectsAndTransfersOnce(t *testing.T) {
	files := parseControllerStopProductionFiles(t)
	file := files["cmd_supervisor.go"]
	if file == nil {
		t.Fatal("cmd_supervisor.go was not parsed")
	}
	var start *ast.FuncDecl
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if ok && fn.Name.Name == "startManagedCity" {
			start = fn
			break
		}
	}
	if start == nil {
		t.Fatal("startManagedCity not found")
	}

	firstEffects := map[string]bool{
		"ensureLegacyNamedPacksCached":           true,
		"loadSupervisorCityConfig":               true,
		"prepareCityForSupervisor":               true,
		"newSessionProviderFromContextWithError": true,
		"checkAgentImages":                       true,
		"newFileEventsRecorder":                  true,
		"newControllerState":                     true,
		"startBeadEventWatcher":                  true,
		"startMaintenanceLoop":                   true,
		"runPoolOnBoot":                          true,
		"startControllerSocket":                  true,
	}
	var acquirePos, publishPos, transferPos, launchPos token.Pos
	transferCalls := 0
	seenEffects := make(map[string]token.Pos, len(firstEffects))
	ast.Inspect(start.Body, func(node ast.Node) bool {
		switch n := node.(type) {
		case *ast.CallExpr:
			callee := controllerStopCalledIdent(n.Fun)
			if callee == "acquireLock" && acquirePos == token.NoPos {
				acquirePos = n.Pos()
			}
			if callee == "publishManagedCity" {
				publishPos = n.Pos()
			}
			if callee == "Transfer" {
				transferCalls++
				transferPos = n.Pos()
			}
			if firstEffects[callee] {
				if _, exists := seenEffects[callee]; !exists {
					seenEffects[callee] = n.Pos()
				}
			}
		case *ast.GoStmt:
			if launchPos == token.NoPos {
				launchPos = n.Pos()
			}
		}
		return true
	})
	if acquirePos == token.NoPos {
		t.Fatal("managed start has no injected controller-lock acquisition")
	}
	for name, pos := range seenEffects {
		if pos < acquirePos {
			t.Errorf("managed start effect %s appears before controller-lock acquisition", name)
		}
	}
	if len(seenEffects) != len(firstEffects) {
		t.Fatalf("managed start effect inventory found %d/%d entries: %v", len(seenEffects), len(firstEffects), seenEffects)
	}
	if transferCalls != 1 {
		t.Fatalf("managed start Transfer calls = %d, want exactly 1", transferCalls)
	}
	if publishPos == token.NoPos || transferPos == token.NoPos || launchPos == token.NoPos || (publishPos >= transferPos || transferPos >= launchPos) {
		t.Fatalf("managed start publish/transfer/launch order = %d/%d/%d, want publish < transfer < launch", publishPos, transferPos, launchPos)
	}
}
