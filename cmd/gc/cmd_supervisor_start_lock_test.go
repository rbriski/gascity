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
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/testutil"
)

type managedCloserSpy struct {
	calls int
	close func() error
}

func (c *managedCloserSpy) Close() error {
	c.calls++
	if c.close == nil {
		return nil
	}
	return c.close()
}

type managedRecorderSpy struct {
	record func(events.Event)
}

func (r managedRecorderSpy) Record(event events.Event) {
	if r.record != nil {
		r.record(event)
	}
}

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

func TestStopManagedCityPreservingSessionsOwnerSkipsProviderAndReleasesLease(t *testing.T) {
	cityPath, reg, registry := newSupervisorManagedStartFixture(t)
	var originalFile *os.File
	acquire := func(path string) (*controllerLockLease, error) {
		lease, err := acquireControllerLock(path)
		if err == nil {
			originalFile = lease.file
		}
		return lease, err
	}
	providerCalls := 0
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
			return nil
		},
	)
	mc := managedCityForPath(t, registry, cityPath)

	if err := stopManagedCityPreservingSessions(mc, cityPath, &stderr); err != nil {
		t.Fatalf("stopManagedCityPreservingSessions: %v; stderr=%q", err, stderr.String())
	}

	if providerCalls != 0 {
		t.Fatalf("managed provider shutdown calls = %d, want zero in preserve mode", providerCalls)
	}
	select {
	case <-mc.done:
	default:
		t.Fatal("preserve-mode managed owner did not signal completion")
	}
	if originalFile == nil {
		t.Fatal("managed start never acquired the source controller lease")
	}
	if _, statErr := originalFile.Stat(); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("transferred descriptor after preserve-mode completion = %v, want closed", statErr)
	}
	if mc.managedShutdownErr != nil {
		t.Fatalf("preserve-mode managed shutdown error = %v, want nil", mc.managedShutdownErr)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after preserve-mode completion: %v", err)
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
	}, nil)

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

func TestManagedControllerStartedPanicEntersRuntimeBeforeSingleNormalCleanup(t *testing.T) {
	cityPath := t.TempDir()
	lease, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("acquire controller lock: %v", err)
	}
	runtimeEntered := make(chan struct{})
	releaseRuntime := make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-releaseRuntime:
		default:
			close(releaseRuntime)
		}
	})
	done := make(chan struct{})
	mc := &managedCity{name: "publication-panic-city", done: done}
	recorded := make(chan events.Event, 1)
	recorder := managedRecorderSpy{record: func(event events.Event) {
		recorded <- event
		panic("controller-started panic")
	}}
	var runtimeEntries atomic.Int32
	var runtimeShutdownCalls atomic.Int32
	var providerShutdownCalls atomic.Int32
	var result managedCityOwnedPhasesResult
	var stderr bytes.Buffer

	go runManagedCityWithLease(mc, lease, &stderr, func() {
		result = runManagedCityOwnedPhases(
			func() {
				recordManagedControllerStarted(mc, recorder, &stderr)
				runtimeEntries.Add(1)
				close(runtimeEntered)
				<-releaseRuntime
			},
			func() { runtimeShutdownCalls.Add(1) },
			func() error {
				providerShutdownCalls.Add(1)
				return nil
			},
		)
	}, nil)

	select {
	case <-runtimeEntered:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("managed runtime did not enter after ControllerStarted recorder panic")
	}
	event := <-recorded
	if event.Type != events.ControllerStarted {
		t.Fatalf("recorded event type = %q, want %q", event.Type, events.ControllerStarted)
	}
	if got := runtimeEntries.Load(); got != 1 {
		t.Fatalf("runtime entries = %d, want exactly 1", got)
	}
	if got := runtimeShutdownCalls.Load(); got != 0 {
		t.Fatalf("runtime shutdown calls while runtime is blocked = %d, want 0", got)
	}
	if got := providerShutdownCalls.Load(); got != 0 {
		t.Fatalf("provider shutdown calls while runtime is blocked = %d, want 0", got)
	}

	close(releaseRuntime)
	select {
	case <-done:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("managed owner did not finish after runtime release")
	}
	if result.recovered != nil {
		t.Fatalf("runtime panic classification = %v, want nil", result.recovered)
	}
	if result.cleanupErr != nil {
		t.Fatalf("cleanup error = %v, want nil", result.cleanupErr)
	}
	if got := runtimeShutdownCalls.Load(); got != 1 {
		t.Fatalf("runtime shutdown calls after runtime exit = %d, want exactly 1", got)
	}
	if got := providerShutdownCalls.Load(); got != 1 {
		t.Fatalf("provider shutdown calls after runtime exit = %d, want exactly 1", got)
	}
	if mc.managedShutdownErr == nil || !strings.Contains(mc.managedShutdownErr.Error(), "controller-started event publication panic") {
		t.Fatalf("managed shutdown error after done = %v, want sticky publication panic", mc.managedShutdownErr)
	}
	if !strings.Contains(stderr.String(), "controller-started event publication panic") {
		t.Fatalf("stderr = %q, want publication panic", stderr.String())
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

func TestRunManagedCityWithLeaseFinalizesAndClosesRecorderBeforeLeaseAndDoneAfterEpiloguePanic(t *testing.T) {
	cityPath := t.TempDir()
	source, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("acquire source controller lock: %v", err)
	}
	lease, err := source.Transfer()
	if err != nil {
		t.Fatalf("transfer controller lock: %v", err)
	}
	if err := source.Close(); err != nil {
		t.Fatalf("close transferred source wrapper: %v", err)
	}
	originalFile := lease.file
	done := make(chan struct{})
	registry := newCityRegistry()
	var order []string
	closer := &managedCloserSpy{close: func() error {
		order = append(order, "recorder")
		if registry.Has(cityPath) {
			t.Fatal("recorder closed before managed publication was finalized")
		}
		if _, statErr := originalFile.Stat(); statErr != nil {
			t.Fatalf("transferred controller lease during recorder close: %v", statErr)
		}
		contender, acquireErr := acquireControllerLock(cityPath)
		if contender != nil {
			_ = contender.Close()
		}
		if contender != nil || !errors.Is(acquireErr, errControllerAlreadyRunning) {
			t.Fatalf("controller lock during recorder close = (%v, %v), want transferred lease held", contender, acquireErr)
		}
		select {
		case <-done:
			t.Fatal("managed completion signaled before recorder close")
		default:
		}
		return nil
	}}
	mc := &managedCity{name: "panic-city", done: done, closer: closer}
	registry.Add(cityPath, mc)
	var stderr bytes.Buffer

	runManagedCityWithLease(mc, lease, &stderr, func() {
		order = append(order, "run")
		panic("deterministic epilogue panic")
	}, func() {
		order = append(order, "finalizer")
		if closer.calls != 0 {
			t.Fatal("managed publication finalized after recorder close")
		}
		if _, statErr := originalFile.Stat(); statErr != nil {
			t.Fatalf("transferred controller lease during finalizer: %v", statErr)
		}
		select {
		case <-done:
			t.Fatal("managed completion signaled before publication finalizer")
		default:
		}
		finalizeManagedCityRun(registry, cityPath, mc.name, mc, nil, &stderr)
	})

	if got, want := strings.Join(order, ","), "run,finalizer,recorder"; got != want {
		t.Fatalf("managed ownership tail order = %q, want %q", got, want)
	}
	select {
	case <-done:
	default:
		t.Fatal("managed completion was not signaled after epilogue panic")
	}
	if closer.calls != 1 {
		t.Fatalf("recorder close calls = %d, want exactly 1", closer.calls)
	}
	if registry.Has(cityPath) {
		t.Fatal("epilogue panic left dead managed publication in registry")
	}
	if mc.managedShutdownErr == nil || !strings.Contains(mc.managedShutdownErr.Error(), "epilogue panic") {
		t.Fatalf("managed shutdown error = %v, want epilogue panic", mc.managedShutdownErr)
	}
	if _, statErr := originalFile.Stat(); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("transferred descriptor after managed completion = %v, want closed", statErr)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after epilogue panic: %v", err)
	}
	_ = reacquired.Close()
}

func TestRunManagedCityWithLeaseRecorderFailureIsStickyAfterDone(t *testing.T) {
	wantCloseErr := errors.New("recorder close failed")
	tests := []struct {
		name     string
		close    func() error
		wantIs   error
		wantText string
	}{
		{
			name:     "error",
			close:    func() error { return wantCloseErr },
			wantIs:   wantCloseErr,
			wantText: wantCloseErr.Error(),
		},
		{
			name: "panic",
			close: func() error {
				panic("recorder close panic")
			},
			wantText: "recorder close panic",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cityPath := t.TempDir()
			source, err := acquireControllerLock(cityPath)
			if err != nil {
				t.Fatalf("acquire source controller lock: %v", err)
			}
			lease, err := source.Transfer()
			if err != nil {
				t.Fatalf("transfer controller lock: %v", err)
			}
			if err := source.Close(); err != nil {
				t.Fatalf("close transferred source wrapper: %v", err)
			}
			originalFile := lease.file
			done := make(chan struct{})
			closer := &managedCloserSpy{close: func() error {
				if _, statErr := originalFile.Stat(); statErr != nil {
					t.Fatalf("transferred controller lease during recorder close: %v", statErr)
				}
				contender, acquireErr := acquireControllerLock(cityPath)
				if contender != nil {
					_ = contender.Close()
				}
				if contender != nil || !errors.Is(acquireErr, errControllerAlreadyRunning) {
					t.Fatalf("controller lock during recorder close = (%v, %v), want transferred lease held", contender, acquireErr)
				}
				select {
				case <-done:
					t.Fatal("managed completion signaled before recorder close")
				default:
				}
				return tt.close()
			}}
			mc := &managedCity{name: "recorder-failure-city", done: done, closer: closer}
			var stderr bytes.Buffer

			runManagedCityWithLease(mc, lease, &stderr, func() {}, nil)

			select {
			case <-done:
			default:
				t.Fatal("managed completion was not signaled after recorder failure")
			}
			for attempt := 1; attempt <= 2; attempt++ {
				stopErr := finishManagedCityStop(mc, nil)
				if tt.wantIs != nil && !errors.Is(stopErr, tt.wantIs) {
					t.Fatalf("finishManagedCityStop attempt %d error = %v, want %v", attempt, stopErr, tt.wantIs)
				}
				if stopErr == nil || !strings.Contains(stopErr.Error(), tt.wantText) {
					t.Fatalf("finishManagedCityStop attempt %d error = %v, want %q", attempt, stopErr, tt.wantText)
				}
			}
			if closer.calls != 1 {
				t.Fatalf("recorder close calls = %d, want exactly 1 after repeated waiter reads", closer.calls)
			}
			if !strings.Contains(stderr.String(), "event recorder close") || !strings.Contains(stderr.String(), tt.wantText) {
				t.Fatalf("stderr = %q, want recorder close failure %q", stderr.String(), tt.wantText)
			}
			if _, statErr := originalFile.Stat(); !errors.Is(statErr, os.ErrClosed) {
				t.Fatalf("transferred descriptor after managed completion = %v, want closed", statErr)
			}
			reacquired, err := acquireControllerLock(cityPath)
			if err != nil {
				t.Fatalf("reacquire after recorder failure: %v", err)
			}
			_ = reacquired.Close()
		})
	}
}

func TestRunManagedCityWithLeaseFinalizerPanicStillClosesRecorderLeaseAndDone(t *testing.T) {
	cityPath := t.TempDir()
	lease, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("acquire controller lock: %v", err)
	}
	originalFile := lease.file
	done := make(chan struct{})
	closer := &managedCloserSpy{}
	mc := &managedCity{name: "finalizer-panic-city", done: done, closer: closer}
	var stderr bytes.Buffer

	runManagedCityWithLease(mc, lease, &stderr, func() {}, func() {
		panic("deterministic finalizer panic")
	})

	select {
	case <-done:
	default:
		t.Fatal("managed completion was not signaled after finalizer panic")
	}
	if closer.calls != 1 {
		t.Fatalf("recorder close calls = %d, want exactly 1", closer.calls)
	}
	if mc.managedShutdownErr == nil || !strings.Contains(mc.managedShutdownErr.Error(), "managed finalizer panic") {
		t.Fatalf("managed shutdown error = %v, want finalizer panic", mc.managedShutdownErr)
	}
	if _, statErr := originalFile.Stat(); !errors.Is(statErr, os.ErrClosed) {
		t.Fatalf("controller descriptor after managed completion = %v, want closed", statErr)
	}
	reacquired, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("reacquire after finalizer panic: %v", err)
	}
	_ = reacquired.Close()
}

func TestFinalizeManagedCityRunRecordsOnlyCurrentOwner(t *testing.T) {
	t.Run("runtime panic records backoff and removes current owner", func(t *testing.T) {
		cityPath := t.TempDir()
		current := &managedCity{name: "current"}
		registry := newCityRegistry()
		registry.Add(cityPath, current)
		registry.BatchUpdate(func(
			_ map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			panicHistory[cityPath] = &panicRecord{count: 2}
		})
		started := time.Now()
		var stderr bytes.Buffer

		finalizeManagedCityRun(registry, cityPath, current.name, current, "runtime panic", &stderr)

		registry.ReadCallback(func(
			cities map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			if _, exists := cities[cityPath]; exists {
				t.Fatal("current owner remained published after runtime panic")
			}
			record := panicHistory[cityPath]
			if record == nil || record.count != 3 || record.backoff.Before(started) {
				t.Fatalf("panic history = %#v, want count 3 with future backoff", record)
			}
		})
		if !strings.Contains(stderr.String(), "panic #3") {
			t.Fatalf("stderr = %q, want current-owner panic backoff", stderr.String())
		}
	})

	t.Run("normal exit clears backoff and removes current owner", func(t *testing.T) {
		cityPath := t.TempDir()
		current := &managedCity{name: "current"}
		registry := newCityRegistry()
		registry.Add(cityPath, current)
		registry.BatchUpdate(func(
			_ map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			panicHistory[cityPath] = &panicRecord{count: 2}
		})

		finalizeManagedCityRun(registry, cityPath, current.name, current, nil, io.Discard)

		registry.ReadCallback(func(
			cities map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			if _, exists := cities[cityPath]; exists {
				t.Fatal("current owner remained published after normal exit")
			}
			if _, exists := panicHistory[cityPath]; exists {
				t.Fatal("normal current-owner exit retained panic history")
			}
		})
	})

	t.Run("normal name-drift exit clears backoff after deliberate publication removal", func(t *testing.T) {
		cityPath := t.TempDir()
		current := &managedCity{name: "old-name"}
		registry := newCityRegistry()
		registry.Add(cityPath, current)
		registry.BatchUpdate(func(
			cities map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			panicHistory[cityPath] = &panicRecord{count: 2, backoff: time.Now().Add(time.Minute)}
			delete(cities, cityPath) // reconcileCities name-drift removal before stopManagedCity
		})

		finalizeManagedCityRun(registry, cityPath, current.name, current, nil, io.Discard)

		registry.ReadCallback(func(
			cities map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			if _, exists := cities[cityPath]; exists {
				t.Fatal("name-drifted owner was republished during finalization")
			}
			if _, exists := panicHistory[cityPath]; exists {
				t.Fatal("normal name-drift exit retained old owner's panic history")
			}
		})
	})

	t.Run("stale owner cannot alter replacement or its backoff", func(t *testing.T) {
		cityPath := t.TempDir()
		stale := &managedCity{name: "stale"}
		replacement := &managedCity{name: "replacement"}
		registry := newCityRegistry()
		registry.Add(cityPath, replacement)
		wantBackoff := time.Now().Add(time.Minute)
		registry.BatchUpdate(func(
			_ map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			panicHistory[cityPath] = &panicRecord{count: 3, backoff: wantBackoff}
		})
		var stderr bytes.Buffer

		finalizeManagedCityRun(registry, cityPath, stale.name, stale, "stale runtime panic", &stderr)

		registry.ReadCallback(func(
			cities map[string]*managedCity,
			_ map[string]cityInitProgress,
			_ map[string]*initFailRecord,
			panicHistory map[string]*panicRecord,
		) {
			if cities[cityPath] != replacement {
				t.Fatalf("published city = %p, want replacement %p", cities[cityPath], replacement)
			}
			record := panicHistory[cityPath]
			if record == nil || record.count != 3 || !record.backoff.Equal(wantBackoff) {
				t.Fatalf("replacement panic history = %#v, want existing record unchanged", record)
			}
		})
		if stderr.Len() != 0 {
			t.Fatalf("stale finalizer stderr = %q, want no replacement-facing panic log", stderr.String())
		}
	})
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
	var acquirePos, publishPos, transferPos, launchPos, startedPublicationPos, runtimeRunPos token.Pos
	transferCalls := 0
	startedPublicationCalls := 0
	shutdownProviderCalls := 0
	controllerStartedFirst := false
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
			if callee == "recordManagedControllerStarted" {
				startedPublicationCalls++
				startedPublicationPos = n.Pos()
			}
			if selector, ok := n.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "run" {
				if ident, ok := selector.X.(*ast.Ident); ok && ident.Name == "cityRuntime" {
					runtimeRunPos = n.Pos()
				}
			}
			if callee == "shutdownProvider" {
				shutdownProviderCalls++
			}
			if callee == "runManagedCityOwnedPhases" && len(n.Args) > 0 {
				if callback, ok := n.Args[0].(*ast.FuncLit); ok && len(callback.Body.List) > 0 {
					if statement, ok := callback.Body.List[0].(*ast.ExprStmt); ok {
						if call, ok := statement.X.(*ast.CallExpr); ok {
							controllerStartedFirst = controllerStopCalledIdent(call.Fun) == "recordManagedControllerStarted"
						}
					}
				}
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
	if startedPublicationCalls != 1 || !controllerStartedFirst {
		t.Fatalf("managed ControllerStarted publications = %d, first-owned-operation=%v; want exactly one owned-first publication", startedPublicationCalls, controllerStartedFirst)
	}
	if shutdownProviderCalls != 1 {
		t.Fatalf("managed shutdownProvider calls = %d, want the existing owned cleanup path only", shutdownProviderCalls)
	}
	if publishPos == token.NoPos || transferPos == token.NoPos || launchPos == token.NoPos ||
		startedPublicationPos == token.NoPos || runtimeRunPos == token.NoPos ||
		publishPos >= transferPos || transferPos >= launchPos || launchPos >= startedPublicationPos || startedPublicationPos >= runtimeRunPos {
		t.Fatalf(
			"managed start publish/transfer/launch/controller-started/runtime order = %d/%d/%d/%d/%d, want publish < transfer < launch < controller-started < runtime",
			publishPos, transferPos, launchPos, startedPublicationPos, runtimeRunPos,
		)
	}
}
