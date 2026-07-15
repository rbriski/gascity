package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/events"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/testutil"
)

type recordingStopProvider struct {
	*runtime.Fake
	stops      chan string
	interrupts chan string
}

func newRecordingStopProvider() *recordingStopProvider {
	return &recordingStopProvider{
		Fake:       runtime.NewFake(),
		stops:      make(chan string, 8),
		interrupts: make(chan string, 8),
	}
}

func (p *recordingStopProvider) Stop(name string) error {
	p.stops <- name
	return p.Fake.Stop(name)
}

func (p *recordingStopProvider) Interrupt(name string) error {
	p.interrupts <- name
	return p.Fake.Interrupt(name)
}

func TestStopCommandTimeoutHelpDescribesCompletionDeadlineAndRetainedOwnership(t *testing.T) {
	cmd := newStopCmd(io.Discard, io.Discard)
	for _, want := range []string{"completion deadline/SLO", "not a hard wall-clock cap", "keeps lifecycle ownership"} {
		if !strings.Contains(cmd.Long, want) {
			t.Fatalf("Long = %q, want %q", cmd.Long, want)
		}
	}
	if strings.Contains(cmd.Long, "cap the wall-clock time") {
		t.Fatalf("Long = %q, must not promise a hard wall-clock cap", cmd.Long)
	}
	timeoutFlag := cmd.Flags().Lookup("timeout")
	if timeoutFlag == nil || !strings.Contains(timeoutFlag.Usage, "completion deadline/SLO") {
		t.Fatalf("--timeout usage = %v, want completion deadline/SLO contract", timeoutFlag)
	}
}

func TestCmdStopWaitsForStandaloneControllerExit(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-stop-")
	for legacyLen := len(filepath.Join(dir, ".gc", "controller.sock")); legacyLen <= 120; legacyLen = len(filepath.Join(dir, ".gc", "controller.sock")) {
		dir = filepath.Join(dir, "very-long-controller-path-segment")
	}
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}
	writeBuiltinImportsFixture(t, dir, "core")
	if got := controllerSocketPath(dir); got == filepath.Join(dir, ".gc", "controller.sock") {
		t.Fatalf("controllerSocketPath(%q) = legacy path %q, want short fallback", dir, got)
	}
	if got, want := controllerSocketPath(dir), controllerSocketPath(canonicalTestPath(dir)); got != want {
		t.Fatalf("controllerSocketPath fallback mismatch across equivalent paths: %q vs %q", got, want)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}
	const seededSession = "seeded-session"

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		running, _ := sp.ListRunning("")
		for _, name := range running {
			sp.release(name)
		}
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)
	if err := sp.Start(context.Background(), seededSession, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 0, false)
	}()

	stopped := sp.waitForStops(t, 1)
	if len(stopped) != 1 || stopped[0] != seededSession {
		t.Fatalf("stop targets = %v, want [%s]", stopped, seededSession)
	}

	select {
	case code := <-stopDone:
		t.Fatalf("cmdStop returned early with code %d; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	case <-time.After(200 * time.Millisecond):
	}

	sp.release(stopped[0])

	select {
	case code := <-stopDone:
		if code != 0 {
			t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdStop did not finish after releasing controller shutdown")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not exit after cmdStop")
	}

	if pid := controllerAlive(dir); pid != 0 {
		t.Fatalf("controllerAlive after cmdStop = %d, want 0", pid)
	}
	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if stderr.String() != "" {
		t.Fatalf("unexpected stderr: %q", stderr.String())
	}
}

func TestCmdStopCompletionDeadlineRetainsEnteredStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	cityDir := shortSocketTempDir(t, "gc-stop-timeout-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "timeout-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		Agents: []config.Agent{
			{Name: "worker", StartCommand: "sleep 1"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newHangingProvider()
	sessionName := lookupSessionNameOrLegacy(nil, loadedCityName(cfg, cityDir), cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	oldFactory := sessionProviderForStopCity
	oldNow := stopCompletionNow
	oldPerTargetTimeout := stopPerTargetTimeoutDefault
	started := time.Date(2026, 7, 13, 7, 0, 0, 0, time.UTC)
	deadlineExpired := atomic.Bool{}
	stopCompletionNow = func() time.Time {
		if deadlineExpired.Load() {
			return started.Add(101 * time.Millisecond)
		}
		return started
	}
	stopPerTargetTimeoutDefault = 50 * time.Millisecond
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
		return sp, nil
	}
	t.Cleanup(func() {
		sp.release()
		sessionProviderForStopCity = oldFactory
		stopCompletionNow = oldNow
		stopPerTargetTimeoutDefault = oldPerTargetTimeout
	})

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{cityDir}, &stdout, &stderr, 100*time.Millisecond, false)
	}()
	select {
	case stopped := <-sp.stopIn:
		if stopped != sessionName {
			t.Fatalf("entered Stop(%q), want %q", stopped, sessionName)
		}
		deadlineExpired.Store(true)
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cmdStop never entered the provider Stop call")
	}
	select {
	case code := <-stopDone:
		t.Fatalf("cmdStop detached from an entered provider call with code %d", code)
	case <-time.After(200 * time.Millisecond):
		// The 100ms completion deadline is expired, but lifecycle ownership stays
		// joined until the already-entered native call returns.
	}
	contender, err := acquireControllerLock(cityDir)
	if contender != nil {
		_ = contender.Close()
	}
	if !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("concurrent starter acquired during entered Stop: %v", err)
	}
	sp.release()
	var code int
	select {
	case code = <-stopDone:
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cmdStop did not return after the entered provider call completed")
	}
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want deadline code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "completion deadline") {
		t.Fatalf("stderr = %q, want completion deadline message", stderr.String())
	}
	reacquired, err := acquireControllerLock(cityDir)
	if err != nil {
		t.Fatalf("stop lease not released after joined Stop returned: %v", err)
	}
	_ = reacquired.Close()
}

func TestCmdStopCompletionDeadlineRetainsEnteredInterrupt(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	cityDir := shortSocketTempDir(t, "gc-stop-interrupt-deadline-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "interrupt-deadline-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "50ms"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "sleep 1"}},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newHangingProvider()
	sessionName := lookupSessionNameOrLegacy(nil, loadedCityName(cfg, cityDir), cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	oldFactory := sessionProviderForStopCity
	oldNow := stopCompletionNow
	oldMargin := interruptPerTargetTimeoutMargin
	started := time.Date(2026, 7, 13, 7, 30, 0, 0, time.UTC)
	deadlineExpired := atomic.Bool{}
	stopCompletionNow = func() time.Time {
		if deadlineExpired.Load() {
			return started.Add(101 * time.Millisecond)
		}
		return started
	}
	interruptPerTargetTimeoutMargin = 0
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
	t.Cleanup(func() {
		sp.release()
		sessionProviderForStopCity = oldFactory
		stopCompletionNow = oldNow
		interruptPerTargetTimeoutMargin = oldMargin
	})

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() { stopDone <- cmdStop([]string{cityDir}, &stdout, &stderr, 100*time.Millisecond, false) }()
	select {
	case interrupted := <-sp.intIn:
		if interrupted != sessionName {
			t.Fatalf("entered Interrupt(%q), want %q", interrupted, sessionName)
		}
		deadlineExpired.Store(true)
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cmdStop never entered the provider Interrupt call")
	}
	select {
	case code := <-stopDone:
		t.Fatalf("cmdStop detached from an entered Interrupt with code %d", code)
	case <-time.After(100 * time.Millisecond):
		// Past both the fake outer deadline and the 50ms per-target cap.
	}
	contender, err := acquireControllerLock(cityDir)
	if contender != nil {
		_ = contender.Close()
	}
	if !errors.Is(err, errControllerAlreadyRunning) {
		t.Fatalf("concurrent starter acquired during entered Interrupt: %v", err)
	}
	sp.release()
	select {
	case code := <-stopDone:
		if code != 1 {
			t.Fatalf("cmdStop = %d, want deadline failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cmdStop did not return after the entered Interrupt completed")
	}
	if !strings.Contains(stderr.String(), "completion deadline") {
		t.Fatalf("stderr = %q, want completion deadline message", stderr.String())
	}
	reacquired, err := acquireControllerLock(cityDir)
	if err != nil {
		t.Fatalf("stop lease not released after joined Interrupt returned: %v", err)
	}
	_ = reacquired.Close()
}

func TestCmdStopRetainedInterruptTimeoutRemainsFailureBeforeOuterDeadline(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	cityDir := shortSocketTempDir(t, "gc-stop-interrupt-inner-timeout-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "interrupt-inner-timeout-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "20ms"},
		Agents:    []config.Agent{{Name: "worker", StartCommand: "sleep 1"}},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newHangingProvider()
	sessionName := lookupSessionNameOrLegacy(nil, loadedCityName(cfg, cityDir), cfg.Agents[0].QualifiedName(), cfg.Workspace.SessionTemplate)
	if err := sp.Start(context.Background(), sessionName, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	oldFactory := sessionProviderForStopCity
	oldMargin := interruptPerTargetTimeoutMargin
	interruptPerTargetTimeoutMargin = 0
	sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) { return sp, nil }
	t.Cleanup(func() {
		sp.release()
		sessionProviderForStopCity = oldFactory
		interruptPerTargetTimeoutMargin = oldMargin
	})

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() { stopDone <- cmdStop([]string{cityDir}, &stdout, &stderr, 10*time.Second, false) }()
	select {
	case interrupted := <-sp.intIn:
		if interrupted != sessionName {
			t.Fatalf("entered Interrupt(%q), want %q", interrupted, sessionName)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cmdStop never entered the provider Interrupt call")
	}

	// Let the 20ms per-target cap elapse while the outer ten-second command
	// budget remains live. The retained owner still joins the provider call.
	time.Sleep(60 * time.Millisecond)
	select {
	case code := <-stopDone:
		t.Fatalf("cmdStop detached from retained Interrupt with code %d", code)
	default:
	}
	sp.release()

	select {
	case code := <-stopDone:
		if code != 1 {
			t.Fatalf("cmdStop = %d, want sticky inner-timeout failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("cmdStop did not return after retained Interrupt completed")
	}
	if !strings.Contains(stderr.String(), "did not return within") {
		t.Fatalf("stderr = %q, want retained per-target timeout", stderr.String())
	}
	if strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout reported terminal success after retained Interrupt timeout: %q", stdout.String())
	}
}

func TestGracefulStopRetainedPoolStopTimeoutRemainsFailureBeforeOuterDeadline(t *testing.T) {
	oldStopTimeout := stopPerTargetTimeoutDefault
	stopPerTargetTimeoutDefault = 20 * time.Millisecond
	t.Cleanup(func() { stopPerTargetTimeoutDefault = oldStopTimeout })

	store := beads.NewMemStoreFrom(1, []beads.Bead{{
		ID:     "session-1",
		Type:   "session",
		Status: "open",
		Labels: []string{"gc:session"},
		Metadata: map[string]string{
			"session_name":         "pool-1",
			"state":                "active",
			poolManagedMetadataKey: boolMetadata(true),
		},
	}}, nil)
	sp := newHangingProvider()
	if err := sp.Start(context.Background(), "pool-1", runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(sp.release)
	started := time.Now()
	budget := newStopCompletionBudget(started, 10*time.Second, time.Now)

	stopDone := make(chan error, 1)
	go func() {
		stopDone <- gracefulStopAllWithOwnership(
			[]string{"pool-1"}, sp, 20*time.Millisecond, events.Discard, nil,
			beads.SessionStore{Store: store}, io.Discard, io.Discard, nil, true, &budget,
		)
	}()
	select {
	case stopped := <-sp.stopIn:
		if stopped != "pool-1" {
			t.Fatalf("entered Stop(%q), want pool-1", stopped)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("graceful stop never entered the pool-managed Stop call")
	}

	time.Sleep(60 * time.Millisecond)
	select {
	case err := <-stopDone:
		t.Fatalf("graceful stop detached from retained pool Stop: %v", err)
	default:
	}
	sp.release()

	select {
	case err := <-stopDone:
		if err == nil || !strings.Contains(err.Error(), "did not return within") {
			t.Fatalf("graceful stop error = %v, want sticky pool Stop timeout", err)
		}
	case <-time.After(testutil.GoroutineRaceTimeout):
		t.Fatal("graceful stop did not return after retained pool Stop completed")
	}
	if budget.expired() {
		t.Fatal("outer command budget expired; test must isolate the inner timeout")
	}
}

func TestCmdStopForceDelegatesImmediateControllerStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-force-stop-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "force-stop-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "250ms"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newRecordingStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)
	const sess = "force-stop-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 2*time.Second, true)
	}()

	select {
	case interrupted := <-sp.interrupts:
		t.Fatalf("gc stop --force delegated interrupt for %q; want immediate stop", interrupted)
	case stopped := <-sp.stops:
		if stopped != sess {
			t.Fatalf("stopped = %q, want %q", stopped, sess)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for delegated force stop")
	}

	select {
	case code := <-stopDone:
		if code != 0 {
			t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q controller stderr=%q", code, stdout.String(), stderr.String(), controllerStderr.String())
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cmdStop did not finish after delegated force stop")
	}
}

func TestCmdStopFailsClosedOnUncertainControllerStopBeforeSideEffects(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result controllerStopResult
	}{
		{
			name: "request may have entered",
			result: classifiedControllerStopResult(
				controllerStopMayHaveEntered,
				"test acknowledgement loss",
				errors.New("reply lost"),
			),
		},
		{
			name:   "invalid result",
			result: controllerStopResult{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			t.Setenv("GC_HOME", t.TempDir())
			t.Setenv("GC_BEADS", "file")
			t.Setenv("GC_BEADS_SCOPE_ROOT", "")

			cityDir := setupCity(t, "ambiguous-stop")
			writeRigAnywhereCityToml(t, cityDir, "[workspace]\nname = \"ambiguous-stop\"\n\n[beads]\nprovider = \"file\"\n")
			eventsPath := filepath.Join(cityDir, ".gc", "events.jsonl")
			if err := os.Remove(eventsPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}
			retiredSentinel := filepath.Join(cityDir, ".gc", "system", "packs", "retired", "keep")
			if err := os.MkdirAll(filepath.Dir(retiredSentinel), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(retiredSentinel, []byte("keep"), 0o600); err != nil {
				t.Fatal(err)
			}
			shimPath := gcBeadsBdScriptPath(cityDir)
			if err := os.Remove(shimPath); err != nil && !errors.Is(err, os.ErrNotExist) {
				t.Fatal(err)
			}

			requests := 0
			oldRequest := controllerStopRequestForCommand
			controllerStopRequestForCommand = func(path string, force bool) controllerStopResult {
				requests++
				assertSameTestPath(t, path, cityDir)
				if force {
					t.Fatal("ordinary stop requested force transport")
				}
				return tt.result
			}
			t.Cleanup(func() { controllerStopRequestForCommand = oldRequest })

			storeOpens := 0
			oldOpen := openCityStoreForStop
			openCityStoreForStop = func(string) (beads.Store, error) {
				storeOpens++
				return beads.NewMemStore(), nil
			}
			t.Cleanup(func() { openCityStoreForStop = oldOpen })

			providerConstructions := 0
			oldProvider := sessionProviderForStopCity
			sessionProviderForStopCity = func(*config.City, string) (runtime.Provider, error) {
				providerConstructions++
				return runtime.NewFake(), nil
			}
			t.Cleanup(func() { sessionProviderForStopCity = oldProvider })

			storeShutdowns := 0
			overrideShutdownBeadsProviderForStop(t, func(string) error {
				storeShutdowns++
				return nil
			})

			var stdout, stderr lockedBuffer
			code := cmdStop([]string{cityDir}, &stdout, &stderr, time.Second, false)

			if code != 1 {
				t.Fatalf("cmdStop() = %d, want fail-closed code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if requests != 1 {
				t.Fatalf("controller stop requests = %d, want 1", requests)
			}
			if storeOpens != 0 || providerConstructions != 0 || storeShutdowns != 0 {
				t.Fatalf("post-ambiguity effects = store opens:%d provider constructions:%d store shutdowns:%d, want all zero", storeOpens, providerConstructions, storeShutdowns)
			}
			if strings.Contains(stdout.String(), "City stopped") {
				t.Fatalf("stdout reports false success: %q", stdout.String())
			}
			if _, err := os.Stat(eventsPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("events file created after uncertain stop, stat error = %v", err)
			}
			if _, err := os.Stat(retiredSentinel); err != nil {
				t.Fatalf("pre-classification config cleanup removed retired-pack sentinel: %v", err)
			}
			if _, err := os.Stat(shimPath); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("pre-classification config load materialized bead shim, stat error = %v", err)
			}
			if !strings.Contains(stderr.String(), "controller stop") {
				t.Fatalf("stderr = %q, want controller-stop diagnostic", stderr.String())
			}
		})
	}
}

func TestCmdStopNoArgResolutionIsReadOnlyBeforeTransportClassification(t *testing.T) {
	resetFlags(t)
	t.Setenv("GC_HOME", t.TempDir())
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_RIG", "")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := setupCity(t, "no-arg-read-only")
	workingDir := filepath.Join(cityDir, "nested", "work")
	if err := os.MkdirAll(workingDir, 0o755); err != nil {
		t.Fatal(err)
	}
	oldCWD, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(workingDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(oldCWD) })

	retiredSentinel := filepath.Join(cityDir, ".gc", "system", "packs", "retired", "keep")
	if err := os.MkdirAll(filepath.Dir(retiredSentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retiredSentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	shimPath := gcBeadsBdScriptPath(cityDir)
	if err := os.Remove(shimPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	requests := 0
	oldRequest := controllerStopRequestForCommand
	controllerStopRequestForCommand = func(path string, force bool) controllerStopResult {
		requests++
		assertSameTestPath(t, path, cityDir)
		if force {
			t.Fatal("ordinary stop requested force transport")
		}
		return classifiedControllerStopResult(
			controllerStopMayHaveEntered,
			"test acknowledgement loss",
			errors.New("reply lost"),
		)
	}
	t.Cleanup(func() { controllerStopRequestForCommand = oldRequest })

	var stdout, stderr lockedBuffer
	code := cmdStop(nil, &stdout, &stderr, time.Second, false)

	if code != 1 {
		t.Fatalf("cmdStop(no args) = %d, want fail-closed code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if requests != 1 {
		t.Fatalf("controller stop requests = %d, want 1", requests)
	}
	if _, err := os.Stat(retiredSentinel); err != nil {
		t.Fatalf("no-arg resolution removed retired-pack sentinel before classification: %v", err)
	}
	if _, err := os.Stat(shimPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-arg resolution materialized bead shim before classification, stat error = %v", err)
	}
}

func TestCmdStopForceEscalatesInProgressControllerStop(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-force-escalate-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "force-escalate-city"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "5s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	tomlPath := filepath.Join(dir, "city.toml")
	if err := os.WriteFile(tomlPath, data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, tomlPath, cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		tryStopControllerWithForce(dir, io.Discard, true)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)
	const sess = "force-escalate-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	var normalStdout, normalStderr lockedBuffer
	normalDone := make(chan int, 1)
	go func() {
		normalDone <- cmdStop([]string{dir}, &normalStdout, &normalStderr, 10*time.Second, false)
	}()

	interrupted := sp.waitForInterrupts(t, 1)
	if interrupted[0] != sess {
		t.Fatalf("interrupted = %q, want %q", interrupted[0], sess)
	}

	var forceStdout, forceStderr lockedBuffer
	forceDone := make(chan int, 1)
	go func() {
		forceDone <- cmdStop([]string{dir}, &forceStdout, &forceStderr, 10*time.Second, true)
	}()

	stopped := sp.waitForStops(t, 1)
	if stopped[0] != sess {
		t.Fatalf("stopped = %q, want %q", stopped[0], sess)
	}
	sp.release(stopped[0])
	sp.releaseInterrupt(interrupted[0])

	for _, result := range []struct {
		name string
		ch   <-chan int
		out  *lockedBuffer
		err  *lockedBuffer
	}{
		{name: "normal stop", ch: normalDone, out: &normalStdout, err: &normalStderr},
		{name: "force stop", ch: forceDone, out: &forceStdout, err: &forceStderr},
	} {
		select {
		case code := <-result.ch:
			if code != 0 {
				t.Fatalf("%s code = %d, want 0; stdout=%q stderr=%q controller stderr=%q",
					result.name, code, result.out.String(), result.err.String(), controllerStderr.String())
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("%s did not finish after force escalation", result.name)
		}
	}
}

func TestCmdStopExplicitRegisteredRigPathUsesSharedResolver(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_BEADS", "")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := setupCity(t, "stop-registered-rig")
	rigDir := filepath.Join(t.TempDir(), "registered-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("[workspace]\nname = \"stop-registered-rig\"\n\n[beads]\nprovider = \"file\"\n\n[[agent]]\nname = \"worker\"\n\n[[rigs]]\nname = \"registered-rig\"\npath = %q\n", rigDir)
	writeRigAnywhereCityToml(t, cityDir, toml)
	registerCityForRigResolution(t, gcHome, cityDir, "stop-registered-rig")

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
	var gotCityPath string
	sessionProviderForStopCity = func(_ *config.City, cityPath string) (runtime.Provider, error) {
		gotCityPath = cityPath
		return runtime.NewFake(), nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{rigDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertSameTestPath(t, gotCityPath, cityDir)
}

func TestCmdStopRegisteredRigResolutionIsReadOnlyBeforeTransportClassification(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_CITY", "")
	t.Setenv("GC_CITY_PATH", "")
	t.Setenv("GC_CITY_ROOT", "")
	t.Setenv("GC_DIR", "")
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := setupCity(t, "stop-read-only-rig")
	rigDir := filepath.Join(t.TempDir(), "registered-rig")
	if err := os.MkdirAll(rigDir, 0o755); err != nil {
		t.Fatal(err)
	}
	toml := fmt.Sprintf("[workspace]\nname = \"stop-read-only-rig\"\n\n[beads]\nprovider = \"file\"\n\n[[rigs]]\nname = \"registered-rig\"\npath = %q\n", rigDir)
	writeRigAnywhereCityToml(t, cityDir, toml)
	registerCityForRigResolution(t, gcHome, cityDir, "stop-read-only-rig")

	retiredSentinel := filepath.Join(cityDir, ".gc", "system", "packs", "retired", "keep")
	if err := os.MkdirAll(filepath.Dir(retiredSentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(retiredSentinel, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	shimPath := gcBeadsBdScriptPath(cityDir)
	if err := os.Remove(shimPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 0 },
		func(string) (bool, string, bool) { return false, "", false },
		20*time.Millisecond,
		time.Millisecond,
	)
	requests := 0
	oldRequest := controllerStopRequestForCommand
	controllerStopRequestForCommand = func(path string, force bool) controllerStopResult {
		requests++
		assertSameTestPath(t, path, cityDir)
		if force {
			t.Fatal("ordinary stop requested force transport")
		}
		return classifiedControllerStopResult(
			controllerStopMayHaveEntered,
			"test acknowledgement loss",
			errors.New("reply lost"),
		)
	}
	t.Cleanup(func() { controllerStopRequestForCommand = oldRequest })

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{rigDir}, &stdout, &stderr, time.Second, false)

	if code != 1 {
		t.Fatalf("cmdStop(rig) = %d, want fail-closed code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if requests != 1 {
		t.Fatalf("controller stop requests = %d, want 1", requests)
	}
	if _, err := os.Stat(retiredSentinel); err != nil {
		t.Fatalf("rig resolution removed retired-pack sentinel before classification: %v", err)
	}
	if _, err := os.Stat(shimPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rig resolution materialized bead shim before classification, stat error = %v", err)
	}
}

func TestCmdStopExplicitCityPathIgnoresUnrelatedRegisteredCityLoadErrors(t *testing.T) {
	for _, tt := range []struct {
		name string
		arg  func(string) string
	}{
		{
			name: "city_root",
			arg:  func(cityDir string) string { return cityDir },
		},
		{
			name: "inside_city",
			arg: func(cityDir string) string {
				return filepath.Join(cityDir, "nested", "work")
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			clearInheritedBeadsEnv(t)
			gcHome := t.TempDir()
			t.Setenv("GC_HOME", gcHome)
			t.Setenv("GC_CITY", "")
			t.Setenv("GC_CITY_PATH", "")
			t.Setenv("GC_CITY_ROOT", "")
			t.Setenv("GC_DIR", "")
			t.Setenv("GC_BEADS_SCOPE_ROOT", "")

			cityDir := setupCity(t, "stop-explicit-city")
			writeRigAnywhereCityToml(t, cityDir, "[workspace]\nname = \"stop-explicit-city\"\n\n[beads]\nprovider = \"file\"\n\n[[agent]]\nname = \"worker\"\n")
			arg := tt.arg(cityDir)
			if err := os.MkdirAll(arg, 0o755); err != nil {
				t.Fatal(err)
			}

			badCity := setupCity(t, "broken-registered-city")
			registerCityForRigResolution(t, gcHome, badCity, "broken-registered-city")
			if err := os.WriteFile(filepath.Join(badCity, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o644); err != nil {
				t.Fatal(err)
			}

			withSupervisorTestHooks(
				t,
				func(_, _ io.Writer) int { return 0 },
				func(_, _ io.Writer) int { return 0 },
				func() int { return 0 },
				func(string) (bool, string, bool) { return false, "", false },
				20*time.Millisecond,
				time.Millisecond,
			)

			oldFactory := sessionProviderForStopCity
			t.Cleanup(func() { sessionProviderForStopCity = oldFactory })
			var gotCityPath string
			sessionProviderForStopCity = func(_ *config.City, cityPath string) (runtime.Provider, error) {
				gotCityPath = cityPath
				return runtime.NewFake(), nil
			}

			var stdout, stderr lockedBuffer
			code := cmdStop([]string{arg}, &stdout, &stderr, 0, false)
			if code != 0 {
				t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			assertSameTestPath(t, gotCityPath, cityDir)
			if strings.Contains(stderr.String(), "loading registered city rig bindings") {
				t.Fatalf("stderr = %q, want unrelated registered-city load error ignored for explicit city path", stderr.String())
			}
		})
	}
}

func TestCmdStopSupervisorManagedInvalidCityTomlWaitsForControllerStop(t *testing.T) {
	resetFlags(t)
	gcHome := t.TempDir()
	t.Setenv("GC_HOME", gcHome)
	t.Setenv("GC_BEADS", "file")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := filepath.Join(t.TempDir(), "invalid-supervisor-city")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	reg := registryAt(t, gcHome)
	if err := reg.Register(cityDir, "invalid-supervisor-city"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)
	var waitedPath string
	waitForSupervisorControllerStopHook = func(path string, _ time.Duration) error {
		waitedPath = path
		return nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, time.Second, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertSameTestPath(t, waitedPath, cityDir)
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("stderr = %q, want invalid config warning", stderr.String())
	}
}

func TestCmdStopSupervisorManagedInvalidCityTomlDoesNotRepeatManagedShutdown(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	gcHome := os.Getenv("GC_HOME")
	reg := registryAt(t, gcHome)
	if err := reg.Register(cityDir, "invalid-supervisor-city"); err != nil {
		t.Fatal(err)
	}

	withSupervisorTestHooks(
		t,
		func(_, _ io.Writer) int { return 0 },
		func(_, _ io.Writer) int { return 0 },
		func() int { return 4242 },
		func(string) (bool, string, bool) { return false, "", true },
		20*time.Millisecond,
		time.Millisecond,
	)
	waitForSupervisorControllerStopHook = func(string, time.Duration) error {
		return nil
	}
	shutdownCalls := 0
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdownCalls++
		assertSameTestPath(t, path, cityDir)
		return errors.New("CLI must not repeat managed provider shutdown")
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, time.Second, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if shutdownCalls != 0 {
		t.Fatalf("CLI-side provider shutdown calls = %d, want zero after managed cleanup", shutdownCalls)
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, want managed stop success", stdout.String())
	}
}

func TestCmdStopInvalidConfigManagedRuntimeStopsAfterVerifiedShutdown(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		return nil
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("stderr = %q, want invalid config warning", stderr.String())
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
}

func TestCmdStopInvalidConfigManagedRuntimeStopsStandaloneController(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	stopCommands := startAcknowledgingStandaloneController(t, cityDir)
	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		return nil
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	select {
	case cmd := <-stopCommands:
		if cmd != "stop" {
			t.Fatalf("controller command = %q, want stop", cmd)
		}
	case <-time.After(time.Second):
		t.Fatalf("controller did not receive stop command; stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
	if !strings.Contains(stderr.String(), "invalid config") {
		t.Fatalf("stderr = %q, want invalid config warning", stderr.String())
	}
}

func TestCmdStopInvalidConfigFailsClosedOnUncertainControllerStop(t *testing.T) {
	for _, tt := range []struct {
		name   string
		result controllerStopResult
	}{
		{
			name: "request may have entered",
			result: classifiedControllerStopResult(
				controllerStopMayHaveEntered,
				"test acknowledgement loss",
				errors.New("reply lost"),
			),
		},
		{
			name:   "invalid result",
			result: controllerStopResult{},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			resetFlags(t)
			cityDir := setupInvalidConfigManagedRuntime(t)

			requests := 0
			oldRequest := controllerStopRequestForCommand
			controllerStopRequestForCommand = func(path string, force bool) controllerStopResult {
				requests++
				assertSameTestPath(t, path, cityDir)
				if force {
					t.Fatal("ordinary invalid-config stop requested force transport")
				}
				return tt.result
			}
			t.Cleanup(func() { controllerStopRequestForCommand = oldRequest })

			storeShutdowns := 0
			overrideShutdownBeadsProviderForStop(t, func(string) error {
				storeShutdowns++
				return nil
			})

			var stdout, stderr lockedBuffer
			code := cmdStop([]string{cityDir}, &stdout, &stderr, time.Second, false)

			if code != 1 {
				t.Fatalf("cmdStop() = %d, want fail-closed code 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
			}
			if requests != 1 {
				t.Fatalf("controller stop requests = %d, want 1", requests)
			}
			if storeShutdowns != 0 {
				t.Fatalf("managed store shutdowns = %d, want 0", storeShutdowns)
			}
			if strings.Contains(stdout.String(), "City stopped") {
				t.Fatalf("stdout reports false success: %q", stdout.String())
			}
			if !strings.Contains(stderr.String(), "controller stop") {
				t.Fatalf("stderr = %q, want controller-stop diagnostic", stderr.String())
			}
		})
	}
}

func TestCmdStopInvalidConfigManagedRuntimeFailsWhenShutdownFails(t *testing.T) {
	resetFlags(t)
	cityDir := setupInvalidConfigManagedRuntime(t)
	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		return fmt.Errorf("provider-stop-failed")
	})

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 1 {
		t.Fatalf("cmdStop() = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, did not want success message", stdout.String())
	}
	if !strings.Contains(stderr.String(), "bead store") || !strings.Contains(stderr.String(), "provider-stop-failed") {
		t.Fatalf("stderr = %q, want bead-store shutdown error", stderr.String())
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
}

func TestStopCityManagedBeadsProviderUsesProviderStateWhenPublishedStateIsMissing(t *testing.T) {
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")

	cityDir := t.TempDir()
	_ = writeReachableProviderManagedDoltState(t, cityDir)

	var shutdowns int
	overrideShutdownBeadsProviderForStop(t, func(path string) error {
		shutdowns++
		assertSameTestPath(t, path, cityDir)
		return nil
	})

	stopped, err := stopCityManagedBeadsProviderWithHeldOwnership(cityDir)
	if err != nil {
		t.Fatalf("stopCityManagedBeadsProviderWithHeldOwnership() error = %v", err)
	}
	if !stopped {
		t.Fatal("stopCityManagedBeadsProviderWithHeldOwnership() stopped = false, want true")
	}
	if shutdowns != 1 {
		t.Fatalf("shutdown calls = %d, want 1", shutdowns)
	}
	if _, err := os.Stat(managedDoltStatePath(cityDir)); !os.IsNotExist(err) {
		t.Fatalf("stop detection should not publish runtime state, stat err = %v", err)
	}
}

func setupInvalidConfigManagedRuntime(t *testing.T) string {
	t.Helper()

	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))
	t.Setenv("GC_BEADS", "bd")
	t.Setenv("GC_BEADS_SCOPE_ROOT", "")
	t.Setenv("GC_DOLT", "")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), []byte("[workspace\nname = \"broken\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	state := doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	if err := writeDoltState(cityDir, state); err != nil {
		t.Fatalf("writeDoltState: %v", err)
	}

	return cityDir
}

func overrideShutdownBeadsProviderForStop(t *testing.T, fn func(string) error) {
	t.Helper()
	old := shutdownBeadsProviderForStop
	shutdownBeadsProviderForStop = fn
	t.Cleanup(func() { shutdownBeadsProviderForStop = old })
}

func startAcknowledgingStandaloneController(t *testing.T, cityDir string) <-chan string {
	t.Helper()

	lock, err := acquireControllerLock(cityDir)
	if err != nil {
		t.Fatalf("acquireControllerLock: %v", err)
	}
	sockPath := controllerSocketPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(sockPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sockPath); err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	lis, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen controller socket: %v", err)
	}
	commands := make(chan string, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		conn, err := lis.Accept()
		if err != nil {
			return
		}
		defer conn.Close() //nolint:errcheck // best-effort test cleanup
		buf := make([]byte, 64)
		n, err := conn.Read(buf)
		if err != nil {
			return
		}
		commands <- strings.TrimSpace(string(buf[:n]))
		_, _ = conn.Write([]byte("ok\n"))
		_ = lis.Close()
		_ = os.Remove(sockPath)
		_ = lock.Close()
	}()
	t.Cleanup(func() {
		_ = lis.Close()
		_ = os.Remove(sockPath)
		_ = lock.Close()
		select {
		case <-done:
		case <-time.After(time.Second):
		}
	})
	return commands
}

func TestDefaultStopWallClockTimeoutScalesWithConfiguredStopTargets(t *testing.T) {
	origStop := stopPerTargetTimeoutDefault
	origMargin := interruptPerTargetTimeoutMargin
	stopPerTargetTimeoutDefault = 10 * time.Second
	interruptPerTargetTimeoutMargin = time.Second
	t.Cleanup(func() {
		stopPerTargetTimeoutDefault = origStop
		interruptPerTargetTimeoutMargin = origMargin
	})

	cfg := &config.City{
		Daemon: config.DaemonConfig{ShutdownTimeout: "2s"},
	}
	for i := 0; i < 7; i++ {
		cfg.Agents = append(cfg.Agents, config.Agent{Name: fmt.Sprintf("worker-%d", i+1)})
	}

	got := defaultStopWallClockTimeout(cfg)
	// One stop pass budgets a 3s interrupt-dispatch cap, 2s graceful-exit
	// wait, and three 10s stop waves. The default cap allows two passes plus
	// one extra orphan-cleanup stop wave: 2*(3s+2s+30s)+10s.
	want := 80 * time.Second
	if got != want {
		t.Fatalf("defaultStopWallClockTimeout() = %s, want %s", got, want)
	}
}

func TestStopCityManagedBeadsProviderWithHeldOwnershipStopsDefaultBD(t *testing.T) {
	skipSlowCmdGCTest(t, "exercises managed bd provider shutdown; run make test-cmd-gc-process for full coverage")
	t.Setenv("GC_BEADS", "bd")

	cityDir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(cityDir, ".beads", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := gcBeadsBdScriptPath(cityDir)
	if err := os.MkdirAll(filepath.Dir(script), 0o755); err != nil {
		t.Fatal(err)
	}

	logFile := filepath.Join(t.TempDir(), "ops.log")
	if err := os.WriteFile(script, []byte("#!/bin/sh\necho \"$@\" >> \""+logFile+"\"\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close() //nolint:errcheck

	state := doltRuntimeState{
		Running:   true,
		PID:       os.Getpid(),
		Port:      ln.Addr().(*net.TCPAddr).Port,
		DataDir:   filepath.Join(cityDir, ".beads", "dolt"),
		StartedAt: time.Now().UTC().Format(time.RFC3339),
	}
	stateData, err := json.Marshal(state)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, ".gc", "runtime", "packs", "dolt", "dolt-state.json"), stateData, 0o644); err != nil {
		t.Fatal(err)
	}

	stopped, err := stopCityManagedBeadsProviderWithHeldOwnership(cityDir)
	if err != nil {
		t.Fatalf("stopCityManagedBeadsProviderWithHeldOwnership: %v", err)
	}
	if !stopped {
		t.Fatal("stopCityManagedBeadsProviderWithHeldOwnership stopped = false, want true")
	}
	ops := readOpLog(t, logFile)
	if len(ops) != 1 || ops[0] != "stop" {
		t.Fatalf("provider ops = %v, want [stop]", ops)
	}
}

func TestMarkCityStopSessionSleepReasonSkipsCreatingSessions(t *testing.T) {
	store := beads.NewMemStore()
	active, err := store.Create(beads.Bead{
		Title:  "active",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"state":        "active",
			"session_name": "active",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	creating, err := store.Create(beads.Bead{
		Title:  "creating",
		Type:   sessionBeadType,
		Labels: []string{sessionBeadLabel},
		Metadata: map[string]string{
			"state":                "creating",
			"session_name":         "creating",
			"pending_create_claim": "true",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	markCityStopSessionSleepReason(sessionFrontDoor(store), ioDiscard{})

	activeUpdated, err := store.Get(active.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := activeUpdated.Metadata["sleep_reason"]; got != string(sessionpkg.SleepReasonCityStop) {
		t.Fatalf("active sleep_reason = %q, want %q", got, string(sessionpkg.SleepReasonCityStop))
	}
	creatingUpdated, err := store.Get(creating.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got := creatingUpdated.Metadata["sleep_reason"]; got != "" {
		t.Fatalf("creating sleep_reason = %q, want empty because create rollback owns this state", got)
	}
}

func TestCmdStopUsesTargetCitySessionProviderOutsideCityDir(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	cityDir := shortSocketTempDir(t, "gc-stop-city-")
	if err := os.MkdirAll(filepath.Join(cityDir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{
		Workspace: config.Workspace{Name: "bright-lights"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Session:   config.SessionConfig{Provider: "subprocess"},
		Agents: []config.Agent{
			{Name: "mayor", StartCommand: "sleep 1"},
		},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cityDir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	otherDir := t.TempDir()
	cwd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(otherDir); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(cwd)
	})

	oldFactory := sessionProviderForStopCity
	t.Cleanup(func() { sessionProviderForStopCity = oldFactory })

	var gotPath, gotName, gotProvider string
	sessionProviderForStopCity = func(cfg *config.City, cityPath string) (runtime.Provider, error) {
		gotPath = cityPath
		if cfg != nil {
			gotName = cfg.Workspace.Name
			gotProvider = cfg.Session.Provider
		}
		return runtime.NewFake(), nil
	}

	var stdout, stderr lockedBuffer
	code := cmdStop([]string{cityDir}, &stdout, &stderr, 0, false)
	if code != 0 {
		t.Fatalf("cmdStop() = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	assertSameTestPath(t, gotPath, cityDir)
	if gotName != "bright-lights" {
		t.Fatalf("session provider cityName = %q, want %q", gotName, "bright-lights")
	}
	if gotProvider != "subprocess" {
		t.Fatalf("session provider provider = %q, want %q", gotProvider, "subprocess")
	}
}

// TestCmdStopMarginExhaustion verifies that cmdStop tolerates slow controller
// shutdowns without timing out. With a non-zero ShutdownTimeout and a provider
// whose Stop blocks briefly (simulating CI scheduling delays or an in-flight
// tick), the increased wait margin must absorb the overhead.
//
// Regression test for gastownhall/gascity#572.
func TestCmdStopMarginExhaustion(t *testing.T) {
	t.Setenv("GC_HOME", shortSocketTempDir(t, "gc-home-"))

	dir := shortSocketTempDir(t, "gc-margin-")
	if err := os.MkdirAll(filepath.Join(dir, ".gc"), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.City{
		Workspace: config.Workspace{Name: "test-margin"},
		Beads:     config.BeadsConfig{Provider: "file"},
		Daemon:    config.DaemonConfig{ShutdownTimeout: "1s"},
	}
	data, err := cfg.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	sp := newGatedStopProvider()
	buildFn := func(_ *config.City, _ runtime.Provider, _ beads.Store) DesiredStateResult {
		return DesiredStateResult{State: map[string]TemplateParams{}}
	}

	var controllerStdout, controllerStderr lockedBuffer
	done := make(chan struct{})
	go func() {
		runController(dir, filepath.Join(dir, "city.toml"), cfg, "", buildFn, nil, sp, nil, nil, nil, nil, events.Discard, nil, &controllerStdout, &controllerStderr)
		close(done)
	}()
	t.Cleanup(func() {
		running, _ := sp.ListRunning("")
		for _, name := range running {
			sp.release(name)
		}
		tryStopController(dir, &bytes.Buffer{})
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	})

	waitForControllerAvailable(t, dir)

	const sess = "margin-session"
	if err := sp.Start(context.Background(), sess, runtime.Config{}); err != nil {
		t.Fatal(err)
	}

	go func() {
		sp.waitForInterrupts(t, 1)
		sp.releaseInterrupt(sess)
	}()

	var stdout, stderr lockedBuffer
	stopDone := make(chan int, 1)
	go func() {
		stopDone <- cmdStop([]string{dir}, &stdout, &stderr, 0, false)
	}()

	stopped := sp.waitForStops(t, 1)
	if len(stopped) != 1 || stopped[0] != sess {
		t.Fatalf("stop targets = %v, want [%s]", stopped, sess)
	}

	time.AfterFunc(500*time.Millisecond, func() {
		sp.release(sess)
	})

	select {
	case code := <-stopDone:
		if code != 0 {
			t.Fatalf("cmdStop = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
	case <-time.After(20 * time.Second):
		t.Fatal("cmdStop did not finish within margin budget")
	}

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("controller did not exit after cmdStop")
	}

	if !strings.Contains(stdout.String(), "Controller stopping...") {
		t.Fatalf("stdout missing controller stop message: %q", stdout.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout missing city stopped message: %q", stdout.String())
	}
}

func waitForControllerAvailable(t *testing.T, dir string) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for {
		if controllerAcceptsPing(dir, 100*time.Millisecond) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("timed out waiting for controller socket to become available")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func controllerAcceptsPing(dir string, timeout time.Duration) bool {
	conn, err := net.DialTimeout("unix", controllerSocketPath(dir), timeout)
	if err != nil {
		return false
	}
	defer conn.Close() //nolint:errcheck // best-effort cleanup
	if err := conn.SetDeadline(time.Now().Add(timeout)); err != nil {
		return false
	}
	if _, err := conn.Write([]byte("ping\n")); err != nil {
		return false
	}
	buf := make([]byte, 64)
	n, err := conn.Read(buf)
	return err == nil && strings.TrimSpace(string(buf[:n])) != ""
}
