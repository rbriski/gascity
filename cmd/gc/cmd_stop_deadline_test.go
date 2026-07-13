package main

import (
	"context"
	"errors"
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
	"github.com/gastownhall/gascity/internal/formula"
	"github.com/gastownhall/gascity/internal/molecule"
	"github.com/gastownhall/gascity/internal/runtime"
	sessionpkg "github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/supervisor"
	"github.com/gastownhall/gascity/internal/testutil"
)

type trackingStopSupervisorRegistry struct {
	entries      []supervisor.CityEntry
	pendingID    string
	storeCalls   int
	consumeCalls int
}

func (r *trackingStopSupervisorRegistry) List() ([]supervisor.CityEntry, error) {
	return append([]supervisor.CityEntry(nil), r.entries...), nil
}

func (r *trackingStopSupervisorRegistry) Register(cityPath, effectiveName string) error {
	r.entries = append(r.entries, supervisor.CityEntry{Path: cityPath, Name: effectiveName})
	return nil
}

func (r *trackingStopSupervisorRegistry) Unregister(cityPath string) error {
	for i, entry := range r.entries {
		if samePath(entry.Path, cityPath) {
			r.entries = append(r.entries[:i], r.entries[i+1:]...)
			return nil
		}
	}
	return nil
}

func (r *trackingStopSupervisorRegistry) StorePendingCityRequestID(_ string, requestID string) error {
	r.storeCalls++
	if r.pendingID != "" {
		return supervisor.ErrPendingCityRequestExists
	}
	r.pendingID = requestID
	return nil
}

//nolint:unparam // error result matches the production pending-request method shape.
func (r *trackingStopSupervisorRegistry) ConsumePendingCityRequestID(_ string) (string, bool, error) {
	r.consumeCalls++
	if r.pendingID == "" {
		return "", false, nil
	}
	requestID := r.pendingID
	r.pendingID = ""
	return requestID, true, nil
}

func TestManagedStopUsesCityAbsenceAndExactLeaseWithoutPendingRequestMutation(t *testing.T) {
	tests := []struct {
		name      string
		pendingID string
	}{
		{name: "no pending API request"},
		{name: "pre-existing generic API request", pendingID: "req-api-unregister"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_HOME", t.TempDir())
			cityPath := setupCity(t, "managed-stop-no-witness")
			registry := &trackingStopSupervisorRegistry{
				entries:   []supervisor.CityEntry{{Path: cityPath, Name: "managed-stop-no-witness"}},
				pendingID: tt.pendingID,
			}
			oldRegistry := newSupervisorRegistry
			newSupervisorRegistry = func() supervisorRegistry { return registry }
			t.Cleanup(func() { newSupervisorRegistry = oldRegistry })

			started := time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC)
			deadline := started.Add(time.Second)
			var acquired *controllerLockLease
			oldOps := stopSupervisorUnregisterDeadlineOps
			stopSupervisorUnregisterDeadlineOps = supervisorUnregisterDeadlineOps{
				now:             func() time.Time { return started },
				supervisorAlive: func(time.Time) int { return 4242 },
				requestStop: func(string, bool, time.Time) controllerStopResult {
					t.Fatal("acknowledged managed stop issued an alternate controller request")
					return controllerStopResult{}
				},
				reload: func(io.Writer, io.Writer, time.Time) int { return 0 },
				waitCity: func(string, bool, time.Time, time.Duration, io.Writer) error {
					return nil
				},
				acquireOwnership: func(path string, _ time.Time, _ time.Duration) (*controllerLockLease, error) {
					lease, err := acquireControllerLock(path)
					acquired = lease
					return lease, err
				},
			}
			t.Cleanup(func() {
				stopSupervisorUnregisterDeadlineOps = oldOps
				if acquired != nil {
					_ = acquired.Close()
				}
			})

			result := unregisterCityFromSupervisorWithForceResultUntil(
				cityPath,
				io.Discard,
				io.Discard,
				"gc stop",
				false,
				deadline,
				time.Second,
			)

			if result.state != supervisorUnregisterManagedCleanupComplete {
				t.Fatalf("managed stop state = %v, want cleanup complete", result.state)
			}
			if acquired == nil || result.ownership != acquired {
				t.Fatalf("managed stop ownership = %p, want exact acquired lease %p", result.ownership, acquired)
			}
			if registry.storeCalls != 0 || registry.consumeCalls != 0 {
				t.Fatalf("pending request mutations = store:%d consume:%d, want zero", registry.storeCalls, registry.consumeCalls)
			}
			if registry.pendingID != tt.pendingID {
				t.Fatalf("pending API request = %q, want preserved %q", registry.pendingID, tt.pendingID)
			}
			if len(registry.entries) != 0 {
				t.Fatalf("registered cities after managed stop = %#v, want none", registry.entries)
			}
		})
	}
}

func TestManagedStopBespokeWitnessSymbolsStayRemoved(t *testing.T) {
	files := map[string][]string{
		"cmd_supervisor_city.go": {
			"supervisorPendingRequestRegistry",
			"stopSupervisorRequestIDPrefix",
			"newStopSupervisorRequestID",
			"waitForSupervisorUnregisterTerminalUntil",
			"waitTerminal",
			"StorePendingCityRequestID",
			"ConsumePendingCityRequestID",
			"ReadFilteredWithInFlight",
		},
		"cmd_supervisor.go": {
			"ListPendingCityRequests",
			"stopSupervisorRequestIDPrefix",
		},
		filepath.Join("..", "..", "internal", "supervisor", "registry.go"): {
			"ListPendingCityRequests",
		},
	}
	for path, forbidden := range files {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		for _, symbol := range forbidden {
			if strings.Contains(string(data), symbol) {
				t.Errorf("%s still contains bespoke stop witness symbol %q", path, symbol)
			}
		}
	}
}

func TestStopCompletionTimeoutAtEntryIsReadOnlyBeforeOwnership(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	gcHome := filepath.Join(t.TempDir(), "gc-home")
	t.Setenv("GC_HOME", gcHome)
	cityPath := writeCityWithLockedPublicGastownImport(t)
	data, err := os.ReadFile(filepath.Join(cityPath, "city.toml"))
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, []byte("\n[daemon]\nshutdown_timeout = \"4m\"\nformula_v2 = false\n")...)
	if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), data, 0o644); err != nil {
		t.Fatal(err)
	}

	formula.SetFormulaV2Enabled(true)
	molecule.SetGraphApplyEnabled(true)
	t.Cleanup(func() {
		formula.SetFormulaV2Enabled(false)
		molecule.SetGraphApplyEnabled(false)
	})
	owner, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = owner.Close() })

	if got := stopCompletionTimeoutAtEntry(cityPath, 0); got <= 4*time.Minute {
		t.Fatalf("derived completion timeout = %s, want shutdown timeout plus stop-wave budgets", got)
	}
	assertPublicGastownSyntheticCacheAbsent(t, gcHome)
	if !formula.IsFormulaV2Enabled() {
		t.Fatal("stop timeout read changed the process-global formula v2 flag")
	}
	if !molecule.IsGraphApplyEnabled() {
		t.Fatal("stop timeout read changed the process-global graph apply flag")
	}
}

type deadlineEffectProvider struct {
	*runtime.Fake
	beforeStop     func(string)
	afterStop      func(string)
	afterInterrupt func(string)
	afterIsRunning func(string)
	listErr        error
	forceRunning   *bool
}

func (p *deadlineEffectProvider) Stop(name string) error {
	if p.beforeStop != nil {
		p.beforeStop(name)
	}
	err := p.Fake.Stop(name)
	if p.afterStop != nil {
		p.afterStop(name)
	}
	return err
}

func (p *deadlineEffectProvider) Interrupt(name string) error {
	err := p.Fake.Interrupt(name)
	if p.afterInterrupt != nil {
		p.afterInterrupt(name)
	}
	return err
}

func (p *deadlineEffectProvider) IsRunning(name string) bool {
	running := p.Fake.IsRunning(name)
	if p.afterIsRunning != nil {
		p.afterIsRunning(name)
	}
	if p.forceRunning != nil {
		return *p.forceRunning
	}
	return running
}

func (p *deadlineEffectProvider) ListRunning(prefix string) ([]string, error) {
	names, err := p.Fake.ListRunning(prefix)
	if p.listErr != nil {
		return names, p.listErr
	}
	return names, err
}

type deadlineEffectStore struct {
	beads.Store
	listCalls             int
	getCalls              int
	setMetadataBatchCalls int
	afterList             func(int)
	afterGet              func(int)
}

func (s *deadlineEffectStore) List(query beads.ListQuery) ([]beads.Bead, error) {
	s.listCalls++
	rows, err := s.Store.List(query)
	if s.afterList != nil {
		s.afterList(s.listCalls)
	}
	return rows, err
}

func (s *deadlineEffectStore) Get(id string) (beads.Bead, error) {
	s.getCalls++
	bead, err := s.Store.Get(id)
	if s.afterGet != nil {
		s.afterGet(s.getCalls)
	}
	return bead, err
}

func (s *deadlineEffectStore) SetMetadataBatch(id string, values map[string]string) error {
	s.setMetadataBatchCalls++
	return s.Store.SetMetadataBatch(id, values)
}

type deadlineEffectRecorder struct{ calls atomic.Int32 }

func (r *deadlineEffectRecorder) Record(events.Event) { r.calls.Add(1) }

func countDeadlineProviderCalls(provider *runtime.Fake, method string) int {
	count := 0
	for _, call := range provider.SnapshotCalls() {
		if call.Method == method {
			count++
		}
	}
	return count
}

type deadlineCloseStore struct {
	*beads.MemStore
	closed  int
	onClose func()
}

//nolint:unparam // error return is required by the store close interface.
func (s *deadlineCloseStore) CloseStore() error {
	s.closed++
	if s.onClose != nil {
		s.onClose()
	}
	return nil
}

type lockWitnessStopProvider struct {
	*runtime.Fake
	onTeardown func()
}

func (p *lockWitnessStopProvider) ConfigureServer() error { return nil }

//nolint:unparam // error return is required by runtime.ServerLifecycle.
func (p *lockWitnessStopProvider) TeardownServer() error {
	if p.onTeardown != nil {
		p.onTeardown()
	}
	return nil
}

func TestCmdStopAcquiresOwnershipBeforeCleanupAndRetainsItThroughRelease(t *testing.T) {
	writeCity := func(t *testing.T, cityPath string) {
		t.Helper()
		cfg := &config.City{
			Workspace: config.Workspace{Name: "ownership-deadline-city"},
			Beads:     config.BeadsConfig{Provider: "file"},
			Daemon:    config.DaemonConfig{ShutdownTimeout: "0s"},
		}
		data, err := cfg.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(cityPath, "city.toml"), data, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("acknowledged starter wins and cleanup stays empty", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())
		cityPath := t.TempDir()
		writeCity(t, cityPath)
		started := time.Date(2026, 7, 13, 13, 0, 0, 0, time.UTC)
		oldNow := stopCompletionNow
		stopCompletionNow = func() time.Time { return started }
		t.Cleanup(func() { stopCompletionNow = oldNow })
		oldRequest := controllerStopRequestUntilForCommand
		controllerStopRequestUntilForCommand = func(string, bool, time.Time) controllerStopResult {
			return controllerStopResult{outcome: controllerStopAcknowledged}
		}
		t.Cleanup(func() { controllerStopRequestUntilForCommand = oldRequest })
		oldAcquire := acquireStopControllerUntilForCommand
		var gotDeadline time.Time
		acquireStopControllerUntilForCommand = func(_ string, _ controllerStopResult, deadline time.Time, _ time.Duration) (*controllerLockLease, error) {
			gotDeadline = deadline
			return nil, errControllerAlreadyRunning
		}
		t.Cleanup(func() { acquireStopControllerUntilForCommand = oldAcquire })

		openCalls := 0
		providerCalls := 0
		shutdownCalls := 0
		oldOpen := openCityStoreForStop
		oldProvider := sessionProviderForStopCity
		oldShutdown := shutdownBeadsProviderForStop
		openCityStoreForStop = func(string) (beads.Store, error) {
			openCalls++
			return beads.NewMemStore(), nil
		}
		sessionProviderForStopCity = func(*config.City, string) runtime.Provider {
			providerCalls++
			return runtime.NewFake()
		}
		shutdownBeadsProviderForStop = func(string) error { shutdownCalls++; return nil }
		t.Cleanup(func() {
			openCityStoreForStop = oldOpen
			sessionProviderForStopCity = oldProvider
			shutdownBeadsProviderForStop = oldShutdown
		})

		var stdout, stderr strings.Builder
		if code := cmdStopJSON([]string{cityPath}, &stdout, &stderr, time.Second, false, false); code != 1 {
			t.Fatalf("cmdStopJSON = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if gotDeadline != started.Add(time.Second) {
			t.Fatalf("ownership deadline = %v, want %v", gotDeadline, started.Add(time.Second))
		}
		if openCalls != 0 || providerCalls != 0 || shutdownCalls != 0 {
			t.Fatalf("cleanup after starter win = open:%d provider:%d shutdown:%d, want zero", openCalls, providerCalls, shutdownCalls)
		}
	})

	t.Run("stop wins and retains lease through every cleanup obligation", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())
		cityPath := t.TempDir()
		writeCity(t, cityPath)
		oldRequest := controllerStopRequestUntilForCommand
		controllerStopRequestUntilForCommand = func(path string, _ bool, _ time.Time) controllerStopResult {
			return controllerStopResult{
				outcome:    controllerStopDefinitePreEntryUnavailable,
				socketPath: controllerSocketPath(path),
			}
		}
		t.Cleanup(func() { controllerStopRequestUntilForCommand = oldRequest })
		oldAcquire := acquireStopControllerUntilForCommand
		acquireStopControllerUntilForCommand = func(path string, _ controllerStopResult, _ time.Time, _ time.Duration) (*controllerLockLease, error) {
			return acquireControllerLock(path)
		}
		t.Cleanup(func() { acquireStopControllerUntilForCommand = oldAcquire })

		assertLeaseHeld := func(stage string) {
			t.Helper()
			probe, err := acquireControllerLock(cityPath)
			if probe != nil {
				_ = probe.Close()
			}
			if !errors.Is(err, errControllerAlreadyRunning) {
				t.Fatalf("%s: concurrent starter acquire error = %v, want held stop lease", stage, err)
			}
		}
		provider := &lockWitnessStopProvider{Fake: runtime.NewFake(), onTeardown: func() { assertLeaseHeld("runtime teardown") }}
		store := &deadlineCloseStore{MemStore: beads.NewMemStore(), onClose: func() { assertLeaseHeld("store close") }}
		oldOpen := openCityStoreForStop
		oldProvider := sessionProviderForStopCity
		oldShutdown := shutdownBeadsProviderForStop
		oldCloseRecorder := closeEventRecorderForStop
		openCityStoreForStop = func(string) (beads.Store, error) { return store, nil }
		sessionProviderForStopCity = func(*config.City, string) runtime.Provider { return provider }
		shutdownBeadsProviderForStop = func(string) error { assertLeaseHeld("bead provider shutdown"); return nil }
		closeEventRecorderForStop = func(events.Recorder) error { assertLeaseHeld("event recorder close"); return nil }
		t.Cleanup(func() {
			openCityStoreForStop = oldOpen
			sessionProviderForStopCity = oldProvider
			shutdownBeadsProviderForStop = oldShutdown
			closeEventRecorderForStop = oldCloseRecorder
		})

		var stdout, stderr strings.Builder
		if code := cmdStopJSON([]string{cityPath}, &stdout, &stderr, 5*time.Second, true, false); code != 0 {
			t.Fatalf("cmdStopJSON = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if store.closed != 1 {
			t.Fatalf("store closes = %d, want one", store.closed)
		}
		probe, err := acquireControllerLock(cityPath)
		if err != nil {
			t.Fatalf("stop lease not released after cleanup: %v", err)
		}
		_ = probe.Close()
	})

	t.Run("ownership release failure suppresses terminal success", func(t *testing.T) {
		t.Setenv("GC_HOME", t.TempDir())
		cityPath := t.TempDir()
		writeCity(t, cityPath)
		oldRequest := controllerStopRequestUntilForCommand
		controllerStopRequestUntilForCommand = func(path string, _ bool, _ time.Time) controllerStopResult {
			return controllerStopResult{outcome: controllerStopDefinitePreEntryUnavailable, socketPath: controllerSocketPath(path)}
		}
		t.Cleanup(func() { controllerStopRequestUntilForCommand = oldRequest })
		oldAcquire := acquireStopControllerUntilForCommand
		acquireStopControllerUntilForCommand = func(path string, _ controllerStopResult, _ time.Time, _ time.Duration) (*controllerLockLease, error) {
			file, err := os.CreateTemp(t.TempDir(), "closed-stop-lease-")
			if err != nil {
				return nil, err
			}
			if err := file.Close(); err != nil {
				return nil, err
			}
			return &controllerLockLease{path: controllerLockPath(path), file: file}, nil
		}
		t.Cleanup(func() { acquireStopControllerUntilForCommand = oldAcquire })
		oldOpen := openCityStoreForStop
		oldProvider := sessionProviderForStopCity
		oldShutdown := shutdownBeadsProviderForStop
		openCityStoreForStop = func(string) (beads.Store, error) { return beads.NewMemStore(), nil }
		sessionProviderForStopCity = func(*config.City, string) runtime.Provider { return runtime.NewFake() }
		shutdownBeadsProviderForStop = func(string) error { return nil }
		t.Cleanup(func() {
			openCityStoreForStop = oldOpen
			sessionProviderForStopCity = oldProvider
			shutdownBeadsProviderForStop = oldShutdown
		})

		var stdout, stderr strings.Builder
		if code := cmdStopJSON([]string{cityPath}, &stdout, &stderr, 5*time.Second, true, false); code != 1 {
			t.Fatalf("cmdStopJSON = %d, want ownership-release failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if strings.Contains(stdout.String(), "City stopped.") {
			t.Fatalf("stdout claims success after ownership release failure: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), "releasing controller ownership") {
			t.Fatalf("stderr = %q, want ownership release error", stderr.String())
		}
	})
}

func TestCmdStopSupervisorRouteCarriesOriginalDeadlineAndReturnsOwnership(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := setupCity(t, "managed-deadline")
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "managed-deadline"); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 7, 13, 15, 0, 0, 0, time.UTC)
	deadline := started.Add(time.Second)
	oldNow := stopCompletionNow
	stopCompletionNow = func() time.Time { return started }
	t.Cleanup(func() { stopCompletionNow = oldNow })

	var observedDeadlines []time.Time
	oldOps := stopSupervisorUnregisterDeadlineOps
	stopSupervisorUnregisterDeadlineOps = supervisorUnregisterDeadlineOps{
		now: func() time.Time { return started },
		supervisorAlive: func(got time.Time) int {
			observedDeadlines = append(observedDeadlines, got)
			return 4242
		},
		requestStop: func(string, bool, time.Time) controllerStopResult {
			t.Fatal("managed ordinary stop issued an alternate controller request")
			return controllerStopResult{}
		},
		reload: func(_ io.Writer, _ io.Writer, got time.Time) int {
			observedDeadlines = append(observedDeadlines, got)
			entries, err := reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 0 {
				t.Fatalf("reload observed registration before unregister: %v", entries)
			}
			return 0
		},
		waitCity: func(_ string, wantRunning bool, got time.Time, timeoutLabel time.Duration, _ io.Writer) error {
			observedDeadlines = append(observedDeadlines, got)
			if wantRunning {
				t.Fatal("managed stop waited for running state")
			}
			if timeoutLabel != time.Second {
				t.Fatalf("city wait timeout label = %s, want 1s", timeoutLabel)
			}
			return nil
		},
		acquireOwnership: func(path string, got time.Time, timeoutLabel time.Duration) (*controllerLockLease, error) {
			observedDeadlines = append(observedDeadlines, got)
			if timeoutLabel != time.Second {
				t.Fatalf("ownership timeout label = %s, want 1s", timeoutLabel)
			}
			return acquireControllerLock(path)
		},
	}
	t.Cleanup(func() { stopSupervisorUnregisterDeadlineOps = oldOps })

	oldOpen := openCityStoreForStop
	oldProvider := sessionProviderForStopCity
	oldShutdown := shutdownBeadsProviderForStop
	openCityStoreForStop = func(string) (beads.Store, error) {
		t.Fatal("managed stop opened a direct store")
		return nil, nil
	}
	sessionProviderForStopCity = func(*config.City, string) runtime.Provider {
		t.Fatal("managed stop constructed a direct provider")
		return nil
	}
	shutdownBeadsProviderForStop = func(string) error {
		t.Fatal("CLI repeated supervisor-owned provider shutdown")
		return nil
	}
	t.Cleanup(func() {
		openCityStoreForStop = oldOpen
		sessionProviderForStopCity = oldProvider
		shutdownBeadsProviderForStop = oldShutdown
	})

	var stdout, stderr strings.Builder
	if code := cmdStopJSON([]string{cityPath}, &stdout, &stderr, time.Second, false, false); code != 0 {
		t.Fatalf("cmdStopJSON = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if len(observedDeadlines) != 4 {
		t.Fatalf("deadline-bearing stages = %d, want 4", len(observedDeadlines))
	}
	for i, got := range observedDeadlines {
		if got != deadline {
			t.Fatalf("stage %d deadline = %v, want original %v", i, got, deadline)
		}
	}
	probe, err := acquireControllerLock(cityPath)
	if err != nil {
		t.Fatalf("returned ownership was not released before terminal return: %v", err)
	}
	_ = probe.Close()
}

func TestCmdStopRegisteredMissingCityNeedsNoCleanupOwnership(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := filepath.Join(t.TempDir(), "gone-city")
	if err := os.MkdirAll(cityPath, 0o755); err != nil {
		t.Fatal(err)
	}
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "gone-city"); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(cityPath); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 7, 13, 15, 30, 0, 0, time.UTC)
	oldNow := stopCompletionNow
	stopCompletionNow = func() time.Time { return started }
	t.Cleanup(func() { stopCompletionNow = oldNow })

	oldOps := stopSupervisorUnregisterDeadlineOps
	stopSupervisorUnregisterDeadlineOps = supervisorUnregisterDeadlineOps{
		now:             func() time.Time { return started },
		supervisorAlive: func(time.Time) int { return 42 },
		requestStop: func(string, bool, time.Time) controllerStopResult {
			t.Fatal("standalone stop request issued while supervisor is alive")
			return controllerStopResult{}
		},
		reload: func(io.Writer, io.Writer, time.Time) int { return 0 },
		waitCity: func(string, bool, time.Time, time.Duration, io.Writer) error {
			t.Fatal("city-status wait issued for an absent city directory")
			return nil
		},
		acquireOwnership: func(string, time.Time, time.Duration) (*controllerLockLease, error) {
			t.Fatal("controller ownership requested for an absent city directory")
			return nil, nil
		},
	}
	t.Cleanup(func() { stopSupervisorUnregisterDeadlineOps = oldOps })

	var stdout, stderr strings.Builder
	if code := cmdStopJSON([]string{"gone-city"}, &stdout, &stderr, time.Second, false, false); code != 0 {
		t.Fatalf("cmdStopJSON = %d, want success for already-absent city; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if !strings.Contains(stdout.String(), "City stopped.") {
		t.Fatalf("stdout = %q, want terminal success", stdout.String())
	}
	if strings.Contains(stderr.String(), "without returning controller ownership") {
		t.Fatalf("stderr reports impossible ownership requirement: %q", stderr.String())
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("registry entries = %v, want absent city unregistered", entries)
	}
}

func TestCmdStopSupervisorRouteDeadlineAfterLivenessDoesNotUnregister(t *testing.T) {
	t.Setenv("GC_HOME", t.TempDir())
	cityPath := setupCity(t, "managed-liveness-deadline")
	reg := supervisor.NewRegistry(supervisor.RegistryPath())
	if err := reg.Register(cityPath, "managed-liveness-deadline"); err != nil {
		t.Fatal(err)
	}

	started := time.Date(2026, 7, 13, 15, 30, 0, 0, time.UTC)
	deadline := started.Add(time.Second)
	now := started
	oldNow := stopCompletionNow
	stopCompletionNow = func() time.Time { return now }
	t.Cleanup(func() { stopCompletionNow = oldNow })

	oldOps := stopSupervisorUnregisterDeadlineOps
	stopSupervisorUnregisterDeadlineOps = supervisorUnregisterDeadlineOps{
		now: func() time.Time { return now },
		supervisorAlive: func(got time.Time) int {
			if got != deadline {
				t.Fatalf("liveness deadline = %v, want %v", got, deadline)
			}
			now = deadline
			return 4242
		},
		requestStop: func(string, bool, time.Time) controllerStopResult {
			t.Fatal("deadline-expired path issued controller stop")
			return controllerStopResult{}
		},
		reload: func(io.Writer, io.Writer, time.Time) int {
			t.Fatal("deadline-expired path reloaded supervisor")
			return 1
		},
		waitCity: func(string, bool, time.Time, time.Duration, io.Writer) error {
			t.Fatal("deadline-expired path waited for city")
			return nil
		},
		acquireOwnership: func(string, time.Time, time.Duration) (*controllerLockLease, error) {
			t.Fatal("deadline-expired path acquired ownership")
			return nil, nil
		},
	}
	t.Cleanup(func() { stopSupervisorUnregisterDeadlineOps = oldOps })

	var stdout, stderr strings.Builder
	if code := cmdStopJSON([]string{cityPath}, &stdout, &stderr, time.Second, false, false); code != 1 {
		t.Fatalf("cmdStopJSON = %d, want deadline failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	entries, err := reg.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || !samePath(entries[0].Path, cityPath) {
		t.Fatalf("registration changed after liveness exhausted deadline: %v", entries)
	}
}

func TestSupervisorUnregisterDeadlineStopsBeforeEveryNextPhase(t *testing.T) {
	tests := []struct {
		name       string
		force      bool
		alive      bool
		expireAt   string
		wantStages []string
	}{
		{name: "liveness", alive: true, expireAt: "alive", wantStages: []string{"alive"}},
		{name: "standalone stop request", expireAt: "request", wantStages: []string{"alive", "request"}},
		{name: "managed force request", force: true, alive: true, expireAt: "request", wantStages: []string{"alive", "request"}},
		{name: "supervisor reload", alive: true, expireAt: "reload", wantStages: []string{"alive", "reload"}},
		{name: "city status wait", alive: true, expireAt: "wait-city", wantStages: []string{"alive", "reload", "wait-city"}},
		{name: "ownership return", alive: true, expireAt: "ownership", wantStages: []string{"alive", "reload", "wait-city", "ownership"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("GC_HOME", t.TempDir())
			cityPath := setupCity(t, "unregister-deadline-"+strings.ReplaceAll(tt.expireAt, "-", ""))
			reg := supervisor.NewRegistry(supervisor.RegistryPath())
			if err := reg.Register(cityPath, filepath.Base(cityPath)); err != nil {
				t.Fatal(err)
			}

			started := time.Date(2026, 7, 13, 15, 45, 0, 0, time.UTC)
			deadline := started.Add(time.Second)
			now := started
			var stages []string
			record := func(stage string) {
				stages = append(stages, stage)
				if stage == tt.expireAt {
					now = deadline
				}
			}

			oldOps := stopSupervisorUnregisterDeadlineOps
			stopSupervisorUnregisterDeadlineOps = supervisorUnregisterDeadlineOps{
				now: func() time.Time { return now },
				supervisorAlive: func(got time.Time) int {
					if got != deadline {
						t.Fatalf("liveness deadline = %v, want %v", got, deadline)
					}
					record("alive")
					if tt.alive {
						return 4242
					}
					return 0
				},
				requestStop: func(path string, force bool, got time.Time) controllerStopResult {
					if !samePath(path, cityPath) || force != tt.force || got != deadline {
						t.Fatalf("request = (%q, force=%t, %v), want (%q, force=%t, %v)", path, force, got, cityPath, tt.force, deadline)
					}
					record("request")
					if tt.alive {
						return controllerStopResult{outcome: controllerStopAcknowledged}
					}
					return controllerStopResult{
						outcome:    controllerStopDefinitePreEntryUnavailable,
						socketPath: controllerSocketPath(path),
					}
				},
				reload: func(_ io.Writer, _ io.Writer, got time.Time) int {
					if got != deadline {
						t.Fatalf("reload deadline = %v, want %v", got, deadline)
					}
					record("reload")
					return 0
				},
				waitCity: func(_ string, _ bool, got time.Time, timeoutLabel time.Duration, _ io.Writer) error {
					if got != deadline || timeoutLabel != time.Second {
						t.Fatalf("city wait deadline/label = %v/%s, want %v/1s", got, timeoutLabel, deadline)
					}
					record("wait-city")
					return nil
				},
				acquireOwnership: func(path string, got time.Time, timeoutLabel time.Duration) (*controllerLockLease, error) {
					if got != deadline || timeoutLabel != time.Second {
						t.Fatalf("ownership deadline/label = %v/%s, want %v/1s", got, timeoutLabel, deadline)
					}
					record("ownership")
					return acquireControllerLock(path)
				},
			}
			t.Cleanup(func() { stopSupervisorUnregisterDeadlineOps = oldOps })

			var stdout, stderr strings.Builder
			result := unregisterCityFromSupervisorWithForceResultUntil(
				cityPath, &stdout, &stderr, "gc stop", tt.force, deadline, time.Second,
			)
			if result.state != supervisorUnregisterFailed || result.ownership != nil {
				t.Fatalf("unregister result = %+v, want failed without ownership", result)
			}
			if strings.Join(stages, ",") != strings.Join(tt.wantStages, ",") {
				t.Fatalf("stages = %v, want %v", stages, tt.wantStages)
			}
			entries, err := reg.List()
			if err != nil {
				t.Fatal(err)
			}
			if len(entries) != 1 || !samePath(entries[0].Path, cityPath) {
				t.Fatalf("registration was not preserved/restored: %v", entries)
			}
			probe, err := acquireControllerLock(cityPath)
			if err != nil {
				t.Fatalf("deadline phase leaked controller ownership: %v", err)
			}
			_ = probe.Close()
		})
	}
}

func TestSupervisorReloadCarriesOriginalDeadlineAcrossDialWriteAndRead(t *testing.T) {
	started := time.Date(2026, 7, 13, 16, 0, 0, 0, time.UTC)
	deadline := started.Add(75 * time.Millisecond)
	conn := &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("ok\n")}}}
	var gotNetwork, gotAddress string
	var gotDialTimeout time.Duration
	dial := func(network, address string, timeout time.Duration) (net.Conn, error) {
		gotNetwork, gotAddress, gotDialTimeout = network, address, timeout
		return conn, nil
	}

	var stdout, stderr strings.Builder
	code := reloadSupervisorAtPathUntil("/tmp/test-supervisor.sock", &stdout, &stderr, deadline, func() time.Time { return started }, dial)
	if code != 0 {
		t.Fatalf("reload = %d, want 0; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if gotNetwork != "unix" || gotAddress != "/tmp/test-supervisor.sock" || gotDialTimeout != 75*time.Millisecond {
		t.Fatalf("dial = (%q, %q, %s), want unix path with 75ms remaining", gotNetwork, gotAddress, gotDialTimeout)
	}
	if got := conn.writes.String(); got != "reload\n" {
		t.Fatalf("reload command = %q, want exact command", got)
	}
	if len(conn.writeDeadlineValues) != 1 || conn.writeDeadlineValues[0] != deadline {
		t.Fatalf("write deadlines = %v, want original %v", conn.writeDeadlineValues, deadline)
	}
	if len(conn.readDeadlineValues) != 1 || conn.readDeadlineValues[0] != deadline {
		t.Fatalf("read deadlines = %v, want original %v", conn.readDeadlineValues, deadline)
	}
}

func TestSupervisorReloadDoesNotWriteAfterDialConsumesDeadline(t *testing.T) {
	started := time.Date(2026, 7, 13, 16, 30, 0, 0, time.UTC)
	deadline := started.Add(75 * time.Millisecond)
	now := started
	conn := &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("ok\n")}}}
	dial := func(string, string, time.Duration) (net.Conn, error) {
		now = deadline
		return conn, nil
	}

	var stdout, stderr strings.Builder
	code := reloadSupervisorAtPathUntil("/tmp/test-supervisor.sock", &stdout, &stderr, deadline, func() time.Time { return now }, dial)
	if code != 1 {
		t.Fatalf("reload = %d, want deadline failure; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
	}
	if conn.writes.Len() != 0 || conn.writeDeadlines != 0 || conn.readDeadlines != 0 {
		t.Fatalf("post-deadline wire effects = writes:%q write deadlines:%d read deadlines:%d, want none", conn.writes.String(), conn.writeDeadlines, conn.readDeadlines)
	}
}

func TestSupervisorAliveProbeCarriesOriginalDeadlineAcrossDialWriteAndRead(t *testing.T) {
	started := time.Date(2026, 7, 13, 17, 0, 0, 0, time.UTC)
	deadline := started.Add(75 * time.Millisecond)
	conn := &scriptedStopConn{reads: []scriptedStopRead{{data: []byte("4242\n")}}}
	var gotDialTimeout time.Duration
	dial := func(network, address string, timeout time.Duration) (net.Conn, error) {
		if network != "unix" || address != "/tmp/test-supervisor.sock" {
			t.Fatalf("dial = (%q, %q), want exact supervisor socket", network, address)
		}
		gotDialTimeout = timeout
		return conn, nil
	}

	pid := supervisorAliveAtPathUntilWithDial("/tmp/test-supervisor.sock", deadline, func() time.Time { return started }, dial)
	if pid != 4242 {
		t.Fatalf("supervisor PID = %d, want 4242", pid)
	}
	if gotDialTimeout != 75*time.Millisecond {
		t.Fatalf("dial timeout = %s, want remaining 75ms", gotDialTimeout)
	}
	if got := conn.writes.String(); got != "ping\n" {
		t.Fatalf("probe command = %q, want exact ping", got)
	}
	if len(conn.writeDeadlineValues) != 1 || conn.writeDeadlineValues[0] != deadline {
		t.Fatalf("write deadlines = %v, want original %v", conn.writeDeadlineValues, deadline)
	}
	if len(conn.readDeadlineValues) != 1 || conn.readDeadlineValues[0] != deadline {
		t.Fatalf("read deadlines = %v, want original %v", conn.readDeadlineValues, deadline)
	}
}

func TestCmdStopDirectResourceAdmissionAndCleanupAreFailClosed(t *testing.T) {
	cfg := config.DefaultCity("deadline-resource")
	result := classifiedControllerStopResult(controllerStopDefinitePreEntryUnavailable, "test", errControllerStopDefinitePreEntryUnavailable)

	t.Run("store open error prevents provider construction", func(t *testing.T) {
		wantErr := errors.New("store unavailable")
		oldOpen := openCityStoreForStop
		oldProvider := sessionProviderForStopCity
		oldShutdown := shutdownBeadsProviderForStop
		providerCalls := 0
		shutdownCalls := 0
		openCityStoreForStop = func(string) (beads.Store, error) { return nil, wantErr }
		sessionProviderForStopCity = func(*config.City, string) runtime.Provider {
			providerCalls++
			return runtime.NewFake()
		}
		shutdownBeadsProviderForStop = func(string) error { shutdownCalls++; return nil }
		t.Cleanup(func() {
			openCityStoreForStop = oldOpen
			sessionProviderForStopCity = oldProvider
			shutdownBeadsProviderForStop = oldShutdown
		})

		var stdout, stderr strings.Builder
		if code := cmdStopBodyWithHeldOwnership(t.TempDir(), &cfg, true, result, &stdout, &stderr, nil); code != 1 {
			t.Fatalf("stop body = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if providerCalls != 0 || shutdownCalls != 0 {
			t.Fatalf("post-open effects = provider:%d shutdown:%d, want none", providerCalls, shutdownCalls)
		}
		if !strings.Contains(stderr.String(), wantErr.Error()) {
			t.Fatalf("stderr = %q, want store-open error", stderr.String())
		}
	})

	t.Run("opened store closes and shutdown failure suppresses success", func(t *testing.T) {
		store := &deadlineCloseStore{MemStore: beads.NewMemStore()}
		wantErr := errors.New("shutdown failed")
		oldOpen := openCityStoreForStop
		oldProvider := sessionProviderForStopCity
		oldShutdown := shutdownBeadsProviderForStop
		openCityStoreForStop = func(string) (beads.Store, error) { return store, nil }
		sessionProviderForStopCity = func(*config.City, string) runtime.Provider { return runtime.NewFake() }
		shutdownBeadsProviderForStop = func(string) error { return wantErr }
		t.Cleanup(func() {
			openCityStoreForStop = oldOpen
			sessionProviderForStopCity = oldProvider
			shutdownBeadsProviderForStop = oldShutdown
		})

		var stdout, stderr strings.Builder
		if code := cmdStopBodyWithHeldOwnership(t.TempDir(), &cfg, true, result, &stdout, &stderr, nil); code != 1 {
			t.Fatalf("stop body = %d, want 1; stdout=%q stderr=%q", code, stdout.String(), stderr.String())
		}
		if store.closed != 1 {
			t.Fatalf("store close calls = %d, want one", store.closed)
		}
		if strings.Contains(stdout.String(), "City stopped") {
			t.Fatalf("stdout reports success after required shutdown failure: %q", stdout.String())
		}
		if !strings.Contains(stderr.String(), wantErr.Error()) {
			t.Fatalf("stderr = %q, want shutdown failure", stderr.String())
		}
	})
}

func TestStopCompletionDeadlineAdmitsEachExistingNativeEffect(t *testing.T) {
	const completionTimeout = 100 * time.Millisecond
	started := time.Date(2026, 7, 13, 6, 0, 0, 0, time.UTC)

	markedStore := func(t *testing.T, sessionName string) *deadlineEffectStore {
		t.Helper()
		corpus := []beads.Bead{{
			ID:     "session-1",
			Type:   "session",
			Status: "open",
			Labels: []string{"gc:session"},
			Metadata: map[string]string{
				"session_name": sessionName,
				"state":        "active",
				"sleep_reason": string(sessionpkg.SleepReasonCityStop),
			},
		}}
		return &deadlineEffectStore{Store: beads.NewMemStoreFrom(len(corpus), corpus, nil)}
	}
	unmarkedStore := func(t *testing.T, sessionName string) *deadlineEffectStore {
		t.Helper()
		corpus := []beads.Bead{{
			ID:     "session-1",
			Type:   "session",
			Status: "open",
			Labels: []string{"gc:session"},
			Metadata: map[string]string{
				"session_name": sessionName,
				"state":        "active",
			},
		}}
		return &deadlineEffectStore{Store: beads.NewMemStoreFrom(len(corpus), corpus, nil)}
	}

	t.Run("resolution expiry denies stop before native entry", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := unmarkedStore(t, "worker")
		store.afterGet = func(call int) {
			if call == 1 {
				now = started.Add(completionTimeout + time.Millisecond)
			}
		}
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}

		err := stopTargetThroughWorkerBoundaryWithBudget(
			stopTarget{name: "session-1"}, store, provider, nil, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("stop error = %v, want completion deadline", err)
		}
		if provider.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("native Stop entries = %d, want zero", provider.CountCalls("Stop", "worker"))
		}
	})

	t.Run("resolution expiry denies marked pool kill before native entry", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := markedStore(t, "pool-1")
		store.afterGet = func(call int) {
			if call == 2 {
				now = started.Add(completionTimeout + time.Millisecond)
			}
		}
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "pool-1", runtime.Config{}); err != nil {
			t.Fatal(err)
		}

		err := stopTargetThroughWorkerBoundaryWithBudget(
			stopTarget{sessionID: "session-1", name: "pool-1", poolManaged: true}, store, provider, nil, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("kill error = %v, want completion deadline", err)
		}
		if provider.CountCalls("Stop", "pool-1") != 0 {
			t.Fatalf("native pool Kill/Stop entries = %d, want zero", provider.CountCalls("Stop", "pool-1"))
		}
	})

	t.Run("resolution expiry denies interrupt before native entry", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := unmarkedStore(t, "worker")
		store.afterGet = func(call int) {
			if call == 1 {
				now = started.Add(completionTimeout + time.Millisecond)
			}
		}
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}

		sent, _ := interruptTargetsBoundedRetainingEnteredWithBudget(
			[]stopTarget{{sessionID: "session-1", name: "worker", resolved: true}}, nil, store, provider, io.Discard, &budget,
		)
		if sent != 0 {
			t.Fatalf("interrupts reported sent = %d, want zero", sent)
		}
		if provider.CountCalls("Interrupt", "worker") != 0 {
			t.Fatalf("native Interrupt entries = %d, want zero", provider.CountCalls("Interrupt", "worker"))
		}
	})

	t.Run("native stop admission rechecks after liveness probe", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := unmarkedStore(t, "worker")
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		provider.afterIsRunning = func(string) {
			now = started.Add(completionTimeout + time.Millisecond)
		}

		err := stopTargetThroughWorkerBoundaryWithBudget(
			stopTarget{name: "session-1"}, store, provider, nil, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("stop error = %v, want completion deadline", err)
		}
		if provider.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("native Stop entries = %d, want zero", provider.CountCalls("Stop", "worker"))
		}
	})

	t.Run("late native stop cannot begin post-provider state write", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := unmarkedStore(t, "worker")
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		provider.afterStop = func(string) {
			now = started.Add(completionTimeout + time.Millisecond)
		}

		err := stopTargetThroughWorkerBoundaryWithBudget(
			stopTarget{name: "session-1"}, store, provider, nil, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("stop error = %v, want completion deadline", err)
		}
		if provider.CountCalls("Stop", "worker") != 1 {
			t.Fatalf("native Stop entries = %d, want one joined call", provider.CountCalls("Stop", "worker"))
		}
		row, getErr := store.Store.Get("session-1")
		if getErr != nil {
			t.Fatal(getErr)
		}
		if row.Metadata["state"] != "active" {
			t.Fatalf("post-deadline state = %q, want active", row.Metadata["state"])
		}
	})

	t.Run("native interrupt admission rechecks after liveness probe", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := unmarkedStore(t, "worker")
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		provider.afterIsRunning = func(string) {
			now = started.Add(completionTimeout + time.Millisecond)
		}

		sent, _ := interruptTargetsBoundedRetainingEnteredWithBudget(
			[]stopTarget{{sessionID: "session-1", name: "worker", resolved: true}}, nil, store, provider, io.Discard, &budget,
		)
		if sent != 0 {
			t.Fatalf("interrupts reported sent = %d, want zero", sent)
		}
		if provider.CountCalls("Interrupt", "worker") != 0 {
			t.Fatalf("native Interrupt entries = %d, want zero", provider.CountCalls("Interrupt", "worker"))
		}
	})

	t.Run("late stop blocks event and later target", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		for _, name := range []string{"first", "second"} {
			if err := provider.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		provider.afterStop = func(name string) {
			if name == "first" {
				now = started.Add(completionTimeout + time.Millisecond)
			}
		}
		recorder := &deadlineEffectRecorder{}

		_, err := stopTargetsBoundedWithOwnershipAndBudget(
			[]stopTarget{{name: "first"}, {name: "second"}}, nil, nil, provider,
			recorder, "gc", io.Discard, io.Discard, true, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("stop error = %v, want completion deadline", err)
		}
		if provider.CountCalls("Stop", "first") != 1 || provider.CountCalls("Stop", "second") != 0 {
			t.Fatalf("Stop calls = first:%d second:%d, want 1 then 0", provider.CountCalls("Stop", "first"), provider.CountCalls("Stop", "second"))
		}
		if recorder.calls.Load() != 0 {
			t.Fatalf("events after late Stop = %d, want none", recorder.calls.Load())
		}
	})

	t.Run("late marker read blocks kill marker and event", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := markedStore(t, "worker")
		store.afterGet = func(call int) {
			if call == 1 {
				now = started.Add(completionTimeout + time.Millisecond)
			}
		}
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		recorder := &deadlineEffectRecorder{}

		_, err := stopTargetsBoundedWithOwnershipAndBudget(
			[]stopTarget{{sessionID: "session-1", name: "worker"}}, nil, store, provider,
			recorder, "gc", io.Discard, io.Discard, true, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("stop error = %v, want completion deadline", err)
		}
		if store.getCalls != 1 || provider.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("post-marker effects = Get:%d Stop:%d, want 1 and 0", store.getCalls, provider.CountCalls("Stop", "worker"))
		}
		if store.setMetadataBatchCalls != 0 || recorder.calls.Load() != 0 {
			t.Fatalf("post-marker writes/events = %d/%d, want none", store.setMetadataBatchCalls, recorder.calls.Load())
		}
	})

	t.Run("late kill blocks asleep marker and event", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := markedStore(t, "worker")
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		provider.afterStop = func(string) { now = started.Add(completionTimeout + time.Millisecond) }
		recorder := &deadlineEffectRecorder{}

		_, err := stopTargetsBoundedWithOwnershipAndBudget(
			[]stopTarget{{sessionID: "session-1", name: "worker"}}, nil, store, provider,
			recorder, "gc", io.Discard, io.Discard, true, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("stop error = %v, want completion deadline", err)
		}
		if provider.CountCalls("Stop", "worker") != 1 || store.setMetadataBatchCalls != 0 || recorder.calls.Load() != 0 {
			t.Fatalf("effects = Stop:%d marker:%d events:%d, want 1/0/0", provider.CountCalls("Stop", "worker"), store.setMetadataBatchCalls, recorder.calls.Load())
		}
	})

	t.Run("pool-managed path shares admission", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := markedStore(t, "pool-1")
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		for _, name := range []string{"pool-1", "later"} {
			if err := provider.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		provider.afterStop = func(string) { now = started.Add(completionTimeout + time.Millisecond) }

		_, _ = interruptTargetsBoundedRetainingEnteredWithBudget(
			[]stopTarget{
				{sessionID: "session-1", name: "pool-1", poolManaged: true},
				{name: "later"},
			}, nil, store, provider, io.Discard, &budget,
		)
		if provider.CountCalls("Stop", "pool-1") != 1 {
			t.Fatalf("pool-managed Stop calls = %d, want one", provider.CountCalls("Stop", "pool-1"))
		}
		if provider.CountCalls("Interrupt", "later") != 0 || provider.CountCalls("Stop", "later") != 0 {
			t.Fatalf("later provider calls = Interrupt:%d Stop:%d, want none", provider.CountCalls("Interrupt", "later"), provider.CountCalls("Stop", "later"))
		}
		if store.setMetadataBatchCalls != 0 {
			t.Fatalf("asleep marker writes after late pool Stop = %d, want none", store.setMetadataBatchCalls)
		}
	})

	t.Run("grace fallback checks each target", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		forceRunning := false
		provider := &deadlineEffectProvider{Fake: runtime.NewFake(), listErr: errors.New("list unavailable"), forceRunning: &forceRunning}
		for _, name := range []string{"first", "second"} {
			if err := provider.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
		}
		provider.afterIsRunning = func(string) { now = started.Add(completionTimeout + time.Millisecond) }

		err := gracefulStopAllWithOwnership(
			[]string{"first", "second"}, provider, time.Second, events.Discard, nil,
			beads.SessionStore{}, io.Discard, io.Discard, nil, true, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("graceful stop error = %v, want completion deadline", err)
		}
		if got := countDeadlineProviderCalls(provider.Fake, "IsRunning"); got != 1 {
			t.Fatalf("grace fallback probes = %d, want one", got)
		}
	})

	t.Run("pass two late probe blocks stop marker and event", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		forceRunning := false
		provider := &deadlineEffectProvider{Fake: runtime.NewFake(), listErr: errors.New("list unavailable"), forceRunning: &forceRunning}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}
		provider.afterIsRunning = func(string) { now = started.Add(completionTimeout + time.Millisecond) }
		forceChecks := 0
		forceStop := func() bool {
			forceChecks++
			return forceChecks >= 2
		}
		recorder := &deadlineEffectRecorder{}

		err := gracefulStopAllWithOwnership(
			[]string{"worker"}, provider, time.Second, recorder, nil,
			beads.SessionStore{}, io.Discard, io.Discard, forceStop, true, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("graceful stop error = %v, want completion deadline", err)
		}
		if provider.CountCalls("Stop", "worker") != 0 || recorder.calls.Load() != 0 {
			t.Fatalf("post-probe effects = Stop:%d events:%d, want none", provider.CountCalls("Stop", "worker"), recorder.calls.Load())
		}
	})

	t.Run("hydration blocks second query and provider", func(t *testing.T) {
		now := started
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time { return now })
		store := &deadlineEffectStore{Store: beads.NewMemStore()}
		store.afterList = func(call int) {
			if call == 1 {
				now = started.Add(completionTimeout + time.Millisecond)
			}
		}
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		if err := provider.Start(context.Background(), "worker", runtime.Config{}); err != nil {
			t.Fatal(err)
		}

		_, err := stopTargetsBoundedWithOwnershipAndBudget(
			[]stopTarget{{name: "worker"}}, nil, store, provider, events.Discard,
			"gc", io.Discard, io.Discard, true, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("stop error = %v, want completion deadline", err)
		}
		if store.listCalls != 1 || provider.CountCalls("Stop", "worker") != 0 {
			t.Fatalf("effects = List:%d Stop:%d, want 1/0", store.listCalls, provider.CountCalls("Stop", "worker"))
		}
	})

	t.Run("parallel queue denies unentered target", func(t *testing.T) {
		expired := atomic.Bool{}
		budget := newStopCompletionBudget(started, completionTimeout, func() time.Time {
			if expired.Load() {
				return started.Add(completionTimeout + time.Millisecond)
			}
			return started
		})
		provider := &deadlineEffectProvider{Fake: runtime.NewFake()}
		names := []string{"worker-1", "worker-2", "worker-3", "worker-4"}
		cfg := config.DefaultCity("parallel-deadline")
		cfg.Agents = make([]config.Agent, 0, len(names))
		targets := make([]stopTarget, 0, len(names))
		for _, name := range names {
			if err := provider.Start(context.Background(), name, runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			cfg.Agents = append(cfg.Agents, config.Agent{Name: name})
			targets = append(targets, stopTarget{name: name, template: name, resolved: true})
		}
		entered := atomic.Int32{}
		release := make(chan struct{})
		provider.beforeStop = func(string) {
			if entered.Add(1) == int32(defaultMaxParallelStopsPerWave) {
				expired.Store(true)
				close(release)
			}
			select {
			case <-release:
			case <-time.After(testutil.GoroutineRaceTimeout):
				t.Error("parallel wave did not fill admitted slots")
			}
		}
		recorder := &deadlineEffectRecorder{}

		_, err := stopTargetsBoundedWithOwnershipAndBudget(
			targets, &cfg, nil, provider, recorder, "gc", io.Discard, io.Discard, true, &budget,
		)
		if !errors.Is(err, errStopCompletionDeadline) {
			t.Fatalf("parallel stop error = %v, want completion deadline", err)
		}
		if got := countDeadlineProviderCalls(provider.Fake, "Stop"); got != defaultMaxParallelStopsPerWave {
			t.Fatalf("parallel Stop calls = %d, want %d", got, defaultMaxParallelStopsPerWave)
		}
		if recorder.calls.Load() != 0 {
			t.Fatalf("parallel events after expiry = %d, want none", recorder.calls.Load())
		}
	})
}
